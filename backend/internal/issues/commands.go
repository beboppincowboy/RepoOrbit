// Package issues runs user-initiated GitHub issue writes (create, label/assignee add/remove, close/reopen) as audited
// operations. Every write is recorded before it is sent, uses only the actor's own GitHub token, rechecks live access
// and permission, and reports what GitHub actually did, never what was assumed. An ambiguous create is never resent.
package issues

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/repoorbit/repoorbit/backend/internal/accessfence"
	"github.com/repoorbit/repoorbit/backend/internal/auth"
	"github.com/repoorbit/repoorbit/backend/internal/db"
	"github.com/repoorbit/repoorbit/backend/internal/githubapp"
	"github.com/repoorbit/repoorbit/backend/internal/ingestion"
	"github.com/repoorbit/repoorbit/backend/internal/repositories"
)

const (
	maxTitle = 256   // GitHub's title limit, in characters
	maxBody  = 65536 // GitHub's body limit, in characters
	maxItems = 10    // labels or assignees per list; GitHub allows at most 10 assignees
	maxLabel = 50    // GitHub's label name limit

	HeaderWriteDisposition           = "RepoOrbit-Write-Disposition"
	WriteDispositionNotRecorded      = "not-recorded"
	WriteDispositionPossiblyRecorded = "possibly-recorded"
)

type submitWriter struct {
	http.ResponseWriter
	disposition string
}

func newSubmitWriter(w http.ResponseWriter) *submitWriter {
	return &submitWriter{ResponseWriter: w, disposition: WriteDispositionNotRecorded}
}

func (sw *submitWriter) WriteHeader(code int) {
	if code >= http.StatusBadRequest && sw.disposition != "" {
		if code != http.StatusInternalServerError || sw.disposition == WriteDispositionPossiblyRecorded {
			sw.Header().Set(HeaderWriteDisposition, sw.disposition)
		}
	}
	sw.ResponseWriter.WriteHeader(code)
}

var (
	loginPattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})$`)
	keyPattern   = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,128}$`)
)

// Service serves the issue write API. Mount Register's routes behind auth.Handler.Protect (Origin + CSRF checks).
type Service struct {
	Pool    *pgxpool.Pool
	Queries *db.Queries
	GitHub  *githubapp.Client
	// Access decides live repository access and permissions; its TokenSource supplies the actor's own user token.
	// Installation credentials are never used for these writes.
	Access    *repositories.Service
	GitHubURL string // browser-facing GitHub origin without a trailing slash (platform.Config.GitHubURL), for an unknown create's searchUrl
}

// Register adds the issue write routes to mux; docs/api.md ("REST: issue writes") gives each route's body, status
// codes and errors. A repository {id} is its GitHub ID; an operation {id} is the operation's id. Behind
// auth.Handler.Protect a request without a session gets 401 there; a demo viewer gets 404 here, and a 401 from here
// means the user's GitHub token is gone. Errors are plain text, except a duplicate create's 409 ({"error",
// "operationId"}), an update's 409 when its expect no longer matches GitHub ({"error", "current"}) and a recheck's 409
// (the unchanged operation).
func (s *Service) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/repositories/{id}/issue-capabilities", s.capabilities)
	mux.HandleFunc("GET /api/repositories/{id}/assignees", s.assignees)
	mux.HandleFunc("POST /api/repositories/{id}/issues", s.create)
	mux.HandleFunc("POST /api/repositories/{id}/issues/{number}/updates", s.update)
	mux.HandleFunc("GET /api/repositories/{id}/write-operations", s.list)
	mux.HandleFunc("GET /api/write-operations/{id}", s.get)
	mux.HandleFunc("POST /api/write-operations/{id}/recheck", s.recheck)
	mux.HandleFunc("GET /api/repositories/{id}/status-labels", s.statusLabelState)
	mux.HandleFunc("POST /api/repositories/{id}/status-labels", s.enableStatusLabels)
	mux.HandleFunc("DELETE /api/repositories/{id}/status-labels", s.disableStatusLabels)
}

type createDraft struct {
	Title     string   `json:"title"`
	Body      string   `json:"body"`
	Labels    []string `json:"labels"`
	Assignees []string `json:"assignees"`
}

type delta struct {
	Add    []string `json:"add"`
	Remove []string `json:"remove"`
}

type updateDraft struct {
	Labels    delta        `json:"labels"`
	Assignees delta        `json:"assignees"`
	State     string       `json:"state"`
	Expect    *expectation `json:"expect,omitempty"`
}

type expectation struct {
	Status       string   `json:"status"`
	Assignees    []string `json:"assignees"`
	StatusLabels []string `json:"statusLabels"`
}

type issueView struct {
	Number    int64    `json:"number"`
	URL       string   `json:"url"`
	Title     string   `json:"title"`
	State     string   `json:"state"`
	Labels    []string `json:"labels"`
	Assignees []string `json:"assignees"`
}

// outcome is what GitHub actually did. Unapplied lists requested changes the confirmed issue does not show, each as
// "add label X", "remove label X", "add assignee X", "remove assignee X" or "state open|closed" (or one fixed
// sentence when a wiped create's labels and assignees cannot be verified); for a status_labels operation, "create
// label X" for each status label the repository still lacks. AllStepsAnswered (updates only) means every
// step got a definite answer (2xx, 4xx, or failed before it was sent), so nothing of the update can still land and a
// recheck may fail it. Once the draft is wiped the stored outcome is redacted instead, by SQL redacted_write_outcome, to
// {issue: {number, url, state}, unappliedCount, error, redacted: true}; clients must accept both shapes.
type outcome struct {
	Issue            *issueView `json:"issue,omitempty"`
	Unapplied        []string   `json:"unapplied,omitempty"`
	Created          []string   `json:"created,omitempty"`
	Error            string     `json:"error,omitempty"`
	AllStepsAnswered bool       `json:"allStepsAnswered,omitempty"`
}

type opView struct {
	ID           int64           `json:"id"`
	RepositoryID int64           `json:"repositoryId"`        // the repository's GitHub ID
	Kind         string          `json:"kind"`                // "create", "update", "status_labels" or, for the app's own writes, "cleanup"
	State        string          `json:"state"`               // the operation's state: "pending", "unknown", "succeeded" or "failed"
	IssueNumber  *int64          `json:"issueNumber"`         // an update's or cleanup's target; a create's issue once confirmed, else null
	Draft        json.RawMessage `json:"draft"`               // null once wiped after the actor lost access
	Outcome      json.RawMessage `json:"outcome"`             // an outcome, full or redacted
	Actor        string          `json:"actor"`               // the initiating user's GitHub login; "" for the app's own writes (kind "cleanup")
	SearchURL    string          `json:"searchUrl,omitempty"` // unknown creates only: a GitHub issue search for the actor's issue with the draft title
	CreatedAt    time.Time       `json:"createdAt"`
	UpdatedAt    time.Time       `json:"updatedAt"`
}

func decodeStrict(w http.ResponseWriter, r *http.Request, out any) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return errors.New("invalid JSON body")
	}
	if dec.Decode(new(json.RawMessage)) != io.EOF {
		return errors.New("unexpected data after the JSON body")
	}
	return nil
}

func names(list []string, what string, valid func(string) bool) ([]string, error) {
	if len(list) > maxItems {
		return nil, fmt.Errorf("at most %d %ss", maxItems, what)
	}
	for i, v := range list {
		if !valid(v) {
			return nil, fmt.Errorf("invalid %s %q", what, v)
		}
		if slices.IndexFunc(list[:i], func(o string) bool { return strings.EqualFold(o, v) }) >= 0 {
			return nil, fmt.Errorf("duplicate %s %q", what, v)
		}
	}
	if len(list) == 0 {
		return nil, nil
	}
	return list, nil
}

func validLabel(v string) bool {
	return strings.TrimSpace(v) == v && v != "" && utf8.RuneCountInString(v) <= maxLabel
}
func validLogin(v string) bool { return loginPattern.MatchString(v) }

func (d *createDraft) validate() (err error) {
	d.Title = strings.TrimSpace(d.Title)
	if d.Title == "" || utf8.RuneCountInString(d.Title) > maxTitle {
		return fmt.Errorf("title must be 1 to %d characters", maxTitle)
	}
	if utf8.RuneCountInString(d.Body) > maxBody {
		return fmt.Errorf("body must be at most %d characters", maxBody)
	}
	if d.Labels, err = names(d.Labels, "label", validLabel); err != nil {
		return err
	}
	d.Assignees, err = names(d.Assignees, "assignee", validLogin)
	return err
}

func (d *updateDraft) validate() (err error) {
	for _, l := range []*[]string{&d.Labels.Add, &d.Labels.Remove} {
		if *l, err = names(*l, "label", validLabel); err != nil {
			return err
		}
	}
	for _, l := range []*[]string{&d.Assignees.Add, &d.Assignees.Remove} {
		if *l, err = names(*l, "assignee", validLogin); err != nil {
			return err
		}
	}
	for _, x := range []delta{d.Labels, d.Assignees} {
		for _, v := range x.Add {
			if slices.ContainsFunc(x.Remove, func(o string) bool { return strings.EqualFold(o, v) }) {
				return fmt.Errorf("%q is both added and removed", v)
			}
		}
	}
	if d.State != "" && d.State != "open" && d.State != "closed" {
		return errors.New(`state must be "open" or "closed"`)
	}
	if len(d.Labels.Add)+len(d.Labels.Remove)+len(d.Assignees.Add)+len(d.Assignees.Remove) == 0 && d.State == "" {
		return errors.New("no change requested")
	}
	if e := d.Expect; e != nil {
		if !slices.Contains(boardStates, e.Status) {
			return fmt.Errorf("expect.status must be one of %s", strings.Join(boardStates, ", "))
		}
		if e.Assignees, err = names(e.Assignees, "expected assignee", validLogin); err != nil {
			return err
		}
		e.StatusLabels, err = names(e.StatusLabels, "expected status label", func(v string) bool { return statusRank(v) > 0 })
		return err
	}
	return nil
}

func (e *expectation) matches(issue *issueView) bool {
	sameSet := func(a, b []string) bool {
		within := func(x, y []string) bool { return !slices.ContainsFunc(x, func(v string) bool { return !has(y, v) }) }
		return within(a, b) && within(b, a)
	}
	if boardStatus(issue.State, issue.Labels) != e.Status || !sameSet(issue.Assignees, e.Assignees) {
		return false
	}
	live := slices.DeleteFunc(slices.Clone(issue.Labels), func(l string) bool { return statusRank(l) == 0 })
	return issue.State == "closed" || sameSet(live, e.StatusLabels)
}

func contentHash(title, body string) []byte {
	norm := func(s string) string { return strings.ReplaceAll(strings.TrimSpace(s), "\r\n", "\n") }
	sum := sha256.Sum256([]byte(norm(title) + "\x00" + norm(body)))
	return sum[:]
}

// Per-action rules follow GitHub: anyone who can read may open a plain issue; labels or assignees on a new issue need
// push (GitHub silently drops them otherwise); changing labels or assignees later and closing or reopening need
// triage. Archived repositories and repositories without issues accept no writes.
func canPush(r githubapp.Repo) bool { p := r.Permissions; return p.Push || p.Maintain || p.Admin }
func canTriage(r githubapp.Repo) bool {
	return r.Permissions.Triage || canPush(r)
}
func writable(r githubapp.Repo) bool  { return r.HasIssues && !r.Archived }
func canCreate(r githubapp.Repo) bool { return writable(r) && (r.Permissions.Pull || canTriage(r)) }
func canCreateWithMetadata(r githubapp.Repo) bool {
	return canCreate(r) && canPush(r)
}
func canManage(r githubapp.Repo) bool {
	return writable(r) && canTriage(r)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func pathID(r *http.Request, name string) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue(name), 10, 64)
	return id, err == nil && id > 0
}

func (s *Service) access(w http.ResponseWriter, r *http.Request, userID, repoID int64, fresh bool) (repositories.Decision, bool) {
	private, err := s.Queries.GetRepositoryIsPrivate(r.Context(), repoID)
	if errors.Is(err, pgx.ErrNoRows) {
		http.Error(w, "not found", http.StatusNotFound)
		return repositories.Decision{}, false
	}
	if err != nil {
		s.internal(w, err)
		return repositories.Decision{}, false
	}
	d := s.Access.CheckOne(r.Context(), userID, repoID, fresh)
	switch {
	case d.Unverified:
		if !d.RetryAt.IsZero() {
			w.Header().Set("Retry-After", strconv.Itoa(int(time.Until(d.RetryAt).Seconds())+1))
		}
		http.Error(w, "access could not be verified; retry later", http.StatusServiceUnavailable)
		return d, false
	case !d.Allowed:
		if private {
			if err := s.Queries.WipeActorDrafts(r.Context(), db.WipeActorDraftsParams{UserID: userID, RepositoryID: repoID}); err != nil {
				log.Printf("issues: wipe drafts: %v", err)
			}
		}
		http.Error(w, "not found", http.StatusNotFound)
		return d, false
	}
	return d, true
}

func (s *Service) checkedAccess(w http.ResponseWriter, r *http.Request, userID, repoID int64) (repositories.Decision, accessfence.Snapshot, bool) {
	for attempt := 0; ; attempt++ {
		snap, err := accessfence.Take(r.Context(), s.Pool, userID, repoID)
		if err != nil {
			s.internal(w, err)
			return repositories.Decision{}, snap, false
		}
		d, ok := s.access(w, r, userID, repoID, true)
		if !ok {
			return d, snap, false
		}
		current, err := snap.Current(r.Context(), s.Pool)
		switch {
		case err != nil:
			s.internal(w, err)
			return d, snap, false
		case current:
			return d, snap, true
		case attempt == 1:
			http.Error(w, "access changed while it was being checked; retry", http.StatusServiceUnavailable)
			return d, snap, false
		}
	}
}

func (s *Service) fenceHolds(w http.ResponseWriter, ctx context.Context, snap accessfence.Snapshot) bool {
	current, err := snap.Current(ctx, s.Pool)
	if err != nil {
		s.internal(w, err)
		return false
	}
	if !current {
		http.Error(w, "access changed while loading; retry", http.StatusServiceUnavailable)
	}
	return current
}

func (s *Service) internal(w http.ResponseWriter, err error) {
	log.Printf("issues: %v", err)
	http.Error(w, "temporarily unavailable", http.StatusInternalServerError)
}

func (s *Service) token(w http.ResponseWriter, ctx context.Context, userID int64) (string, bool) {
	token, err := s.Access.TokenSource.AccessToken(ctx, userID)
	switch {
	case errors.Is(err, auth.ErrUnauthenticated):
		http.Error(w, "sign in required", http.StatusUnauthorized)
	case err != nil:
		http.Error(w, "GitHub credential unavailable; retry later", http.StatusServiceUnavailable)
	default:
		return token, true
	}
	return "", false
}

func (s *Service) upstream(w http.ResponseWriter, ctx context.Context, userID int64, err error) {
	if s.revokeIfUnauthorized(ctx, userID, err) {
		http.Error(w, "sign in required", http.StatusUnauthorized)
		return
	}
	http.Error(w, "GitHub could not be reached; nothing was sent", http.StatusServiceUnavailable)
}

func (s *Service) revokeIfUnauthorized(ctx context.Context, userID int64, err error) bool {
	var api *githubapp.APIError
	if !errors.As(err, &api) || !api.Unauthorized() {
		return false
	}
	if err := s.Access.TokenSource.Revoke(ctx, userID); err != nil {
		log.Printf("issues: revoke failed: %v", err)
	}
	return true
}

func (s *Service) respond(w http.ResponseWriter, ctx context.Context, status int, userID, opID int64, snap accessfence.Snapshot) {
	row, err := s.Queries.GetWriteOperation(ctx, db.GetWriteOperationParams{ID: opID, UserID: userID})
	if err != nil {
		w.Header().Set(HeaderWriteDisposition, WriteDispositionPossiblyRecorded)
		s.internal(w, err)
		return
	}
	current, err := snap.Current(ctx, s.Pool)
	if err != nil {
		w.Header().Set(HeaderWriteDisposition, WriteDispositionPossiblyRecorded)
		s.internal(w, err)
		return
	}
	if !current {
		w.Header().Set(HeaderWriteDisposition, WriteDispositionPossiblyRecorded)
		http.Error(w, "access changed; the operation was recorded and may have been sent: retry with the same Idempotency-Key", http.StatusServiceUnavailable)
		return
	}
	if status == http.StatusCreated && row.State == "unknown" {
		status = http.StatusAccepted
	}
	writeJSON(w, status, s.view(row))
}

type command struct {
	kind        string
	issueNumber int64
	draft       any
	contentHash []byte
	hasMetadata bool
	assignees   []string
	allowed     func(githubapp.Repo) bool
	authorMay   bool
}

func (s *Service) create(w http.ResponseWriter, r *http.Request) {
	sw := newSubmitWriter(w)
	var d createDraft
	if err := decodeStrict(sw, r, &d); err != nil {
		http.Error(sw, err.Error(), http.StatusBadRequest)
		return
	}
	if err := d.validate(); err != nil {
		http.Error(sw, err.Error(), http.StatusBadRequest)
		return
	}
	allowed := canCreate
	if len(d.Labels)+len(d.Assignees) > 0 {
		allowed = canCreateWithMetadata
	}
	s.submit(sw, r, command{kind: "create", draft: &d, contentHash: contentHash(d.Title, d.Body), hasMetadata: len(d.Labels)+len(d.Assignees) > 0, assignees: d.Assignees, allowed: allowed})
}

func (s *Service) update(w http.ResponseWriter, r *http.Request) {
	sw := newSubmitWriter(w)
	number, ok := pathID(r, "number")
	if !ok {
		http.Error(sw, "not found", http.StatusNotFound)
		return
	}
	var d updateDraft
	if err := decodeStrict(sw, r, &d); err != nil {
		http.Error(sw, err.Error(), http.StatusBadRequest)
		return
	}
	if err := d.validate(); err != nil {
		http.Error(sw, err.Error(), http.StatusBadRequest)
		return
	}
	// GitHub lets an issue's author close and reopen it with read access alone; labels and assignees need triage.
	stateOnly := len(d.Labels.Add)+len(d.Labels.Remove)+len(d.Assignees.Add)+len(d.Assignees.Remove) == 0
	s.submit(sw, r, command{kind: "update", issueNumber: number, draft: &d, assignees: d.Assignees.Add, allowed: canManage, authorMay: stateOnly})
}

func (s *Service) replay(sw *submitWriter, ctx context.Context, userID int64, key string, hash []byte, snap accessfence.Snapshot) bool {
	prior, err := s.Queries.GetWriteOperationByKey(ctx, db.GetWriteOperationByKeyParams{UserID: userID, IdempotencyKey: key})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return false
	case err != nil:
		sw.disposition = ""
		s.internal(sw, err)
	case !bytes.Equal(prior.RequestHash, hash):
		sw.disposition = WriteDispositionNotRecorded
		http.Error(sw, "idempotency key reused with a different request", http.StatusUnprocessableEntity)
	default:
		sw.disposition = WriteDispositionPossiblyRecorded
		if err := s.Queries.FailStalePendingOperations(ctx, userID); err != nil {
			s.internal(sw, err)
			return true
		}
		s.respond(sw, ctx, http.StatusOK, userID, prior.ID, snap)
	}
	return true
}

var errAccessMoved = errors.New("access changed")

func (s *Service) submit(sw *submitWriter, r *http.Request, c command) {
	ctx := r.Context()
	userID, ok := auth.SignedInUserID(ctx)
	repoID, idOK := pathID(r, "id")
	if !ok || !idOK {
		http.Error(sw, "not found", http.StatusNotFound)
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if !keyPattern.MatchString(key) {
		http.Error(sw, "an Idempotency-Key header of 1 to 128 letters, digits, or ._:- is required", http.StatusBadRequest)
		return
	}
	draft, err := json.Marshal(c.draft)
	if err != nil {
		s.internal(sw, err)
		return
	}
	sum := sha256.Sum256(fmt.Appendf(nil, "%s\x00%d\x00%d\x00%s", c.kind, repoID, c.issueNumber, draft))
	hash := sum[:]

	d, snap, ok := s.checkedAccess(sw, r, userID, repoID)
	if !ok {
		return
	}
	if s.replay(sw, ctx, userID, key, hash, snap) {
		return
	}
	denied := func() {
		http.Error(sw, "your current GitHub permission does not allow this action", http.StatusForbidden)
	}
	if !c.allowed(d.Repo) && !c.authorMay {
		denied()
		return
	}
	token, ok := s.token(sw, ctx, userID)
	if !ok {
		return
	}
	if c.kind == "update" {
		issue, err := s.GitHub.FetchIssue(ctx, token, d.Repo.FullName, c.issueNumber)
		var api *githubapp.APIError
		if errors.As(err, &api) && api.NotFound() || err == nil && issue.PullRequest != nil {
			http.Error(sw, "issue not found", http.StatusNotFound)
			return
		}
		if err != nil {
			s.upstream(sw, ctx, userID, err)
			return
		}
		if !c.allowed(d.Repo) {
			actor, err := s.Queries.GetUserGithubID(ctx, userID)
			if err != nil {
				s.internal(sw, err)
				return
			}
			if issue.User == nil || issue.User.ID != actor || !canCreate(d.Repo) {
				denied()
				return
			}
		}
		if e := c.draft.(*updateDraft).Expect; e != nil {
			if live := viewIssue(issue); !e.matches(live) {
				writeJSON(sw, http.StatusConflict, map[string]any{"error": "changed on GitHub", "current": map[string]any{"state": live.State,
					"labels": live.Labels, "assignees": live.Assignees, "status": boardStatus(live.State, live.Labels), "updatedAt": issue.UpdatedAt}})
				return
			}
		}
	}
	var ineligible []string
	for _, login := range c.assignees {
		ok, err := s.GitHub.IsAssignable(ctx, token, d.Repo.FullName, login)
		if err != nil {
			s.upstream(sw, ctx, userID, err)
			return
		}
		if !ok {
			ineligible = append(ineligible, login)
		}
	}
	if len(ineligible) > 0 {
		http.Error(sw, "cannot be assigned in this repository: "+strings.Join(ineligible, ", "), http.StatusUnprocessableEntity)
		return
	}

	sw.disposition = ""
	ctx = context.WithoutCancel(ctx)
	var opID int64
	insert := func() error {
		return db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
			if current, err := snap.StillCurrent(ctx, tx); err != nil {
				return err
			} else if !current {
				return errAccessMoved
			}
			var err error
			opID, err = s.Queries.WithTx(tx).InsertWriteOperation(ctx, db.InsertWriteOperationParams{UserID: userID, RepositoryID: repoID, IdempotencyKey: key, Kind: c.kind,
				IssueNumber: pgtype.Int8{Int64: c.issueNumber, Valid: c.kind == "update"}, RequestHash: hash, ContentHash: c.contentHash, HasRequestedMetadata: c.hasMetadata, Draft: draft})
			return err
		})
	}
	for attempt := 0; ; attempt++ {
		err = insert()
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.ConstraintName != "write_operations_unresolved_create" {
			break
		}
		unresolved := db.FindUnresolvedCreateParams{UserID: userID, RepositoryID: repoID, ContentHash: c.contentHash}
		if attempt == 0 {
			n, err := s.Queries.FailStalePendingCreate(ctx, db.FailStalePendingCreateParams(unresolved))
			if err != nil {
				s.internal(sw, err)
				return
			}
			if n > 0 {
				continue
			}
		}
		sw.disposition = WriteDispositionNotRecorded
		msg := "an earlier create of this issue is still unresolved; recheck it instead of resubmitting"
		prior, err := s.Queries.FindUnresolvedCreate(ctx, unresolved)
		if errors.Is(err, pgx.ErrNoRows) {
			msg = "an earlier create of this issue was resolved while submitting; nothing was sent: check it before resubmitting"
			prior, err = s.Queries.FindLatestCreate(ctx, db.FindLatestCreateParams(unresolved))
		}
		switch {
		case err == nil:
			writeJSON(sw, http.StatusConflict, map[string]any{"error": msg, "operationId": prior})
		case errors.Is(err, pgx.ErrNoRows):
			http.Error(sw, "an earlier create of this issue changed while submitting; nothing was sent, retry", http.StatusServiceUnavailable)
		default:
			s.internal(sw, err)
		}
		return
	}
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		if !s.replay(sw, ctx, userID, key, hash, snap) {
			s.internal(sw, errors.New("idempotency key vanished"))
		}
		return
	case errors.Is(err, errAccessMoved):
		sw.disposition = WriteDispositionNotRecorded
		http.Error(sw, "access changed while submitting; nothing was sent", http.StatusServiceUnavailable)
		return
	case err != nil:
		s.internal(sw, err)
		return
	}
	sw.disposition = WriteDispositionPossiblyRecorded
	if n, err := s.Queries.MarkWriteOperationSent(ctx, opID); err != nil {
		s.internal(sw, fmt.Errorf("mark sent: %w", err))
		return
	} else if n == 0 {
		if err := s.Queries.FailStalePendingOperations(ctx, userID); err != nil {
			s.internal(sw, err)
			return
		}
		s.respond(sw, ctx, http.StatusOK, userID, opID, snap)
		return
	}
	switch c.kind {
	case "status_labels":
		state, out := s.ensureStatusLabels(ctx, userID, token, d.Repo.FullName)
		err = s.finishStatusLabels(ctx, opID, repoID, snap, state, out)
	case "create":
		state, out, confirmed := s.sendCreate(ctx, userID, token, d.Repo.FullName, c.draft.(*createDraft))
		err = s.finish(ctx, opID, repoID, snap, 0, confirmed != nil, func() (string, outcome, *githubapp.Issue, error) { return state, out, confirmed, nil })
	default:
		u := c.draft.(*updateDraft)
		sent := s.applyUpdate(ctx, userID, token, d.Repo.FullName, c.issueNumber, u)
		var readErr error
		err = s.finish(ctx, opID, repoID, snap, c.issueNumber, false, func() (string, outcome, *githubapp.Issue, error) {
			state, out, confirmed, fetchErr := s.readBack(ctx, token, d.Repo.FullName, c.issueNumber, u, sent)
			readErr = fetchErr
			return state, out, confirmed, nil
		})
		s.revokeIfUnauthorized(ctx, userID, readErr)
	}
	if err != nil {
		s.internal(sw, err)
		return
	}
	s.respond(sw, ctx, http.StatusCreated, userID, opID, snap)
}

func classify(err error) (string, string) {
	if errors.Is(err, githubapp.ErrNotSent) {
		return "failed", "the request could not be prepared; nothing was sent to GitHub"
	}
	var api *githubapp.APIError
	if errors.As(err, &api) && api.Status >= 400 && api.Status < 500 {
		return "failed", fmt.Sprintf("GitHub rejected the request (status %d)", api.Status)
	}
	if api != nil {
		return "unknown", fmt.Sprintf("GitHub answered status %d; the change may or may not have been applied", api.Status)
	}
	return "unknown", "no reliable answer from GitHub; the change may or may not have been applied"
}

func viewIssue(issue githubapp.Issue) *issueView {
	v := &issueView{Number: issue.Number, URL: issue.HTMLURL, Title: issue.Title, State: issue.State, Labels: []string{}, Assignees: []string{}}
	for _, l := range issue.Labels {
		v.Labels = append(v.Labels, l.Name)
	}
	for _, a := range issue.Assignees {
		v.Assignees = append(v.Assignees, a.Login)
	}
	return v
}

func has(list []string, v string) bool {
	return slices.ContainsFunc(list, func(o string) bool { return strings.EqualFold(o, v) })
}

func unapplied(issue *issueView, labels, assignees delta, state string) []string {
	var out []string
	for _, x := range []struct {
		what    string
		want    delta
		current []string
	}{{"label", labels, issue.Labels}, {"assignee", assignees, issue.Assignees}} {
		for _, v := range x.want.Add {
			if !has(x.current, v) {
				out = append(out, "add "+x.what+" "+v)
			}
		}
		for _, v := range x.want.Remove {
			if has(x.current, v) {
				out = append(out, "remove "+x.what+" "+v)
			}
		}
	}
	if state != "" && issue.State != state {
		out = append(out, "state "+state)
	}
	return out
}

func resolved(out outcome) string {
	if len(out.Unapplied) > 0 {
		return "failed"
	}
	return "succeeded"
}

func (s *Service) sendCreate(ctx context.Context, userID int64, token, fullName string, d *createDraft) (string, outcome, *githubapp.Issue) {
	issue, err := s.GitHub.CreateIssue(ctx, token, fullName, githubapp.NewIssue{Title: d.Title, Body: d.Body, Labels: d.Labels, Assignees: d.Assignees})
	if err != nil {
		s.revokeIfUnauthorized(ctx, userID, err)
		state, msg := classify(err)
		return state, outcome{Error: msg}, nil
	}
	out := outcome{Issue: viewIssue(issue)}
	out.Unapplied = unapplied(out.Issue, delta{Add: d.Labels}, delta{Add: d.Assignees}, "")
	return resolved(out), out, &issue
}

// applyUpdate applies add/remove steps (each idempotent), stopping at the first error; readBack then decides the
// outcome. Assignees are removed before they are added, so a swap at GitHub's 10-assignee limit fits. A reopen is the
// first step and a close the last, so a change that reopens or closes an issue never adds a label while it is still
// closed, where the app's status label cleanup could strip it; a rejected reopen applies nothing.
func (s *Service) applyUpdate(ctx context.Context, userID int64, token, fullName string, number int64, d *updateDraft) outcome {
	steps := []func() error{}
	setState := func() error { return s.GitHub.SetIssueState(ctx, token, fullName, number, d.State) }
	if d.State == "open" {
		steps = append(steps, setState)
	}
	if len(d.Labels.Add) > 0 {
		steps = append(steps, func() error { return s.GitHub.AddLabels(ctx, token, fullName, number, d.Labels.Add) })
	}
	for _, l := range d.Labels.Remove {
		steps = append(steps, func() error { return s.GitHub.RemoveLabel(ctx, token, fullName, number, l) })
	}
	if len(d.Assignees.Remove) > 0 {
		steps = append(steps, func() error { return s.GitHub.RemoveAssignees(ctx, token, fullName, number, d.Assignees.Remove) })
	}
	if len(d.Assignees.Add) > 0 {
		steps = append(steps, func() error { return s.GitHub.AddAssignees(ctx, token, fullName, number, d.Assignees.Add) })
	}
	if d.State == "closed" {
		steps = append(steps, setState)
	}
	out := outcome{AllStepsAnswered: true}
	for _, step := range steps {
		if err := step(); err != nil {
			s.revokeIfUnauthorized(ctx, userID, err)
			var state string
			state, out.Error = classify(err)
			out.AllStepsAnswered = state == "failed"
			break
		}
	}
	return out
}

func (s *Service) readBack(ctx context.Context, token, fullName string, number int64, d *updateDraft, out outcome) (string, outcome, *githubapp.Issue, error) {
	issue, err := s.GitHub.FetchIssue(ctx, token, fullName, number)
	if err != nil {
		_, msg := classify(err)
		out.Error = strings.TrimSpace(out.Error + " Reading the result failed: " + msg)
		return "unknown", out, nil, err
	}
	out.Issue = viewIssue(issue)
	out.Unapplied = unapplied(out.Issue, d.Labels, d.Assignees, d.State)
	if len(out.Unapplied) > 0 && !out.AllStepsAnswered {
		return "unknown", out, &issue, nil
	}
	return resolved(out), out, &issue, nil
}

var errAlreadyLinked = errors.New("issue already linked to another operation")

var errMismatch = errors.New("not confirmed")

func (s *Service) finish(ctx context.Context, opID, repoID int64, snap accessfence.Snapshot, lock int64, link bool, read func() (string, outcome, *githubapp.Issue, error)) error {
	err := db.InTx(ctx, s.Pool, func(tx pgx.Tx) (err error) {
		if lock != 0 {
			if _, err = tx.Exec(ctx, `SET LOCAL lock_timeout = '10s'`); err != nil {
				return err
			}
			if err = ingestion.LockIssue(ctx, tx, repoID, lock); err != nil {
				return err
			}
			if _, err = tx.Exec(ctx, `SET LOCAL lock_timeout = 0`); err != nil {
				return err
			}
		}
		state, out, confirmed, err := read()
		if err != nil {
			return err
		}
		body, err := json.Marshal(out)
		if err != nil {
			return err
		}
		current := false
		if confirmed != nil && state != "unknown" {
			if current, err = snap.StillCurrent(ctx, tx); err != nil {
				return err
			}
		}
		number := pgtype.Int8{}
		if link {
			number = pgtype.Int8{Int64: confirmed.Number, Valid: true}
		}
		n, err := s.Queries.WithTx(tx).FinishWriteOperation(ctx, db.FinishWriteOperationParams{ID: opID, State: state, Outcome: body, IssueNumber: number})
		if err != nil || n == 0 || !current {
			return err
		}
		if lock != 0 {
			return ingestion.ProjectConfirmedIssue(ctx, tx, repoID, *confirmed)
		}
		if err = ingestion.LockIssue(ctx, tx, repoID, confirmed.Number); err != nil {
			return err
		}
		return ingestion.ProjectIssue(ctx, tx, repoID, *confirmed)
	})
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return errAlreadyLinked
	}
	return err
}

const sendSkew = 5 * time.Second

func (s *Service) recheck(w http.ResponseWriter, r *http.Request) {
	var in struct {
		IssueNumber int64 `json:"issueNumber"`
	}
	if err := decodeStrict(w, r, &in); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	userID, row, d, snap, ok := s.loadOperation(w, r, true)
	if !ok {
		return
	}
	if row.State != "unknown" {
		writeJSON(w, http.StatusOK, s.view(row))
		return
	}
	if row.Kind == "status_labels" {
		s.recheckStatusLabels(w, r, userID, row, d, snap)
		return
	}
	number := row.IssueNumber.Int64
	if row.Kind == "create" {
		if in.IssueNumber <= 0 {
			http.Error(w, "issueNumber is required to recheck a create", http.StatusBadRequest)
			return
		}
		number = in.IssueNumber
	}
	ctx := r.Context()
	token, ok := s.token(w, ctx, userID)
	if !ok {
		return
	}
	mismatch := func() {
		if s.fenceHolds(w, ctx, snap) {
			writeJSON(w, http.StatusConflict, s.view(row))
		}
	}
	var fetchErr error
	err := s.finish(context.WithoutCancel(ctx), row.ID, row.RepositoryID, snap, number, row.Kind == "create", func() (string, outcome, *githubapp.Issue, error) {
		issue, err := s.GitHub.FetchIssue(ctx, token, d.Repo.FullName, number)
		var api *githubapp.APIError
		switch {
		case errors.As(err, &api) && api.NotFound():
			return "", outcome{}, nil, errMismatch
		case err != nil:
			fetchErr = err
			return "", outcome{}, nil, err
		}
		var out outcome
		out.Issue = viewIssue(issue)
		if row.Kind == "create" {
			if issue.PullRequest != nil || issue.User == nil || issue.User.ID != row.ActorGithubID ||
				!strings.EqualFold(issue.RepoURL, s.GitHub.APIURL+"/repos/"+d.Repo.FullName) ||
				!bytes.Equal(contentHash(issue.Title, issue.Body), row.ContentHash) ||
				!row.SentAt.Valid || issue.CreatedAt.Before(row.SentAt.Time.Add(-sendSkew)) {
				return "", outcome{}, nil, errMismatch
			}
			var draft createDraft
			switch {
			case row.Draft != nil:
				if err := json.Unmarshal(row.Draft, &draft); err != nil {
					return "", outcome{}, nil, err
				}
				out.Unapplied = unapplied(out.Issue, delta{Add: draft.Labels}, delta{Add: draft.Assignees}, "")
			case row.HasRequestedMetadata:
				out.Unapplied = []string{"requested labels or assignees (unverifiable: the draft was removed)"}
			}
		} else {
			var draft updateDraft
			var sent outcome
			if row.Draft == nil || json.Unmarshal(row.Draft, &draft) != nil || json.Unmarshal(row.Outcome, &sent) != nil {
				return "", outcome{}, nil, errMismatch
			}
			out.Unapplied = unapplied(out.Issue, draft.Labels, draft.Assignees, draft.State)
			if len(out.Unapplied) > 0 && !sent.AllStepsAnswered {
				return "", outcome{}, nil, errMismatch
			}
			out.Error, out.AllStepsAnswered = sent.Error, sent.AllStepsAnswered
		}
		return resolved(out), out, &issue, nil
	})
	switch {
	case fetchErr != nil:
		s.upstream(w, ctx, userID, fetchErr)
		return
	case errors.Is(err, errMismatch), errors.Is(err, errAlreadyLinked):
		mismatch()
		return
	case err != nil:
		s.internal(w, err)
		return
	}
	s.respond(w, ctx, http.StatusOK, userID, row.ID, snap)
}

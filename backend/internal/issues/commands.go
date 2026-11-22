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
	"net/url"
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
)

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
// "operationId"}) and a recheck's 409 (the unchanged operation).
func (s *Service) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/repositories/{id}/issue-capabilities", s.capabilities)
	mux.HandleFunc("POST /api/repositories/{id}/issues", s.create)
	mux.HandleFunc("POST /api/repositories/{id}/issues/{number}/updates", s.update)
	mux.HandleFunc("GET /api/repositories/{id}/write-operations", s.list)
	mux.HandleFunc("GET /api/write-operations/{id}", s.get)
	mux.HandleFunc("POST /api/write-operations/{id}/recheck", s.recheck)
}

// createDraft is a new issue as the user reviewed it.
type createDraft struct {
	Title     string   `json:"title"`
	Body      string   `json:"body"`
	Labels    []string `json:"labels"`
	Assignees []string `json:"assignees"`
}

// delta expresses add/remove intent; values not named are never touched. Names compare case-insensitively, and one
// name may not appear in both lists.
type delta struct {
	Add    []string `json:"add"`
	Remove []string `json:"remove"`
}

// updateDraft changes an existing issue: labels and assignees by add/remove, and optionally its state.
type updateDraft struct {
	Labels    delta  `json:"labels"`
	Assignees delta  `json:"assignees"`
	State     string `json:"state"` // requested issue state: "" (unchanged), "open" or "closed"
}

// issueView is an issue as GitHub returned or re-read it.
type issueView struct {
	Number    int64    `json:"number"`
	URL       string   `json:"url"`
	Title     string   `json:"title"`
	State     string   `json:"state"` // the issue's GitHub state, "open" or "closed" (not the operation's state)
	Labels    []string `json:"labels"`
	Assignees []string `json:"assignees"`
}

// outcome is what GitHub actually did. Unapplied lists requested changes the confirmed issue does not show, each as
// "add label X", "remove label X", "add assignee X", "remove assignee X" or "state open|closed" (or one fixed
// sentence when a wiped create's labels and assignees cannot be verified). AllStepsAnswered (updates only) means every
// step got a definite answer (2xx, 4xx, or failed before it was sent), so nothing of the update can still land and a
// recheck may fail it. Once the draft is wiped the stored outcome is redacted instead, by SQL redacted_write_outcome, to
// {issue: {number, url, state}, unappliedCount, error, redacted: true}; clients must accept both shapes.
type outcome struct {
	Issue            *issueView `json:"issue,omitempty"`
	Unapplied        []string   `json:"unapplied,omitempty"`
	Error            string     `json:"error,omitempty"` // a summary that never quotes credentials or content
	AllStepsAnswered bool       `json:"allStepsAnswered,omitempty"`
}

// opView is one write operation as the API returns it.
type opView struct {
	ID           int64           `json:"id"`
	RepositoryID int64           `json:"repositoryId"`        // the repository's GitHub ID
	Kind         string          `json:"kind"`                // "create" or "update"
	State        string          `json:"state"`               // the operation's state: "pending", "unknown", "succeeded" or "failed"
	IssueNumber  *int64          `json:"issueNumber"`         // an update's target; a create's issue once confirmed, else null
	Draft        json.RawMessage `json:"draft"`               // null once wiped after the actor lost access
	Outcome      json.RawMessage `json:"outcome"`             // an outcome, full or redacted
	Actor        string          `json:"actor"`               // the initiating user's GitHub login
	SearchURL    string          `json:"searchUrl,omitempty"` // unknown creates only: a GitHub issue search for the actor's issue with the draft title
	CreatedAt    time.Time       `json:"createdAt"`
	UpdatedAt    time.Time       `json:"updatedAt"`
}

// --- input -------------------------------------------------------------------------------------

// decodeStrict reads exactly one JSON object with no unknown fields.
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

// names validates a label or login list, returning nil for an empty one.
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
	return nil
}

// contentHash identifies an issue's text, so a recheck can verify a candidate even after the draft is wiped.
func contentHash(title, body string) []byte {
	norm := func(s string) string { return strings.ReplaceAll(strings.TrimSpace(s), "\r\n", "\n") }
	sum := sha256.Sum256([]byte(norm(title) + "\x00" + norm(body)))
	return sum[:]
}

// --- permissions -------------------------------------------------------------------------------

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

// --- HTTP plumbing -----------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func pathID(r *http.Request, name string) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue(name), 10, 64)
	return id, err == nil && id > 0
}

// access rechecks the viewer's live access to a stored repository. It answers 404 (denied or not stored) or 503
// (unverifiable) itself and returns ok=false. A definite denial on a private repository wipes the viewer's drafts
// there, since no webhook may ever report that their access ended.
// ponytail: a silently removed collaborator keeps their drafts until their next read here or a user/installation
// revocation; handle GitHub's member webhook or sweep drafts periodically if that window matters.
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

// checkedAccess is access checked fresh against GitHub, with the fence snapshot taken before the check, so a
// revocation committing during the check is never absorbed. The check may itself record a changed installation list
// and move the fence; it then runs once more, and a second move answers 503.
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

// fenceHolds answers 503 itself and reports false when access changed since snap: what was loaded under it is withheld.
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
	log.Printf("issues: %v", err) // database errors only; GitHub errors are summarized before they get here
	http.Error(w, "temporarily unavailable", http.StatusInternalServerError)
}

// token returns the actor's user token, answering 401/503 itself when there is none.
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

// upstream answers a failed pre-flight GitHub read (nothing was written): 401 revokes the actor, anything else is 503.
func (s *Service) upstream(w http.ResponseWriter, ctx context.Context, userID int64, err error) {
	if s.revokeIfUnauthorized(ctx, userID, err) {
		http.Error(w, "sign in required", http.StatusUnauthorized)
		return
	}
	http.Error(w, "GitHub could not be reached; nothing was sent", http.StatusServiceUnavailable)
}

// revokeIfUnauthorized revokes the actor's GitHub authorization when err is GitHub's 401 and reports whether it was.
// The revocation takes its own pool connection, so never call it inside a transaction.
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

func (s *Service) view(row db.GetWriteOperationRow) opView {
	v := opView{ID: row.ID, RepositoryID: row.RepositoryID, Kind: row.Kind, State: row.State, Draft: row.Draft, Outcome: row.Outcome,
		Actor: row.ActorLogin, CreatedAt: row.CreatedAt.Time, UpdatedAt: row.UpdatedAt.Time}
	if row.IssueNumber.Valid {
		v.IssueNumber = &row.IssueNumber.Int64
	}
	if row.Kind == "create" && row.State == "unknown" {
		q := "is:issue author:" + row.ActorLogin
		var d createDraft
		if json.Unmarshal(row.Draft, &d) == nil && d.Title != "" {
			q += ` in:title "` + strings.ReplaceAll(d.Title, `"`, "") + `"`
		}
		v.SearchURL = s.GitHubURL + "/" + row.RepositoryName + "/issues?q=" + url.QueryEscape(q)
	}
	return v
}

// respond reads the operation back and writes it, unless access changed since snap (the snapshot the request's access
// check was made under): then it is withheld with 503, as other fenced reads are. The operation exists and may have
// been sent, so the client must retry under the same key, never a fresh one.
func (s *Service) respond(w http.ResponseWriter, ctx context.Context, status int, userID, opID int64, snap accessfence.Snapshot) {
	row, err := s.Queries.GetWriteOperation(ctx, db.GetWriteOperationParams{ID: opID, UserID: userID})
	if err != nil {
		s.internal(w, err)
		return
	}
	current, err := snap.Current(ctx, s.Pool)
	if err != nil {
		s.internal(w, err)
		return
	}
	if !current {
		http.Error(w, "access changed; the operation was recorded and may have been sent: retry with the same Idempotency-Key", http.StatusServiceUnavailable)
		return
	}
	if status == http.StatusCreated && row.State == "unknown" {
		status = http.StatusAccepted // recorded and possibly applied; the outcome is not known
	}
	writeJSON(w, status, s.view(row))
}

// --- reads -------------------------------------------------------------------------------------

func (s *Service) capabilities(w http.ResponseWriter, r *http.Request) {
	userID, ok := auth.SignedInUserID(r.Context())
	repoID, idOK := pathID(r, "id")
	if !ok || !idOK {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	d, ok := s.access(w, r, userID, repoID, false)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"canCreateIssue": canCreate(d.Repo), "canCreateIssueWithMetadata": canCreateWithMetadata(d.Repo), "canManageIssues": canManage(d.Repo)})
}

// list returns the viewer's own operations on one repository; other users' operations are never listed.
func (s *Service) list(w http.ResponseWriter, r *http.Request) {
	userID, ok := auth.SignedInUserID(r.Context())
	repoID, idOK := pathID(r, "id")
	if !ok || !idOK {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	_, snap, ok := s.checkedAccess(w, r, userID, repoID)
	if !ok {
		return
	}
	rows, err := s.Queries.ListWriteOperations(r.Context(), db.ListWriteOperationsParams{UserID: userID, RepositoryID: repoID})
	if err != nil {
		s.internal(w, err)
		return
	}
	if !s.fenceHolds(w, r.Context(), snap) {
		return
	}
	out := make([]opView, 0, len(rows))
	for _, row := range rows {
		out = append(out, s.view(db.GetWriteOperationRow(row)))
	}
	writeJSON(w, http.StatusOK, map[string]any{"operations": out})
}

// operation loads one of the viewer's operations under a fresh access check of its repository, withholding it when
// access changed while it loaded. Another user's id, a missing id and lost access all answer 404. The snapshot is the
// one the access check was made under.
func (s *Service) operation(w http.ResponseWriter, r *http.Request) (int64, db.GetWriteOperationRow, repositories.Decision, accessfence.Snapshot, bool) {
	userID, ok := auth.SignedInUserID(r.Context())
	opID, idOK := pathID(r, "id")
	if !ok || !idOK {
		http.Error(w, "not found", http.StatusNotFound)
		return 0, db.GetWriteOperationRow{}, repositories.Decision{}, accessfence.Snapshot{}, false
	}
	load := func() (db.GetWriteOperationRow, bool) {
		row, err := s.Queries.GetWriteOperation(r.Context(), db.GetWriteOperationParams{ID: opID, UserID: userID})
		if errors.Is(err, pgx.ErrNoRows) {
			http.Error(w, "not found", http.StatusNotFound)
			return row, false
		}
		if err != nil {
			s.internal(w, err)
			return row, false
		}
		return row, true
	}
	row, ok := load() // only to learn the repository; read again once access is known
	if !ok {
		return 0, row, repositories.Decision{}, accessfence.Snapshot{}, false
	}
	d, snap, ok := s.checkedAccess(w, r, userID, row.RepositoryID)
	if !ok {
		return 0, row, d, snap, false
	}
	if err := s.Queries.FailStalePendingOperation(r.Context(), row.ID); err != nil {
		s.internal(w, err)
		return 0, row, d, snap, false
	}
	if row, ok = load(); !ok || !s.fenceHolds(w, r.Context(), snap) {
		return 0, row, d, snap, false
	}
	return userID, row, d, snap, true
}

func (s *Service) get(w http.ResponseWriter, r *http.Request) {
	if _, row, _, _, ok := s.operation(w, r); ok {
		writeJSON(w, http.StatusOK, s.view(row))
	}
}

// --- commands ----------------------------------------------------------------------------------

// command is one validated write, ready to record and send.
type command struct {
	kind        string // "create" or "update"
	issueNumber int64  // update target
	draft       any    // *createDraft or *updateDraft
	contentHash []byte // create only
	hasMetadata bool   // create only: labels or assignees requested
	assignees   []string
	allowed     func(githubapp.Repo) bool // the per-action permission rule
	authorMay   bool                      // update only: the issue's author may do this without allowed (close/reopen)
}

func (s *Service) create(w http.ResponseWriter, r *http.Request) {
	var d createDraft
	if err := decodeStrict(w, r, &d); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := d.validate(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	allowed := canCreate
	if len(d.Labels)+len(d.Assignees) > 0 {
		allowed = canCreateWithMetadata
	}
	s.submit(w, r, command{kind: "create", draft: &d, contentHash: contentHash(d.Title, d.Body), hasMetadata: len(d.Labels)+len(d.Assignees) > 0, assignees: d.Assignees, allowed: allowed})
}

func (s *Service) update(w http.ResponseWriter, r *http.Request) {
	number, ok := pathID(r, "number")
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	var d updateDraft
	if err := decodeStrict(w, r, &d); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := d.validate(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// GitHub lets an issue's author close and reopen it with read access alone; labels and assignees need triage.
	stateOnly := len(d.Labels.Add)+len(d.Labels.Remove)+len(d.Assignees.Add)+len(d.Assignees.Remove) == 0
	s.submit(w, r, command{kind: "update", issueNumber: number, draft: &d, assignees: d.Assignees.Add, allowed: canManage, authorMay: stateOnly})
}

// replay answers a reused idempotency key: the existing operation for the same request, 422 for a different one.
// It reports false when the key is unused.
func (s *Service) replay(w http.ResponseWriter, ctx context.Context, userID int64, key string, hash []byte, snap accessfence.Snapshot) bool {
	prior, err := s.Queries.GetWriteOperationByKey(ctx, db.GetWriteOperationByKeyParams{UserID: userID, IdempotencyKey: key})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return false
	case err != nil:
		s.internal(w, err)
	case !bytes.Equal(prior.RequestHash, hash):
		http.Error(w, "idempotency key reused with a different request", http.StatusUnprocessableEntity)
	default:
		if err := s.Queries.FailStalePendingOperation(ctx, prior.ID); err != nil {
			s.internal(w, err)
			return true
		}
		s.respond(w, ctx, http.StatusOK, userID, prior.ID, snap)
	}
	return true
}

var errAccessMoved = errors.New("access changed")

// submit records and sends one command: validate access and permission, record the operation as pending, mark it
// unknown, send it with the actor's token, then record what GitHub confirmed. An operation that is no longer pending
// when it is marked (a stale-pending recovery failed it) is sent nowhere and answered as a replay.
func (s *Service) submit(w http.ResponseWriter, r *http.Request, c command) {
	ctx := r.Context()
	userID, ok := auth.SignedInUserID(ctx)
	repoID, idOK := pathID(r, "id")
	if !ok || !idOK {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if !keyPattern.MatchString(key) {
		http.Error(w, "an Idempotency-Key header of 1 to 128 letters, digits, or ._:- is required", http.StatusBadRequest)
		return
	}
	draft, err := json.Marshal(c.draft)
	if err != nil {
		s.internal(w, err)
		return
	}
	sum := sha256.Sum256(fmt.Appendf(nil, "%s\x00%d\x00%d\x00%s", c.kind, repoID, c.issueNumber, draft))
	hash := sum[:]

	d, snap, ok := s.checkedAccess(w, r, userID, repoID)
	if !ok {
		return
	}
	if s.replay(w, ctx, userID, key, hash, snap) {
		return
	}
	denied := func() {
		http.Error(w, "your current GitHub permission does not allow this action", http.StatusForbidden)
	}
	if !c.allowed(d.Repo) && !c.authorMay {
		denied()
		return
	}
	token, ok := s.token(w, ctx, userID)
	if !ok {
		return
	}
	if c.kind == "update" {
		issue, err := s.GitHub.FetchIssue(ctx, token, d.Repo.FullName, c.issueNumber)
		var api *githubapp.APIError
		if errors.As(err, &api) && api.NotFound() || err == nil && issue.PullRequest != nil {
			http.Error(w, "issue not found", http.StatusNotFound)
			return
		}
		if err != nil {
			s.upstream(w, ctx, userID, err)
			return
		}
		if !c.allowed(d.Repo) {
			actor, err := s.Queries.GetUserGithubID(ctx, userID)
			if err != nil {
				s.internal(w, err)
				return
			}
			if issue.User == nil || issue.User.ID != actor || !canCreate(d.Repo) {
				denied()
				return
			}
		}
	}
	var ineligible []string
	for _, login := range c.assignees {
		ok, err := s.GitHub.IsAssignable(ctx, token, d.Repo.FullName, login)
		if err != nil {
			s.upstream(w, ctx, userID, err)
			return
		}
		if !ok {
			ineligible = append(ineligible, login)
		}
	}
	if len(ineligible) > 0 {
		http.Error(w, "cannot be assigned in this repository: "+strings.Join(ineligible, ", "), http.StatusUnprocessableEntity)
		return
	}

	// Once recording starts, a client disconnect must not strand the row between insert and mark-sent (or abandon the
	// call and its recorded outcome later).
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
	// An unresolved create of the same content blocks the insert: a second one could duplicate the issue. Only a
	// blocker this request itself fails as a stale, never-sent pending create frees the content for one retry. Any other
	// blocker may have been sent, and may have succeeded even if it no longer blocks, so nothing is resent: the answer
	// is 409 naming the unresolved create, else the latest create of the same content, else 503.
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
				s.internal(w, err)
				return
			}
			if n > 0 {
				continue
			}
		}
		msg := "an earlier create of this issue is still unresolved; recheck it instead of resubmitting"
		prior, err := s.Queries.FindUnresolvedCreate(ctx, unresolved)
		if errors.Is(err, pgx.ErrNoRows) {
			msg = "an earlier create of this issue was resolved while submitting; nothing was sent: check it before resubmitting"
			prior, err = s.Queries.FindLatestCreate(ctx, db.FindLatestCreateParams(unresolved))
		}
		switch {
		case err == nil:
			writeJSON(w, http.StatusConflict, map[string]any{"error": msg, "operationId": prior})
		case errors.Is(err, pgx.ErrNoRows):
			http.Error(w, "an earlier create of this issue changed while submitting; nothing was sent, retry", http.StatusServiceUnavailable)
		default:
			s.internal(w, err)
		}
		return
	}
	switch {
	case errors.Is(err, pgx.ErrNoRows): // the same key was recorded concurrently
		if !s.replay(w, ctx, userID, key, hash, snap) {
			s.internal(w, errors.New("idempotency key vanished"))
		}
		return
	case errors.Is(err, errAccessMoved):
		http.Error(w, "access changed while submitting; nothing was sent", http.StatusServiceUnavailable)
		return
	case err != nil:
		s.internal(w, err)
		return
	}
	// From here the request may reach GitHub: the row says unknown first, so a crash can never read as "not sent".
	if n, err := s.Queries.MarkWriteOperationSent(ctx, opID); err != nil {
		s.internal(w, fmt.Errorf("mark sent: %w", err))
		return
	} else if n == 0 { // no longer pending: a stale-pending recovery failed it first, so nothing is sent
		s.respond(w, ctx, http.StatusOK, userID, opID, snap)
		return
	}
	if c.kind == "create" {
		// A create's confirmed copy is GitHub's create response: nothing stored for the new issue is older, so it is
		// projected without the per-issue lock and never replaces a same-second copy.
		state, out, confirmed := s.sendCreate(ctx, userID, token, d.Repo.FullName, c.draft.(*createDraft))
		err = s.finish(ctx, opID, repoID, snap, 0, confirmed != nil, func() (string, outcome, *githubapp.Issue, error) { return state, out, confirmed, nil })
	} else {
		u := c.draft.(*updateDraft)
		sent := s.applyUpdate(ctx, userID, token, d.Repo.FullName, c.issueNumber, u)
		var readErr error
		err = s.finish(ctx, opID, repoID, snap, c.issueNumber, false, func() (string, outcome, *githubapp.Issue, error) {
			state, out, confirmed, fetchErr := s.readBack(ctx, token, d.Repo.FullName, c.issueNumber, u, sent)
			readErr = fetchErr
			return state, out, confirmed, nil
		})
		s.revokeIfUnauthorized(ctx, userID, readErr) // only now: finish's transaction has released its connection
	}
	if err != nil {
		s.internal(w, err)
		return
	}
	s.respond(w, ctx, http.StatusCreated, userID, opID, snap)
}

// classify maps a failed write to an operation state and summary: an error raised before sending
// (githubapp.ErrNotSent) or a 4xx is definite (failed); anything else may have been applied (unknown). It has no side
// effects; callers revoke a 401 themselves (revokeIfUnauthorized).
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

// unapplied lists requested changes that the confirmed issue does not show.
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
		return "failed" // GitHub accepted the request but did not do all of it: never reported as success
	}
	return "succeeded"
}

// sendCreate sends the create once. It is never retried: an ambiguous answer leaves the operation unknown.
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
// outcome. Assignees are removed before they are added, so a swap at GitHub's 10-assignee limit fits.
func (s *Service) applyUpdate(ctx context.Context, userID int64, token, fullName string, number int64, d *updateDraft) outcome {
	steps := []func() error{}
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
	if d.State != "" {
		steps = append(steps, func() error { return s.GitHub.SetIssueState(ctx, token, fullName, number, d.State) })
	}
	out := outcome{AllStepsAnswered: true}
	for _, step := range steps {
		if err := step(); err != nil {
			s.revokeIfUnauthorized(ctx, userID, err)
			var state string
			state, out.Error = classify(err)
			out.AllStepsAnswered = state == "failed" // rejected or never sent; an ambiguous step may still land
			break
		}
	}
	return out
}

// readBack reads the issue after applyUpdate: the outcome is whatever the read shows. A step without a reliable answer
// may still land after the read, so the operation then stays unknown unless the read already shows every change;
// without a readable result it stays unknown too, and the read's error is returned for the caller to act on (a 401
// revokes the actor) once outside finish's transaction. It touches no database.
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

var errMismatch = errors.New("not confirmed") // a recheck read that found nothing to confirm: answer 409, record nothing

// finish records the outcome of an unknown operation, as read supplies it inside the transaction, and, when the access
// fence still holds, projects the confirmed issue locally. link records a create's issue number. With lock (an issue
// number, 0 for none) the transaction first takes a per-issue advisory lock, so the readbacks of one issue and their
// projections happen one at a time: the later readback is projected last and may replace a stored copy with the same
// updated_at (GitHub's timestamps have one-second resolution). Without the lock a tie never replaces. The fence rows
// are locked before the operation row, the order accessfence.InvalidateAccess takes them in, so the two cannot
// deadlock. An unknown outcome with a confirmed read is recorded without projecting it. read runs while the
// transaction holds a pool connection: it may call GitHub and compute, but must not use the pool (no queries, no
// revocation) or write the HTTP response; an error it returns aborts the transaction and is returned as is, and the
// caller acts on it after finish returns.
// ponytail: a locked finish holds one pool connection while GitHub answers its readback; lock waiters give up after
// 10s (the operation stays unknown and a recheck resolves it), so a crowded issue cannot exhaust the pool for good.
func (s *Service) finish(ctx context.Context, opID, repoID int64, snap accessfence.Snapshot, lock int64, link bool, read func() (string, outcome, *githubapp.Issue, error)) error {
	err := db.InTx(ctx, s.Pool, func(tx pgx.Tx) (err error) {
		if lock != 0 {
			if _, err = tx.Exec(ctx, `SET LOCAL lock_timeout = '10s'`); err != nil {
				return err
			}
			if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended(format('issue-write:%s:%s', $1::bigint, $2::bigint), 0))`, repoID, lock); err != nil {
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
			return err // access moved: keep the recorded outcome, let the next sync project the issue
		}
		if lock != 0 {
			return ingestion.ProjectConfirmedIssue(ctx, tx, repoID, *confirmed)
		}
		return ingestion.ProjectIssue(ctx, tx, repoID, *confirmed)
	})
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return errAlreadyLinked
	}
	return err
}

// sendSkew is how much earlier than the recorded send a candidate issue's GitHub created_at may be.
// ponytail: a fixed allowance for clock skew between this database and GitHub; an issue opened by hand within it,
// with identical title and body, is indistinguishable. Widen it if real skew is larger.
const sendSkew = 5 * time.Second

// recheck resolves an unknown operation without resending it. A create needs the issue number the actor found on
// GitHub; it is linked only when that issue is in this repository, authored by the actor, has the drafted title and
// body, and was created no earlier than the send. Anything else keeps the operation unknown (409) with its draft. An
// update is re-read: it succeeds only once every requested change shows, fails only when no request of it can still
// land, and otherwise stays unknown (409).
func (s *Service) recheck(w http.ResponseWriter, r *http.Request) {
	userID, row, d, snap, ok := s.operation(w, r)
	if !ok {
		return
	}
	var in struct {
		IssueNumber int64 `json:"issueNumber"`
	}
	if err := decodeStrict(w, r, &in); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if row.State != "unknown" {
		writeJSON(w, http.StatusOK, s.view(row))
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
	mismatch := func() { // the draft is withheld when access changed during the GitHub read
		if s.fenceHolds(w, ctx, snap) {
			writeJSON(w, http.StatusConflict, s.view(row))
		}
	}
	// The read runs under the issue's lock (see finish); it only decides, and the answer is written after finish
	// returned. errMismatch, or a failed GitHub read (fetchErr), ends the recheck unrecorded.
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
			case row.HasRequestedMetadata: // the draft was wiped: the requested labels and assignees cannot be verified
				out.Unapplied = []string{"requested labels or assignees (unverifiable: the draft was removed)"}
			}
		} else {
			var draft updateDraft
			var sent outcome
			if row.Draft == nil || json.Unmarshal(row.Draft, &draft) != nil || json.Unmarshal(row.Outcome, &sent) != nil {
				return "", outcome{}, nil, errMismatch // nothing left to verify against
			}
			out.Unapplied = unapplied(out.Issue, draft.Labels, draft.Assignees, draft.State)
			if len(out.Unapplied) > 0 && !sent.AllStepsAnswered {
				return "", outcome{}, nil, errMismatch // not visible yet, and the original request may still land
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

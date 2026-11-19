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
	GitHubURL string // browser-facing GitHub origin, for the search link of an unresolved create
}

// Register adds the issue write routes to mux.
func (s *Service) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/repositories/{id}/issue-capabilities", s.capabilities)
	mux.HandleFunc("POST /api/repositories/{id}/issues", s.create)
	mux.HandleFunc("POST /api/repositories/{id}/issues/{number}/changes", s.update)
	mux.HandleFunc("GET /api/repositories/{id}/write-operations", s.list)
	mux.HandleFunc("GET /api/write-operations/{op}", s.get)
	mux.HandleFunc("POST /api/write-operations/{op}/recheck", s.recheck)
}

// createDraft is a new issue as the user reviewed it.
type createDraft struct {
	Title     string   `json:"title"`
	Body      string   `json:"body"`
	Labels    []string `json:"labels"`
	Assignees []string `json:"assignees"`
}

// delta expresses add/remove intent; values not named are never touched.
type delta struct {
	Add    []string `json:"add"`
	Remove []string `json:"remove"`
}

// updateDraft changes an existing issue: labels and assignees by add/remove, and optionally its state.
type updateDraft struct {
	Labels    delta  `json:"labels"`
	Assignees delta  `json:"assignees"`
	State     string `json:"state"` // "", "open" or "closed"
}

type issueView struct {
	Number    int64    `json:"number"`
	URL       string   `json:"url"`
	Title     string   `json:"title"`
	State     string   `json:"state"`
	Labels    []string `json:"labels"`
	Assignees []string `json:"assignees"`
}

// outcome is what GitHub actually did. Unapplied lists requested changes the confirmed issue does not show.
type outcome struct {
	Issue     *issueView `json:"issue,omitempty"`
	Unapplied []string   `json:"unapplied,omitempty"`
	Error     string     `json:"error,omitempty"`
}

type opView struct {
	ID           int64           `json:"id"`
	RepositoryID int64           `json:"repositoryId"`
	Kind         string          `json:"kind"`
	State        string          `json:"state"`
	IssueNumber  *int64          `json:"issueNumber"`
	Draft        json.RawMessage `json:"draft"` // null once wiped after the actor lost access
	Outcome      json.RawMessage `json:"outcome"`
	Actor        string          `json:"actor"`
	SearchURL    string          `json:"searchUrl,omitempty"` // unresolved creates: where to look for the issue on GitHub
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
func writable(r githubapp.Repo) bool { return r.HasIssues && !r.Archived }
func canOpen(r githubapp.Repo) bool  { return writable(r) && (r.Permissions.Pull || canTriage(r)) }
func canOpenWithMetadata(r githubapp.Repo) bool {
	return canOpen(r) && canPush(r)
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
	private, err := s.Queries.GetWriteRepository(r.Context(), repoID)
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
	var api *githubapp.APIError
	if errors.As(err, &api) && api.Unauthorized() {
		s.revoke(ctx, userID)
		http.Error(w, "sign in required", http.StatusUnauthorized)
		return
	}
	http.Error(w, "GitHub could not be reached; nothing was sent", http.StatusServiceUnavailable)
}

func (s *Service) revoke(ctx context.Context, userID int64) {
	if err := s.Access.TokenSource.Revoke(ctx, userID); err != nil {
		log.Printf("issues: revoke failed: %v", err)
	}
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

// respond reads the operation back and writes it.
func (s *Service) respond(w http.ResponseWriter, ctx context.Context, status int, userID, opID int64) {
	row, err := s.Queries.GetWriteOperation(ctx, db.GetWriteOperationParams{ID: opID, UserID: userID})
	if err != nil {
		s.internal(w, err)
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
	writeJSON(w, http.StatusOK, map[string]bool{"createIssue": canOpen(d.Repo), "createWithMetadata": canOpenWithMetadata(d.Repo), "manageIssues": canManage(d.Repo)})
}

// list returns the viewer's own operations on one repository; other users' operations are never listed.
func (s *Service) list(w http.ResponseWriter, r *http.Request) {
	userID, ok := auth.SignedInUserID(r.Context())
	repoID, idOK := pathID(r, "id")
	if !ok || !idOK {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if _, ok := s.access(w, r, userID, repoID, false); !ok {
		return
	}
	rows, err := s.Queries.ListWriteOperations(r.Context(), db.ListWriteOperationsParams{UserID: userID, RepositoryID: repoID})
	if err != nil {
		s.internal(w, err)
		return
	}
	out := make([]opView, 0, len(rows))
	for _, row := range rows {
		out = append(out, s.view(db.GetWriteOperationRow(row)))
	}
	writeJSON(w, http.StatusOK, map[string]any{"operations": out})
}

// operation loads one of the viewer's operations and rechecks their access to its repository. Another user's id, a
// missing id and lost access all answer 404.
func (s *Service) operation(w http.ResponseWriter, r *http.Request, fresh bool) (int64, db.GetWriteOperationRow, repositories.Decision, bool) {
	userID, ok := auth.SignedInUserID(r.Context())
	opID, idOK := pathID(r, "op")
	if !ok || !idOK {
		http.Error(w, "not found", http.StatusNotFound)
		return 0, db.GetWriteOperationRow{}, repositories.Decision{}, false
	}
	row, err := s.Queries.GetWriteOperation(r.Context(), db.GetWriteOperationParams{ID: opID, UserID: userID})
	if errors.Is(err, pgx.ErrNoRows) {
		http.Error(w, "not found", http.StatusNotFound)
		return 0, row, repositories.Decision{}, false
	}
	if err != nil {
		s.internal(w, err)
		return 0, row, repositories.Decision{}, false
	}
	d, ok := s.access(w, r, userID, row.RepositoryID, fresh)
	return userID, row, d, ok
}

func (s *Service) get(w http.ResponseWriter, r *http.Request) {
	if _, row, _, ok := s.operation(w, r, false); ok {
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
	assignees   []string
	allowed     func(githubapp.Repo) bool // the per-action permission rule
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
	allowed := canOpen
	if len(d.Labels)+len(d.Assignees) > 0 {
		allowed = canOpenWithMetadata
	}
	s.submit(w, r, command{kind: "create", draft: &d, contentHash: contentHash(d.Title, d.Body), assignees: d.Assignees, allowed: allowed})
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
	s.submit(w, r, command{kind: "update", issueNumber: number, draft: &d, assignees: d.Assignees.Add, allowed: canManage})
}

// replay answers a reused idempotency key: the existing operation for the same request, 422 for a different one.
// It reports false when the key is unused.
func (s *Service) replay(w http.ResponseWriter, ctx context.Context, userID int64, key string, hash []byte) bool {
	prior, err := s.Queries.GetWriteOperationByKey(ctx, db.GetWriteOperationByKeyParams{UserID: userID, IdempotencyKey: key})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return false
	case err != nil:
		s.internal(w, err)
	case !bytes.Equal(prior.RequestHash, hash):
		http.Error(w, "idempotency key reused with a different request", http.StatusUnprocessableEntity)
	default:
		s.respond(w, ctx, http.StatusOK, userID, prior.ID)
	}
	return true
}

var errAccessMoved = errors.New("access changed")

// submit records and sends one command: validate access and permission, record the operation as pending, mark it
// unknown, send it with the actor's token, then record what GitHub confirmed.
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

	d, ok := s.access(w, r, userID, repoID, true)
	if !ok {
		return
	}
	// Taken after the check, which may itself record a changed installation list; any later revocation is caught at commit.
	snap, err := accessfence.Take(ctx, s.Pool, userID, repoID)
	if err != nil {
		s.internal(w, err)
		return
	}
	if s.replay(w, ctx, userID, key, hash) {
		return
	}
	if !c.allowed(d.Repo) {
		http.Error(w, "your current GitHub permission does not allow this action", http.StatusForbidden)
		return
	}
	if c.kind == "create" {
		prior, err := s.Queries.FindUnknownCreate(ctx, db.FindUnknownCreateParams{UserID: userID, RepositoryID: repoID, ContentHash: c.contentHash})
		if err == nil {
			writeJSON(w, http.StatusConflict, map[string]any{"error": "an earlier create of this issue has an unknown outcome; recheck it instead of resubmitting", "operationId": prior})
			return
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			s.internal(w, err)
			return
		}
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

	var opID int64
	err = db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		if current, err := snap.StillCurrent(ctx, tx); err != nil {
			return err
		} else if !current {
			return errAccessMoved
		}
		var err error
		opID, err = s.Queries.WithTx(tx).InsertWriteOperation(ctx, db.InsertWriteOperationParams{UserID: userID, RepositoryID: repoID, IdempotencyKey: key, Kind: c.kind,
			IssueNumber: pgtype.Int8{Int64: c.issueNumber, Valid: c.kind == "update"}, RequestHash: hash, ContentHash: c.contentHash, Draft: draft})
		return err
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows): // the same key was recorded concurrently
		if !s.replay(w, ctx, userID, key, hash) {
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
	if n, err := s.Queries.MarkWriteOperationSent(ctx, opID); err != nil || n != 1 {
		s.internal(w, fmt.Errorf("mark sent: %v", err))
		return
	}
	ctx = context.WithoutCancel(ctx) // a client disconnect must not abandon the call or its recorded outcome
	var state string
	var out outcome
	var confirmed *githubapp.Issue
	link := false
	if c.kind == "create" {
		state, out, confirmed = s.sendCreate(ctx, userID, token, d.Repo.FullName, c.draft.(*createDraft))
		link = confirmed != nil
	} else {
		state, out, confirmed = s.sendUpdate(ctx, userID, token, d.Repo.FullName, c.issueNumber, c.draft.(*updateDraft))
	}
	if err := s.finish(ctx, opID, repoID, snap, state, out, confirmed, link); err != nil {
		s.internal(w, err)
		return
	}
	s.respond(w, ctx, http.StatusCreated, userID, opID)
}

// classify maps a failed write: a 4xx is a definite rejection (failed), anything else may have been applied (unknown).
func (s *Service) classify(ctx context.Context, userID int64, err error) (string, string) {
	var api *githubapp.APIError
	if errors.As(err, &api) && api.Status >= 400 && api.Status < 500 {
		if api.Unauthorized() {
			s.revoke(ctx, userID)
		}
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
		state, msg := s.classify(ctx, userID, err)
		return state, outcome{Error: msg}, nil
	}
	out := outcome{Issue: viewIssue(issue)}
	out.Unapplied = unapplied(out.Issue, delta{Add: d.Labels}, delta{Add: d.Assignees}, "")
	return resolved(out), out, &issue
}

// sendUpdate applies add/remove steps (each idempotent), stopping at the first error, then reads the issue back:
// the outcome is whatever the read shows. Without a readable result the operation stays unknown.
func (s *Service) sendUpdate(ctx context.Context, userID int64, token, fullName string, number int64, d *updateDraft) (string, outcome, *githubapp.Issue) {
	steps := []func() error{}
	if len(d.Labels.Add) > 0 {
		steps = append(steps, func() error { return s.GitHub.AddLabels(ctx, token, fullName, number, d.Labels.Add) })
	}
	for _, l := range d.Labels.Remove {
		steps = append(steps, func() error { return s.GitHub.RemoveLabel(ctx, token, fullName, number, l) })
	}
	if len(d.Assignees.Add) > 0 {
		steps = append(steps, func() error { return s.GitHub.AddAssignees(ctx, token, fullName, number, d.Assignees.Add) })
	}
	if len(d.Assignees.Remove) > 0 {
		steps = append(steps, func() error { return s.GitHub.RemoveAssignees(ctx, token, fullName, number, d.Assignees.Remove) })
	}
	if d.State != "" {
		steps = append(steps, func() error { return s.GitHub.SetIssueState(ctx, token, fullName, number, d.State) })
	}
	var out outcome
	for _, step := range steps {
		if err := step(); err != nil {
			_, out.Error = s.classify(ctx, userID, err)
			break
		}
	}
	issue, err := s.GitHub.FetchIssue(ctx, token, fullName, number)
	if err != nil {
		_, msg := s.classify(ctx, userID, err)
		out.Error = strings.TrimSpace(out.Error + " Reading the result failed: " + msg)
		return "unknown", out, nil
	}
	out.Issue = viewIssue(issue)
	out.Unapplied = unapplied(out.Issue, d.Labels, d.Assignees, d.State)
	return resolved(out), out, &issue
}

var errAlreadyLinked = errors.New("issue already linked to another operation")

// finish records the outcome of an unknown operation and, when the access fence still holds, projects the confirmed
// issue locally. link records a create's issue number.
func (s *Service) finish(ctx context.Context, opID, repoID int64, snap accessfence.Snapshot, state string, out outcome, confirmed *githubapp.Issue, link bool) error {
	body, err := json.Marshal(out)
	if err != nil {
		return err
	}
	err = db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		number := pgtype.Int8{}
		if link {
			number = pgtype.Int8{Int64: confirmed.Number, Valid: true}
		}
		n, err := s.Queries.WithTx(tx).FinishWriteOperation(ctx, db.FinishWriteOperationParams{ID: opID, State: state, Outcome: body, IssueNumber: number})
		if err != nil || n == 0 || confirmed == nil {
			return err
		}
		if current, err := snap.StillCurrent(ctx, tx); err != nil || !current {
			return err // access moved: keep the recorded outcome, let the next sync project the issue
		}
		return ingestion.ProjectIssue(ctx, tx, repoID, *confirmed)
	})
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return errAlreadyLinked
	}
	return err
}

// recheck resolves an unknown operation without resending it. A create needs the issue number the actor found on
// GitHub; it is linked only when that issue is in this repository, authored by the actor, and has the drafted
// title and body. Anything else keeps the operation unknown (409) with its draft. An update is re-read and verified.
func (s *Service) recheck(w http.ResponseWriter, r *http.Request) {
	userID, row, d, ok := s.operation(w, r, true)
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
	snap, err := accessfence.Take(ctx, s.Pool, userID, row.RepositoryID)
	if err != nil {
		s.internal(w, err)
		return
	}
	token, ok := s.token(w, ctx, userID)
	if !ok {
		return
	}
	issue, err := s.GitHub.FetchIssue(ctx, token, d.Repo.FullName, number)
	var api *githubapp.APIError
	mismatch := func() { writeJSON(w, http.StatusConflict, s.view(row)) }
	switch {
	case errors.As(err, &api) && api.NotFound():
		mismatch()
		return
	case err != nil:
		s.upstream(w, ctx, userID, err)
		return
	}
	var out outcome
	out.Issue = viewIssue(issue)
	link := false
	if row.Kind == "create" {
		if issue.PullRequest != nil || issue.User == nil || issue.User.ID != row.ActorGithubID ||
			!strings.EqualFold(issue.RepoURL, s.GitHub.APIURL+"/repos/"+d.Repo.FullName) ||
			!bytes.Equal(contentHash(issue.Title, issue.Body), row.ContentHash) ||
			issue.CreatedAt.Before(row.CreatedAt.Time.Add(-time.Minute)) {
			mismatch()
			return
		}
		// ponytail: a wiped draft leaves no labels/assignees to verify, so only the content match decides.
		var draft createDraft
		if row.Draft != nil {
			if err := json.Unmarshal(row.Draft, &draft); err != nil {
				s.internal(w, err)
				return
			}
		}
		out.Unapplied = unapplied(out.Issue, delta{Add: draft.Labels}, delta{Add: draft.Assignees}, "")
		link = true
	} else {
		var draft updateDraft
		if row.Draft == nil || json.Unmarshal(row.Draft, &draft) != nil {
			mismatch() // nothing left to verify against
			return
		}
		out.Unapplied = unapplied(out.Issue, draft.Labels, draft.Assignees, draft.State)
	}
	err = s.finish(context.WithoutCancel(ctx), row.ID, row.RepositoryID, snap, resolved(out), out, &issue, link)
	if errors.Is(err, errAlreadyLinked) {
		mismatch()
		return
	}
	if err != nil {
		s.internal(w, err)
		return
	}
	s.respond(w, ctx, http.StatusOK, userID, row.ID)
}

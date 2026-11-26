// Package issues runs user-initiated GitHub issue writes (create, label/assignee add/remove, close/reopen) as audited
// operations. Every write is recorded before it is sent, uses only the actor's own GitHub token, rechecks live access
// and permission, and reports what GitHub actually did, never what was assumed. An ambiguous create is never resent.
package issues

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/repoorbit/repoorbit/backend/internal/accessfence"
	"github.com/repoorbit/repoorbit/backend/internal/auth"
	"github.com/repoorbit/repoorbit/backend/internal/db"
	"github.com/repoorbit/repoorbit/backend/internal/githubapp"
	"github.com/repoorbit/repoorbit/backend/internal/ingestion"
	"github.com/repoorbit/repoorbit/backend/internal/issues/domain"
	"github.com/repoorbit/repoorbit/backend/internal/repositories"
)

const (
	headerWriteDisposition           = "RepoOrbit-Write-Disposition"
	writeDispositionNotRecorded      = "not-recorded"
	writeDispositionPossiblyRecorded = "possibly-recorded"
)

type submitWriter struct {
	http.ResponseWriter
	disposition string
}

func newSubmitWriter(w http.ResponseWriter) *submitWriter {
	return &submitWriter{ResponseWriter: w, disposition: writeDispositionNotRecorded}
}

func (sw *submitWriter) WriteHeader(code int) {
	if code >= http.StatusBadRequest && sw.disposition != "" {
		if code != http.StatusInternalServerError || sw.disposition == writeDispositionPossiblyRecorded {
			sw.Header().Set(headerWriteDisposition, sw.disposition)
		}
	}
	sw.ResponseWriter.WriteHeader(code)
}

// Service serves the issue write API. Mount Register's routes behind auth.Handler.Protect (Origin + CSRF checks).
type Service struct {
	Pool    *pgxpool.Pool
	Queries *db.Queries
	GitHub  *githubapp.Client
	// Access decides live repository access and permissions; its TokenSource supplies the actor's own user token.
	// Installation credentials are never used for these writes.
	Access    *repositories.Service
	GitHubURL string
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

type createDraft = domain.CreateDraft
type updateDraft = domain.UpdateDraft
type delta = domain.Delta
type issueView = domain.IssueView
type outcome = domain.Outcome

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

func domainRepo(r githubapp.Repo) domain.Repo {
	p := r.Permissions
	return domain.Repo{HasIssues: r.HasIssues, Archived: r.Archived, Permissions: domain.RepoPermissions{Admin: p.Admin, Maintain: p.Maintain, Push: p.Push, Triage: p.Triage, Pull: p.Pull}}
}
func canCreate(r githubapp.Repo) bool             { return domain.CanCreate(domainRepo(r)) }
func canCreateWithMetadata(r githubapp.Repo) bool { return domain.CanCreateWithMetadata(domainRepo(r)) }
func canManage(r githubapp.Repo) bool             { return domain.CanManage(domainRepo(r)) }

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
		w.Header().Set(headerWriteDisposition, writeDispositionPossiblyRecorded)
		s.internal(w, err)
		return
	}
	current, err := snap.Current(ctx, s.Pool)
	if err != nil {
		w.Header().Set(headerWriteDisposition, writeDispositionPossiblyRecorded)
		s.internal(w, err)
		return
	}
	if !current {
		w.Header().Set(headerWriteDisposition, writeDispositionPossiblyRecorded)
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
	if err := d.Validate(); err != nil {
		http.Error(sw, err.Error(), http.StatusBadRequest)
		return
	}
	allowed := canCreate
	if len(d.Labels)+len(d.Assignees) > 0 {
		allowed = canCreateWithMetadata
	}
	s.submit(sw, r, command{kind: "create", draft: &d, contentHash: domain.ContentHash(d.Title, d.Body), hasMetadata: len(d.Labels)+len(d.Assignees) > 0, assignees: d.Assignees, allowed: allowed})
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
	if err := d.Validate(); err != nil {
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
		sw.disposition = writeDispositionNotRecorded
		http.Error(sw, "idempotency key reused with a different request", http.StatusUnprocessableEntity)
	default:
		sw.disposition = writeDispositionPossiblyRecorded
		if prior.StalePending.Bool {
			if err := s.Queries.FailStalePendingOperationByID(ctx, prior.ID); err != nil {
				s.internal(sw, err)
				return true
			}
		}
		s.respond(sw, ctx, http.StatusOK, userID, prior.ID, snap)
	}
	return true
}

func mapConcurrent[T, R any](ctx context.Context, items []T, fn func(context.Context, T) (R, error)) ([]R, error) {
	if len(items) == 0 {
		return nil, nil
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make([]R, len(items))
	errs := make([]error, len(items))
	var wg sync.WaitGroup
	for i, item := range items {
		wg.Add(1)
		go func(i int, item T) {
			defer wg.Done()
			res, err := fn(ctx, item)
			if err != nil {
				errs[i] = err
				cancel()
				return
			}
			results[i] = res
		}(i, item)
	}
	wg.Wait()
	var firstErr error
	for _, err := range errs {
		if err == nil {
			continue
		}
		var api *githubapp.APIError
		if errors.As(err, &api) && api.Unauthorized() {
			return nil, err
		}
		if firstErr == nil || (errors.Is(firstErr, context.Canceled) && !errors.Is(err, context.Canceled)) {
			firstErr = err
		}
	}
	if firstErr != nil {
		return nil, firstErr
	}
	return results, nil
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
	if !domain.ValidKey(key) {
		http.Error(sw, "an Idempotency-Key header of 1 to 128 letters, digits, or ._:- is required", http.StatusBadRequest)
		return
	}
	draft, err := json.Marshal(c.draft)
	if err != nil {
		s.internal(sw, err)
		return
	}
	hash := domain.RequestHash(c.kind, repoID, c.issueNumber, draft)

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
			if live := viewIssue(issue); !e.Matches(live) {
				writeJSON(sw, http.StatusConflict, map[string]any{"error": "changed on GitHub", "current": map[string]any{"state": live.State,
					"labels": live.Labels, "assignees": live.Assignees, "status": domain.BoardStatus(live.State, live.Labels), "updatedAt": issue.UpdatedAt}})
				return
			}
		}
	}
	assignable, err := mapConcurrent(ctx, c.assignees, func(ctx context.Context, login string) (bool, error) {
		return s.GitHub.IsAssignable(ctx, token, d.Repo.FullName, login)
	})
	if err != nil {
		s.upstream(sw, ctx, userID, err)
		return
	}
	var ineligible []string
	for i, login := range c.assignees {
		if !assignable[i] {
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
		sw.disposition = writeDispositionNotRecorded
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
		sw.disposition = writeDispositionNotRecorded
		http.Error(sw, "access changed while submitting; nothing was sent", http.StatusServiceUnavailable)
		return
	case err != nil:
		s.internal(sw, err)
		return
	}
	sw.disposition = writeDispositionPossiblyRecorded
	if n, err := s.Queries.MarkWriteOperationSent(ctx, opID); err != nil {
		s.internal(sw, fmt.Errorf("mark sent: %w", err))
		return
	} else if n == 0 {
		if err := s.Queries.FailStalePendingOperationByID(ctx, opID); err != nil {
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

// classify maps a GitHub client error onto a domain error kind and returns domain.Classify's state and reason.
func classify(err error) (string, string) {
	if errors.Is(err, githubapp.ErrNotSent) {
		return domain.Classify(domain.NotSent, 0)
	}
	var api *githubapp.APIError
	switch {
	case !errors.As(err, &api):
		return domain.Classify(domain.OtherError, 0)
	case api.Unauthorized():
		return domain.Classify(domain.Unauthorized, api.Status)
	case api.NotFound():
		return domain.Classify(domain.NotFound, api.Status)
	}
	return domain.Classify(domain.HTTPError, api.Status)
}

func viewIssue(issue githubapp.Issue) *issueView {
	labels := make([]string, len(issue.Labels))
	for i, l := range issue.Labels {
		labels[i] = l.Name
	}
	assignees := make([]string, len(issue.Assignees))
	for i, a := range issue.Assignees {
		assignees[i] = a.Login
	}
	return domain.NewIssueView(issue.Number, issue.HTMLURL, issue.Title, issue.State, labels, assignees)
}

func (s *Service) sendCreate(ctx context.Context, userID int64, token, fullName string, d *createDraft) (string, outcome, *githubapp.Issue) {
	issue, err := s.GitHub.CreateIssue(ctx, token, fullName, githubapp.NewIssue{Title: d.Title, Body: d.Body, Labels: d.Labels, Assignees: d.Assignees})
	if err != nil {
		s.revokeIfUnauthorized(ctx, userID, err)
		state, msg := classify(err)
		return state, outcome{Error: msg}, nil
	}
	out := outcome{Issue: viewIssue(issue)}
	out.Unapplied = domain.Unapplied(out.Issue, delta{Add: d.Labels}, delta{Add: d.Assignees}, "")
	return domain.Resolved(out), out, &issue
}

// applyUpdate applies add/remove steps (each idempotent), stopping at the first error; readBack then decides the
// outcome. Assignees are removed before they are added, so a swap at GitHub's 10-assignee limit fits. A reopen is the
// first step and a close the last, so a change that reopens or closes an issue never adds a label while it is still
// closed, where the app's status label cleanup could strip it; a rejected reopen applies nothing.
func (s *Service) applyUpdate(ctx context.Context, userID int64, token, fullName string, number int64, d *updateDraft) outcome {
	out := outcome{AllStepsAnswered: true}
	for _, step := range domain.PlanUpdate(*d) {
		var err error
		switch step.Kind {
		case domain.Reopen, domain.Close:
			err = s.GitHub.SetIssueState(ctx, token, fullName, number, d.State)
		case domain.AddLabels:
			err = s.GitHub.AddLabels(ctx, token, fullName, number, step.Names)
		case domain.RemoveLabel:
			err = s.GitHub.RemoveLabel(ctx, token, fullName, number, step.Names[0])
		case domain.RemoveAssignees:
			err = s.GitHub.RemoveAssignees(ctx, token, fullName, number, step.Names)
		case domain.AddAssignees:
			err = s.GitHub.AddAssignees(ctx, token, fullName, number, step.Names)
		}
		if err != nil {
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
	out.Unapplied = domain.Unapplied(out.Issue, d.Labels, d.Assignees, d.State)
	if len(out.Unapplied) > 0 && !out.AllStepsAnswered {
		return "unknown", out, &issue, nil
	}
	return domain.Resolved(out), out, &issue, nil
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
				!bytes.Equal(domain.ContentHash(issue.Title, issue.Body), row.ContentHash) ||
				!row.SentAt.Valid || issue.CreatedAt.Before(row.SentAt.Time.Add(-sendSkew)) {
				return "", outcome{}, nil, errMismatch
			}
			var draft createDraft
			switch {
			case row.Draft != nil:
				if err := json.Unmarshal(row.Draft, &draft); err != nil {
					return "", outcome{}, nil, err
				}
				out.Unapplied = domain.Unapplied(out.Issue, delta{Add: draft.Labels}, delta{Add: draft.Assignees}, "")
			case row.HasRequestedMetadata:
				out.Unapplied = []string{"requested labels or assignees (unverifiable: the draft was removed)"}
			}
		} else {
			var draft updateDraft
			var sent outcome
			if row.Draft == nil || json.Unmarshal(row.Draft, &draft) != nil || json.Unmarshal(row.Outcome, &sent) != nil {
				return "", outcome{}, nil, errMismatch
			}
			out.Unapplied = domain.Unapplied(out.Issue, draft.Labels, draft.Assignees, draft.State)
			if len(out.Unapplied) > 0 && !sent.AllStepsAnswered {
				return "", outcome{}, nil, errMismatch
			}
			out.Error, out.AllStepsAnswered = sent.Error, sent.AllStepsAnswered
		}
		return domain.Resolved(out), out, &issue, nil
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

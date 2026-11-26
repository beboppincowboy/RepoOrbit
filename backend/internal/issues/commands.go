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
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/repoorbit/repoorbit/backend/internal/accessfence"
	"github.com/repoorbit/repoorbit/backend/internal/auth"
	"github.com/repoorbit/repoorbit/backend/internal/db"
	"github.com/repoorbit/repoorbit/backend/internal/githubapp"
	"github.com/repoorbit/repoorbit/backend/internal/issues/application"
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

// access answers a repository's live access check for a capability read, writing the 404/503 itself when it fails.
func (s *Service) access(w http.ResponseWriter, r *http.Request, userID, repoID int64, fresh bool) (repositories.Decision, bool) {
	d, decision, err := accessAdapter{s}.check(r.Context(), userID, repoID, fresh)
	return d, s.accessAllowed(w, d, decision, err)
}

func (s *Service) checkedAccess(w http.ResponseWriter, r *http.Request, userID, repoID int64) (repositories.Decision, accessfence.Snapshot, bool) {
	d, decision, snap, err := accessAdapter{s}.checked(r.Context(), userID, repoID)
	return d, snap, s.accessAllowed(w, d, decision, err)
}

func (s *Service) accessAllowed(w http.ResponseWriter, d repositories.Decision, decision application.AccessDecision, err error) bool {
	switch {
	case err != nil:
		s.internal(w, err)
	case decision == application.AccessNotFound:
		http.Error(w, "not found", http.StatusNotFound)
	case decision == application.AccessUnverified:
		if !d.RetryAt.IsZero() {
			w.Header().Set("Retry-After", strconv.Itoa(int(time.Until(d.RetryAt).Seconds())+1))
		}
		http.Error(w, "access could not be verified; retry later", http.StatusServiceUnavailable)
	case decision == application.AccessMoved:
		http.Error(w, "access changed while it was being checked; retry", http.StatusServiceUnavailable)
	default:
		return true
	}
	return false
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
	token, err := accessAdapter{s}.Token(ctx, userID)
	switch {
	case errors.Is(err, application.ErrSignInRequired):
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
	accessAdapter{s}.Revoke(ctx, userID)
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
	writeJSON(w, status, s.view(toOperation(row)))
}

type command struct {
	kind    string
	draft   any
	allowed func(githubapp.Repo) bool
}

// create answers POST /api/repositories/{id}/issues: it parses the request, runs application.CreateIssue and maps its
// result.
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
	userID, ok := auth.SignedInUserID(r.Context())
	repoID, idOK := pathID(r, "id")
	if !ok || !idOK {
		http.Error(sw, "not found", http.StatusNotFound)
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if !domain.ValidKey(key) {
		http.Error(sw, idempotencyKeyRequired, http.StatusBadRequest)
		return
	}
	s.writeResult(w, s.commands().CreateIssue(r.Context(), application.CreateIssueInput{UserID: userID, RepoID: repoID, Key: key, Draft: d}))
}

// update answers POST /api/repositories/{id}/issues/{number}/updates: it parses the request, runs
// application.UpdateIssue and maps its result.
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
	userID, ok := auth.SignedInUserID(r.Context())
	repoID, idOK := pathID(r, "id")
	if !ok || !idOK {
		http.Error(sw, "not found", http.StatusNotFound)
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if !domain.ValidKey(key) {
		http.Error(sw, idempotencyKeyRequired, http.StatusBadRequest)
		return
	}
	s.writeResult(w, s.commands().UpdateIssue(r.Context(), application.UpdateIssueInput{UserID: userID, RepoID: repoID, Number: number, Key: key, Draft: d}))
}

func (s *Service) commands() *application.Commands {
	return &application.Commands{Access: accessAdapter{s}, GitHub: githubGateway{s.GitHub}, Store: operationStore{s}}
}

// writeResult maps a use case's result to today's status code, body and RepoOrbit-Write-Disposition header.
func (s *Service) writeResult(w http.ResponseWriter, res application.Result) {
	sw := &submitWriter{ResponseWriter: w, disposition: string(res.Disposition)}
	text := func(status int, msg string) { http.Error(sw, msg, status) }
	switch res.Kind {
	case application.ResultOperation:
		status := http.StatusOK
		if res.Created {
			status = http.StatusCreated
			if res.Operation.State == "unknown" {
				status = http.StatusAccepted
			}
		}
		writeJSON(sw, status, s.view(res.Operation))
	case application.ResultMismatch:
		writeJSON(sw, http.StatusConflict, s.view(res.Operation))
	case application.ResultNoContent:
		sw.WriteHeader(http.StatusNoContent)
	case application.ResultNotFound:
		text(http.StatusNotFound, "not found")
	case application.ResultIssueNotFound:
		text(http.StatusNotFound, "issue not found")
	case application.ResultIssueNumberRequired:
		text(http.StatusBadRequest, "issueNumber is required to recheck a create")
	case application.ResultAccessUnverified:
		if !res.RetryAt.IsZero() {
			sw.Header().Set("Retry-After", strconv.Itoa(int(time.Until(res.RetryAt).Seconds())+1))
		}
		text(http.StatusServiceUnavailable, "access could not be verified; retry later")
	case application.ResultAccessCheckMoved:
		text(http.StatusServiceUnavailable, "access changed while it was being checked; retry")
	case application.ResultLoadAccessMoved:
		text(http.StatusServiceUnavailable, "access changed while loading; retry")
	case application.ResultSubmitAccessMoved:
		text(http.StatusServiceUnavailable, "access changed while submitting; nothing was sent")
	case application.ResultRecordedAccessMoved:
		text(http.StatusServiceUnavailable, "access changed; the operation was recorded and may have been sent: retry with the same Idempotency-Key")
	case application.ResultDisableAccessMoved:
		text(http.StatusServiceUnavailable, "access changed; retry")
	case application.ResultKeyReuse:
		text(http.StatusUnprocessableEntity, "idempotency key reused with a different request")
	case application.ResultForbidden:
		text(http.StatusForbidden, "your current GitHub permission does not allow this action")
	case application.ResultStatusLabelsForbidden:
		text(http.StatusForbidden, "only Admin or Maintain users can turn status labels off")
	case application.ResultSignInRequired:
		text(http.StatusUnauthorized, "sign in required")
	case application.ResultCredentialUnavailable:
		text(http.StatusServiceUnavailable, "GitHub credential unavailable; retry later")
	case application.ResultUpstream:
		text(http.StatusServiceUnavailable, "GitHub could not be reached; nothing was sent")
	case application.ResultConflict:
		c := res.Current
		writeJSON(sw, http.StatusConflict, map[string]any{"error": "changed on GitHub", "current": map[string]any{"state": c.State,
			"labels": c.Labels, "assignees": c.Assignees, "status": c.Status, "updatedAt": c.UpdatedAt}})
	case application.ResultUnassignable:
		text(http.StatusUnprocessableEntity, "cannot be assigned in this repository: "+strings.Join(res.Logins, ", "))
	case application.ResultDuplicateCreate:
		writeJSON(sw, http.StatusConflict, map[string]any{"error": res.Message, "operationId": res.OperationID})
	case application.ResultCreateChanged:
		text(http.StatusServiceUnavailable, "an earlier create of this issue changed while submitting; nothing was sent, retry")
	default:
		err := res.Err
		if res.Kind != application.ResultInternal {
			err = fmt.Errorf("unmapped result kind %d", res.Kind)
		}
		s.internal(sw, err)
	}
}

const idempotencyKeyRequired = "an Idempotency-Key header of 1 to 128 letters, digits, or ._:- is required"

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
		http.Error(sw, idempotencyKeyRequired, http.StatusBadRequest)
		return
	}
	draft, err := json.Marshal(c.draft)
	if err != nil {
		s.internal(sw, err)
		return
	}
	hash := domain.RequestHash(c.kind, repoID, 0, draft)

	d, snap, ok := s.checkedAccess(sw, r, userID, repoID)
	if !ok {
		return
	}
	if s.replay(sw, ctx, userID, key, hash, snap) {
		return
	}
	if !c.allowed(d.Repo) {
		http.Error(sw, "your current GitHub permission does not allow this action", http.StatusForbidden)
		return
	}
	token, ok := s.token(sw, ctx, userID)
	if !ok {
		return
	}
	sw.disposition = ""
	ctx = context.WithoutCancel(ctx)
	opID, reserved, err := operationStore{s}.Reserve(ctx, snap, application.NewOperation{UserID: userID, RepositoryID: repoID, Key: key, Kind: c.kind,
		RequestHash: hash, Draft: draft})
	switch {
	case err != nil:
		s.internal(sw, err)
		return
	case reserved == application.ReserveRace:
		if !s.replay(sw, ctx, userID, key, hash, snap) {
			s.internal(sw, errors.New("idempotency key vanished"))
		}
		return
	case reserved == application.ReserveAccessMoved:
		sw.disposition = writeDispositionNotRecorded
		http.Error(sw, "access changed while submitting; nothing was sent", http.StatusServiceUnavailable)
		return
	case reserved != application.Reserved:
		// A status_labels operation carries no content hash, so it cannot hit the unresolved-create constraint.
		s.internal(sw, fmt.Errorf("unexpected reserve result %d for %s", reserved, c.kind))
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
	state, out := s.ensureStatusLabels(ctx, userID, token, d.Repo.FullName)
	if err := s.finishStatusLabels(ctx, opID, repoID, snap, state, out); err != nil {
		s.internal(sw, err)
		return
	}
	s.respond(sw, ctx, http.StatusCreated, userID, opID, snap)
}

var errAlreadyLinked = application.ErrAlreadyLinked

var errMismatch = errors.New("not confirmed")

// finish settles an operation under the issue lock through the store (see application.OperationStore.SettleUnderLock).
func (s *Service) finish(ctx context.Context, opID, repoID int64, snap accessfence.Snapshot, lock int64, link bool, read func() (string, outcome, *githubapp.Issue, error)) error {
	return operationStore{s}.SettleUnderLock(ctx, application.Settle{OperationID: opID, RepositoryID: repoID, Fence: snap, LockNumber: lock, Link: link},
		func() (application.Settlement, error) {
			state, out, confirmed, err := read()
			if err != nil {
				return application.Settlement{}, err
			}
			res := application.Settlement{State: state, Outcome: out}
			if confirmed != nil {
				res.Confirmed = *confirmed
			}
			return res, nil
		})
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
		writeJSON(w, http.StatusOK, s.view(toOperation(row)))
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
			writeJSON(w, http.StatusConflict, s.view(toOperation(row)))
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

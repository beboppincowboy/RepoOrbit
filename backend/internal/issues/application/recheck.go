package application

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/repoorbit/repoorbit/backend/internal/issues/domain"
)

// sendSkew is how much earlier than the recorded send a created issue's creation time may be (clock skew).
const sendSkew = 5 * time.Second

// recheckGrace is how long after sending a status-label recheck keeps an operation with a missing label unknown. It
// compares the database sent_at with the app clock, as it always has.
const recheckGrace = 2 * time.Minute

// errMismatch aborts a recheck settlement whose read could not confirm the operation.
var errMismatch = errors.New("not confirmed")

// RecheckOperationInput is a recheck request. The HTTP adapter has already decoded the body strictly and checked the
// signed-in user and the path id; IssueNumber is the body's issueNumber (0 when absent), which only a create needs.
type RecheckOperationInput struct {
	UserID      int64
	OperationID int64
	IssueNumber int64
}

// RecheckOperation reads GitHub again for an unknown operation and settles what it shows. Order: load (scope →
// checked access → fail stale → get → fence) → a state other than unknown is answered as is → status labels take the
// status-label recheck → a create needs IssueNumber → token → settle under the issue lock, reading with the request's
// context → 409 with the unchanged operation when the read cannot confirm it. Only respond's results carry a
// disposition; every other recheck answer carries none.
func (c *Commands) RecheckOperation(ctx context.Context, in RecheckOperationInput) Result {
	res := c.recheck(ctx, in)
	if res.Disposition == DispositionNotRecorded {
		res.Disposition = DispositionNone
	}
	return res
}

func (c *Commands) recheck(ctx context.Context, in RecheckOperationInput) Result {
	op, a, res, ok := c.loadOperation(ctx, in.UserID, in.OperationID)
	if !ok {
		return res
	}
	if op.State != "unknown" {
		return Result{Kind: ResultOperation, Operation: op}
	}
	if op.Kind == "status_labels" {
		return c.recheckStatusLabels(ctx, in.UserID, op, a)
	}
	number := op.IssueNumber
	if op.Kind == "create" {
		if in.IssueNumber <= 0 {
			return Result{Kind: ResultIssueNumberRequired}
		}
		number = in.IssueNumber
	}
	token, res, ok := c.token(ctx, in.UserID)
	if !ok {
		return res
	}
	var fetchErr error
	err := c.Store.SettleUnderLock(context.WithoutCancel(ctx), Settle{OperationID: op.ID, RepositoryID: op.RepositoryID, Fence: a.Fence,
		LockNumber: number, Link: op.Kind == "create"}, func() (Settlement, error) {
		issue, err := c.GitHub.FetchIssue(ctx, token, a.Repo.FullName, number)
		switch {
		case remoteKind(err, domain.NotFound):
			return Settlement{}, errMismatch
		case err != nil:
			fetchErr = err
			return Settlement{}, err
		}
		return confirmRecheck(op, issue)
	})
	switch {
	case fetchErr != nil:
		return c.upstream(ctx, in.UserID, fetchErr)
	case errors.Is(err, errMismatch), errors.Is(err, ErrAlreadyLinked):
		return c.mismatch(ctx, op, a.Fence)
	case err != nil:
		return internalResult(DispositionNone, err)
	}
	return c.respond(ctx, in.UserID, op.ID, a.Fence, false)
}

// confirmRecheck decides what the fetched issue confirms about an unknown create or update; errMismatch when it
// cannot confirm it.
func confirmRecheck(op Operation, issue Issue) (Settlement, error) {
	out := domain.Outcome{Issue: issue.View}
	if op.Kind == "create" {
		if issue.PullRequest || issue.AuthorID == 0 || issue.AuthorID != op.ActorGithubID || !issue.InRepository ||
			!bytes.Equal(domain.ContentHash(issue.View.Title, issue.Body), op.ContentHash) ||
			op.SentAt.IsZero() || issue.CreatedAt.Before(op.SentAt.Add(-sendSkew)) {
			return Settlement{}, errMismatch
		}
		var draft domain.CreateDraft
		switch {
		case op.Draft != nil:
			if err := json.Unmarshal(op.Draft, &draft); err != nil {
				return Settlement{}, err
			}
			out.Unapplied = domain.Unapplied(out.Issue, domain.Delta{Add: draft.Labels}, domain.Delta{Add: draft.Assignees}, "")
		case op.HasRequestedMetadata:
			out.Unapplied = []string{"requested labels or assignees (unverifiable: the draft was removed)"}
		}
	} else {
		var draft domain.UpdateDraft
		var sent domain.Outcome
		if op.Draft == nil || json.Unmarshal(op.Draft, &draft) != nil || json.Unmarshal(op.Outcome, &sent) != nil {
			return Settlement{}, errMismatch
		}
		out.Unapplied = domain.Unapplied(out.Issue, draft.Labels, draft.Assignees, draft.State)
		if len(out.Unapplied) > 0 && !sent.AllStepsAnswered {
			return Settlement{}, errMismatch
		}
		out.Error, out.AllStepsAnswered = sent.Error, sent.AllStepsAnswered
	}
	return Settlement{State: domain.Resolved(out), Outcome: out, Confirmed: issue.Payload}, nil
}

// loadOperation loads the user's operation for a recheck: scope → checked access on its repository → fail it when
// stale pending → get → fence still current.
func (c *Commands) loadOperation(ctx context.Context, userID, opID int64) (Operation, Access, Result, bool) {
	repoID, stale, found, err := c.Store.Scope(ctx, opID, userID)
	switch {
	case err != nil:
		return Operation{}, Access{}, internalResult(DispositionNone, err), false
	case !found:
		return Operation{}, Access{}, Result{Kind: ResultNotFound}, false
	}
	a, res, ok := c.checkedAccess(ctx, userID, repoID)
	if !ok {
		return Operation{}, a, res, false
	}
	if stale {
		if err := c.Store.FailStale(ctx, opID); err != nil {
			return Operation{}, a, internalResult(DispositionNone, err), false
		}
	}
	op, found, err := c.Store.Get(ctx, opID, userID)
	switch {
	case err != nil:
		return op, a, internalResult(DispositionNone, err), false
	case !found:
		return op, a, Result{Kind: ResultNotFound}, false
	}
	if res, ok := c.fenceHolds(ctx, a.Fence); !ok {
		return op, a, res, false
	}
	return op, a, Result{}, true
}

// fenceHolds answers access moved (or an internal error) unless the fence still holds.
func (c *Commands) fenceHolds(ctx context.Context, fence Fence) (Result, bool) {
	current, err := c.Access.FenceCurrent(ctx, fence)
	switch {
	case err != nil:
		return internalResult(DispositionNone, err), false
	case !current:
		return Result{Kind: ResultLoadAccessMoved}, false
	}
	return Result{}, true
}

// mismatch answers a recheck that could not confirm the operation with the unchanged operation, while the fence holds.
func (c *Commands) mismatch(ctx context.Context, op Operation, fence Fence) Result {
	if res, ok := c.fenceHolds(ctx, fence); !ok {
		return res
	}
	return Result{Kind: ResultMismatch, Operation: op}
}

// recheckStatusLabels reads the three status labels again. A missing label within recheckGrace of sending leaves the
// operation unknown (409 with the unchanged operation); otherwise the read is settled.
func (c *Commands) recheckStatusLabels(ctx context.Context, userID int64, op Operation, a Access) Result {
	if !domain.CanChangeStatusLabels(a.Repo.Repo) {
		return Result{Kind: ResultForbidden}
	}
	token, res, ok := c.token(ctx, userID)
	if !ok {
		return res
	}
	var sent domain.Outcome
	_ = json.Unmarshal(op.Outcome, &sent)
	state, out := c.ReadStatusLabels(ctx, token, a.Repo.FullName, domain.Outcome{Created: sent.Created})
	if state == "unknown" {
		return c.upstream(ctx, userID, errors.New("label read failed"))
	}
	if state == "failed" && !op.SentAt.IsZero() && time.Since(op.SentAt) < recheckGrace {
		return c.mismatch(ctx, op, a.Fence)
	}
	if err := c.Store.SettleStatusLabels(context.WithoutCancel(ctx), op.ID, op.RepositoryID, a.Fence, state, out); err != nil {
		return internalResult(DispositionNone, err)
	}
	return c.respond(ctx, userID, op.ID, a.Fence, false)
}

// ReadStatusLabels checks, concurrently, that each status label exists and returns the state and outcome that read
// shows: "unknown" with the read error appended to out.Error when GitHub could not answer, else a missing label is
// listed as unapplied and fails. It never revokes the token.
func (c *Commands) ReadStatusLabels(ctx context.Context, token, fullName string, out domain.Outcome) (string, domain.Outcome) {
	out.Unapplied = nil
	exists, err := MapConcurrent(ctx, domain.StatusLabels, func(ctx context.Context, l domain.StatusLabel) (bool, error) {
		return c.GitHub.LabelExists(ctx, token, fullName, l.Name)
	})
	if err != nil {
		_, msg := classify(err)
		out.Error = strings.TrimSpace(out.Error + " Reading the labels back failed: " + msg)
		return "unknown", out
	}
	for i, l := range domain.StatusLabels {
		if !exists[i] {
			out.Unapplied = append(out.Unapplied, "create label "+l.Name)
		}
	}
	return domain.Resolved(out), out
}

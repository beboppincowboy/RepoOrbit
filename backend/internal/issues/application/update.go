package application

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/repoorbit/repoorbit/backend/internal/issues/domain"
)

// Commands runs the Issue write use cases on the ports package issues implements.
type Commands struct {
	Access AccessChecker
	GitHub IssueGateway
	Store  OperationStore
}

// UpdateIssueInput is a validated update request. The HTTP adapter has already checked the signed-in user, the path
// ids and the Idempotency-Key format.
type UpdateIssueInput struct {
	UserID int64
	RepoID int64
	Number int64
	Key    string
	Draft  domain.UpdateDraft
}

// classify maps a gateway error onto domain.Classify's state and reason; anything that is not a *RemoteError has no
// reliable answer.
func classify(err error) (string, string) {
	var re *RemoteError
	if errors.As(err, &re) {
		return domain.Classify(re.Kind, re.Status)
	}
	return domain.Classify(domain.OtherError, 0)
}

func remoteKind(err error, kind domain.ErrorKind) bool {
	var re *RemoteError
	return errors.As(err, &re) && re.Kind == kind
}

func internalResult(d Disposition, err error) Result {
	return Result{Kind: ResultInternal, Disposition: d, Err: err}
}

// revokeIfUnauthorized drops the user's token when GitHub answered 401 and reports whether it did.
func (c *Commands) revokeIfUnauthorized(ctx context.Context, userID int64, err error) bool {
	if !unauthorized(err) {
		return false
	}
	c.Access.Revoke(ctx, userID)
	return true
}

// upstream answers a GitHub failure before anything was recorded.
func (c *Commands) upstream(ctx context.Context, userID int64, err error) Result {
	if c.revokeIfUnauthorized(ctx, userID, err) {
		return Result{Kind: ResultSignInRequired, Disposition: DispositionNotRecorded}
	}
	return Result{Kind: ResultUpstream, Disposition: DispositionNotRecorded}
}

// checkedAccess turns a checked access into a result unless it allowed the user.
func (c *Commands) checkedAccess(ctx context.Context, userID, repoID int64) (Access, Result, bool) {
	a, err := c.Access.CheckedAccess(ctx, userID, repoID)
	switch {
	case err != nil:
		return a, internalResult(DispositionNotRecorded, err), false
	case a.Decision == AccessNotFound:
		return a, Result{Kind: ResultNotFound, Disposition: DispositionNotRecorded}, false
	case a.Decision == AccessUnverified:
		return a, Result{Kind: ResultAccessUnverified, Disposition: DispositionNotRecorded, RetryAt: a.RetryAt}, false
	case a.Decision == AccessMoved:
		return a, Result{Kind: ResultAccessCheckMoved, Disposition: DispositionNotRecorded}, false
	}
	return a, Result{}, true
}

func (c *Commands) token(ctx context.Context, userID int64) (string, Result, bool) {
	token, err := c.Access.Token(ctx, userID)
	switch {
	case errors.Is(err, ErrSignInRequired):
		return "", Result{Kind: ResultSignInRequired, Disposition: DispositionNotRecorded}, false
	case err != nil:
		return "", Result{Kind: ResultCredentialUnavailable, Disposition: DispositionNotRecorded}, false
	}
	return token, Result{}, true
}

// respond reloads the operation and answers it only while the fence still holds.
func (c *Commands) respond(ctx context.Context, userID, opID int64, fence Fence, created bool) Result {
	op, found, err := c.Store.Get(ctx, opID, userID)
	if err == nil && !found {
		err = fmt.Errorf("write operation %d not found", opID)
	}
	if err != nil {
		return internalResult(DispositionPossiblyRecorded, err)
	}
	current, err := c.Access.FenceCurrent(ctx, fence)
	if err != nil {
		return internalResult(DispositionPossiblyRecorded, err)
	}
	if !current {
		return Result{Kind: ResultRecordedAccessMoved, Disposition: DispositionPossiblyRecorded}
	}
	return Result{Kind: ResultOperation, Disposition: DispositionPossiblyRecorded, Operation: op, Created: created}
}

// replay answers a request whose idempotency key was used before; ok is false when the key is new.
func (c *Commands) replay(ctx context.Context, userID int64, key string, hash []byte, fence Fence) (Result, bool) {
	prior, found, err := c.Store.FindByKey(ctx, userID, key)
	switch {
	case err != nil:
		return internalResult(DispositionNone, err), true
	case !found:
		return Result{}, false
	case !bytes.Equal(prior.RequestHash, hash):
		return Result{Kind: ResultKeyReuse, Disposition: DispositionNotRecorded}, true
	}
	if prior.StalePending {
		if err := c.Store.FailStale(ctx, prior.ID); err != nil {
			return internalResult(DispositionPossiblyRecorded, err), true
		}
	}
	return c.respond(ctx, userID, prior.ID, fence, false), true
}

// checkAssignable asks GitHub whether each login can be assigned, concurrently; ok is false when one cannot or GitHub
// failed.
func (c *Commands) checkAssignable(ctx context.Context, userID int64, token, fullName string, logins []string) (Result, bool) {
	assignable, err := MapConcurrent(ctx, logins, func(ctx context.Context, login string) (bool, error) {
		return c.GitHub.IsAssignable(ctx, token, fullName, login)
	})
	if err != nil {
		return c.upstream(ctx, userID, err), false
	}
	var ineligible []string
	for i, login := range logins {
		if !assignable[i] {
			ineligible = append(ineligible, login)
		}
	}
	if len(ineligible) > 0 {
		return Result{Kind: ResultUnassignable, Disposition: DispositionNotRecorded, Logins: ineligible}, false
	}
	return Result{}, true
}

// reserved answers a concurrent insert of the same key (by replaying it) and a fence that moved before the insert;
// ok is true for any other reserve result.
func (c *Commands) reserved(ctx context.Context, userID int64, key string, hash []byte, fence Fence, r ReserveResult) (Result, bool) {
	switch r {
	case ReserveRace:
		if res, ok := c.replay(ctx, userID, key, hash, fence); ok {
			return res, false
		}
		return internalResult(DispositionNone, errors.New("idempotency key vanished")), false
	case ReserveAccessMoved:
		return Result{Kind: ResultSubmitAccessMoved, Disposition: DispositionNotRecorded}, false
	}
	return Result{}, true
}

// reserveOnce inserts an operation that carries no content hash (an update or status_labels) as pending. Such an
// operation cannot hit the unresolved-create constraint, so any result but Reserved, ReserveRace or
// ReserveAccessMoved is an internal error. ok is false when res is the answer.
func (c *Commands) reserveOnce(ctx context.Context, fence Fence, op NewOperation) (int64, Result, bool) {
	opID, reserved, err := c.Store.Reserve(ctx, fence, op)
	if err != nil {
		return 0, internalResult(DispositionNone, err), false
	}
	if res, ok := c.reserved(ctx, op.UserID, op.Key, op.RequestHash, fence, reserved); !ok {
		return 0, res, false
	}
	if reserved != Reserved {
		return 0, internalResult(DispositionNone, fmt.Errorf("unexpected reserve result %d for %s", reserved, op.Kind)), false
	}
	return opID, Result{}, true
}

// markSent claims a reserved operation for sending; when the claim was lost the operation is failed as stale and
// answered 200, and ok is false.
func (c *Commands) markSent(ctx context.Context, userID, opID int64, fence Fence) (Result, bool) {
	if sent, err := c.Store.MarkSent(ctx, opID); err != nil {
		return internalResult(DispositionPossiblyRecorded, fmt.Errorf("mark sent: %w", err)), false
	} else if !sent {
		if err := c.Store.FailStale(ctx, opID); err != nil {
			return internalResult(DispositionPossiblyRecorded, err), false
		}
		return c.respond(ctx, userID, opID, fence, false), false
	}
	return Result{}, true
}

// UpdateIssue records an update, sends its steps with the user's own token and settles what GitHub shows afterwards.
// Order: checked access → replay → permission (or the author's state-only fallback) → token → fetch → expected
// state → assignability → reserve → mark sent → steps → settle under the issue lock. Nothing is recorded before
// reserve, and from reserve on the request's cancellation is ignored.
func (c *Commands) UpdateIssue(ctx context.Context, in UpdateIssueInput) Result {
	d := in.Draft
	draft, err := json.Marshal(&d)
	if err != nil {
		return internalResult(DispositionNotRecorded, err)
	}
	hash := domain.RequestHash("update", in.RepoID, in.Number, draft)

	a, res, ok := c.checkedAccess(ctx, in.UserID, in.RepoID)
	if !ok {
		return res
	}
	if res, ok := c.replay(ctx, in.UserID, in.Key, hash, a.Fence); ok {
		return res
	}
	forbidden := Result{Kind: ResultForbidden, Disposition: DispositionNotRecorded}
	// GitHub lets an issue's author close and reopen it with read access alone; labels and assignees need triage.
	stateOnly := len(d.Labels.Add)+len(d.Labels.Remove)+len(d.Assignees.Add)+len(d.Assignees.Remove) == 0
	manage := domain.CanManage(a.Repo.Repo)
	if !manage && !stateOnly {
		return forbidden
	}
	token, res, ok := c.token(ctx, in.UserID)
	if !ok {
		return res
	}
	fullName := a.Repo.FullName
	issue, err := c.GitHub.FetchIssue(ctx, token, fullName, in.Number)
	if remoteKind(err, domain.NotFound) || err == nil && issue.PullRequest {
		return Result{Kind: ResultIssueNotFound, Disposition: DispositionNotRecorded}
	}
	if err != nil {
		return c.upstream(ctx, in.UserID, err)
	}
	if !manage {
		actor, err := c.Store.ActorGithubID(ctx, in.UserID)
		if err != nil {
			return internalResult(DispositionNotRecorded, err)
		}
		if issue.AuthorID == 0 || issue.AuthorID != actor || !domain.CanCreate(a.Repo.Repo) {
			return forbidden
		}
	}
	if e := d.Expect; e != nil && !e.Matches(issue.View) {
		live := issue.View
		return Result{Kind: ResultConflict, Disposition: DispositionNotRecorded, Current: Current{State: live.State, Labels: live.Labels,
			Assignees: live.Assignees, Status: domain.BoardStatus(live.State, live.Labels), UpdatedAt: issue.UpdatedAt}}
	}
	if res, ok := c.checkAssignable(ctx, in.UserID, token, fullName, d.Assignees.Add); !ok {
		return res
	}

	ctx = context.WithoutCancel(ctx)
	opID, res, ok := c.reserveOnce(ctx, a.Fence, NewOperation{UserID: in.UserID, RepositoryID: in.RepoID, Key: in.Key, Kind: "update",
		IssueNumber: in.Number, RequestHash: hash, Draft: draft})
	if !ok {
		return res
	}
	if res, ok := c.markSent(ctx, in.UserID, opID, a.Fence); !ok {
		return res
	}
	out := c.applySteps(ctx, in.UserID, token, fullName, in.Number, d)
	var readErr error
	err = c.Store.SettleUnderLock(ctx, Settle{OperationID: opID, RepositoryID: in.RepoID, Fence: a.Fence, LockNumber: in.Number},
		func() (Settlement, error) {
			s, fetchErr := c.readBack(ctx, token, fullName, in.Number, d, out)
			readErr = fetchErr
			return s, nil
		})
	c.revokeIfUnauthorized(ctx, in.UserID, readErr)
	if err != nil {
		return internalResult(DispositionPossiblyRecorded, err)
	}
	return c.respond(ctx, in.UserID, opID, a.Fence, true)
}

// applySteps applies the planned add/remove steps (each idempotent), stopping at the first error; readBack then
// decides the outcome. Assignees are removed before they are added, so a swap at GitHub's 10-assignee limit fits. A
// reopen is the first step and a close the last, so a change that reopens or closes an issue never adds a label while
// it is still closed, where the app's status label cleanup could strip it; a rejected reopen applies nothing.
func (c *Commands) applySteps(ctx context.Context, userID int64, token, fullName string, number int64, d domain.UpdateDraft) domain.Outcome {
	out := domain.Outcome{AllStepsAnswered: true}
	for _, step := range domain.PlanUpdate(d) {
		var err error
		switch step.Kind {
		case domain.Reopen, domain.Close:
			err = c.GitHub.SetIssueState(ctx, token, fullName, number, d.State)
		case domain.AddLabels:
			err = c.GitHub.AddLabels(ctx, token, fullName, number, step.Names)
		case domain.RemoveLabel:
			err = c.GitHub.RemoveLabel(ctx, token, fullName, number, step.Names[0])
		case domain.RemoveAssignees:
			err = c.GitHub.RemoveAssignees(ctx, token, fullName, number, step.Names)
		case domain.AddAssignees:
			err = c.GitHub.AddAssignees(ctx, token, fullName, number, step.Names)
		}
		if err != nil {
			c.revokeIfUnauthorized(ctx, userID, err)
			var state string
			state, out.Error = classify(err)
			out.AllStepsAnswered = state == "failed"
			break
		}
	}
	return out
}

// readBack reads the issue after the steps and settles on what it shows; the fetch error is returned so the caller
// can revoke on 401 after the settlement transaction.
func (c *Commands) readBack(ctx context.Context, token, fullName string, number int64, d domain.UpdateDraft, out domain.Outcome) (Settlement, error) {
	issue, err := c.GitHub.FetchIssue(ctx, token, fullName, number)
	if err != nil {
		_, msg := classify(err)
		out.Error = strings.TrimSpace(out.Error + " Reading the result failed: " + msg)
		return Settlement{State: "unknown", Outcome: out}, err
	}
	out.Issue = issue.View
	out.Unapplied = domain.Unapplied(out.Issue, d.Labels, d.Assignees, d.State)
	if len(out.Unapplied) > 0 && !out.AllStepsAnswered {
		return Settlement{State: "unknown", Outcome: out, Confirmed: issue.Payload}, nil
	}
	return Settlement{State: domain.Resolved(out), Outcome: out, Confirmed: issue.Payload}, nil
}

package application

import (
	"context"
	"encoding/json"

	"github.com/repoorbit/repoorbit/backend/internal/issues/domain"
)

// CreateIssueInput is a validated create request. The HTTP adapter has already checked the signed-in user, the path
// id and the Idempotency-Key format.
type CreateIssueInput struct {
	UserID int64
	RepoID int64
	Key    string
	Draft  domain.CreateDraft
}

// CreateIssue records a create, sends it once with the user's own token (never retried, even when the outcome is
// unknown) and settles what GitHub answered, linking the new issue's number. Order: checked access → replay →
// permission (labels or assignees need CanCreateWithMetadata) → token → assignability → reserve (an unresolved
// earlier create of the same content answers 409) → mark sent → create → settle. Nothing is recorded before reserve,
// and from reserve on the request's cancellation is ignored.
func (c *Commands) CreateIssue(ctx context.Context, in CreateIssueInput) Result {
	d := in.Draft
	contentHash := domain.ContentHash(d.Title, d.Body)
	hasMetadata := len(d.Labels)+len(d.Assignees) > 0
	draft, err := json.Marshal(&d)
	if err != nil {
		return internalResult(DispositionNotRecorded, err)
	}
	hash := domain.RequestHash("create", in.RepoID, 0, draft)

	a, res, ok := c.checkedAccess(ctx, in.UserID, in.RepoID)
	if !ok {
		return res
	}
	if res, ok := c.replay(ctx, in.UserID, in.Key, hash, a.Fence); ok {
		return res
	}
	allowed := domain.CanCreate
	if hasMetadata {
		allowed = domain.CanCreateWithMetadata
	}
	if !allowed(a.Repo.Repo) {
		return Result{Kind: ResultForbidden, Disposition: DispositionNotRecorded}
	}
	token, res, ok := c.token(ctx, in.UserID)
	if !ok {
		return res
	}
	fullName := a.Repo.FullName
	if res, ok := c.checkAssignable(ctx, in.UserID, token, fullName, d.Assignees); !ok {
		return res
	}

	ctx = context.WithoutCancel(ctx)
	op := NewOperation{UserID: in.UserID, RepositoryID: in.RepoID, Key: in.Key, Kind: "create", RequestHash: hash, ContentHash: contentHash,
		HasRequestedMetadata: hasMetadata, Draft: draft}
	opID, res, ok := c.reserveCreate(ctx, a.Fence, op)
	if !ok {
		return res
	}
	if res, ok := c.markSent(ctx, in.UserID, opID, a.Fence); !ok {
		return res
	}
	settlement := c.sendCreate(ctx, in.UserID, token, fullName, d)
	err = c.Store.SettleUnderLock(ctx, Settle{OperationID: opID, RepositoryID: in.RepoID, Fence: a.Fence, LockNumber: 0, Link: true},
		func() (Settlement, error) { return settlement, nil })
	if err != nil {
		return internalResult(DispositionPossiblyRecorded, err)
	}
	return c.respond(ctx, in.UserID, opID, a.Fence, true)
}

// reserveCreate inserts the create as pending. When an earlier create of the same content is unresolved, stale
// pending creates are failed once and the insert retried once; if it is still unresolved, the earlier operation is
// answered with 409 and nothing is recorded.
func (c *Commands) reserveCreate(ctx context.Context, fence Fence, op NewOperation) (int64, Result, bool) {
	k := CreateKey{UserID: op.UserID, RepositoryID: op.RepositoryID, ContentHash: op.ContentHash}
	for attempt := 0; ; attempt++ {
		opID, reserved, err := c.Store.Reserve(ctx, fence, op)
		if err != nil {
			return 0, internalResult(DispositionNone, err), false
		}
		if reserved != ReserveUnresolvedCreate {
			res, ok := c.reserved(ctx, op.UserID, op.Key, op.RequestHash, fence, reserved)
			return opID, res, ok
		}
		if attempt == 0 {
			n, err := c.Store.FailStalePendingCreate(ctx, k)
			if err != nil {
				return 0, internalResult(DispositionNone, err), false
			}
			if n > 0 {
				continue
			}
		}
		return 0, c.duplicateCreate(ctx, k), false
	}
}

// duplicateCreate answers a create whose content matches an earlier create that is (or just was) unresolved.
func (c *Commands) duplicateCreate(ctx context.Context, k CreateKey) Result {
	msg := "an earlier create of this issue is still unresolved; recheck it instead of resubmitting"
	prior, found, err := c.Store.FindUnresolvedCreate(ctx, k)
	if err == nil && !found {
		msg = "an earlier create of this issue was resolved while submitting; nothing was sent: check it before resubmitting"
		prior, found, err = c.Store.FindLatestCreate(ctx, k)
	}
	switch {
	case err != nil:
		return internalResult(DispositionNotRecorded, err)
	case !found:
		return Result{Kind: ResultCreateChanged, Disposition: DispositionNotRecorded}
	}
	return Result{Kind: ResultDuplicateCreate, Disposition: DispositionNotRecorded, Message: msg, OperationID: prior}
}

// sendCreate sends the create exactly once and turns GitHub's answer into the settlement; only a created issue is
// confirmed.
func (c *Commands) sendCreate(ctx context.Context, userID int64, token, fullName string, d domain.CreateDraft) Settlement {
	issue, err := c.GitHub.CreateIssue(ctx, token, fullName, d)
	if err != nil {
		c.revokeIfUnauthorized(ctx, userID, err)
		state, msg := classify(err)
		return Settlement{State: state, Outcome: domain.Outcome{Error: msg}}
	}
	out := domain.Outcome{Issue: issue.View}
	out.Unapplied = domain.Unapplied(out.Issue, domain.Delta{Add: d.Labels}, domain.Delta{Add: d.Assignees}, "")
	return Settlement{State: domain.Resolved(out), Outcome: out, Confirmed: issue.Payload}
}

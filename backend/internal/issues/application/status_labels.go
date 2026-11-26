package application

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/repoorbit/repoorbit/backend/internal/issues/domain"
)

// EnableStatusLabelsInput is an enable request. The HTTP adapter has already decoded the empty body strictly and
// checked the signed-in user, the path id and the Idempotency-Key format.
type EnableStatusLabelsInput struct {
	UserID int64
	RepoID int64
	Key    string
}

// statusLabelsDraft is the stored draft of a status_labels operation; its JSON is part of the request hash.
type statusLabelsDraft struct {
	Labels []string `json:"labels"`
}

// EnableStatusLabels records a status_labels operation, creates the missing status labels with the user's own token
// (reusing existing ones), reads all three back and turns status labels on once they are confirmed. Order: checked
// access → replay → permission (Admin or Maintain) → token → reserve → mark sent → create labels → read back →
// settle. Nothing is recorded before reserve, and from reserve on the request's cancellation is ignored.
func (c *Commands) EnableStatusLabels(ctx context.Context, in EnableStatusLabelsInput) Result {
	d := &statusLabelsDraft{}
	for _, l := range domain.StatusLabels {
		d.Labels = append(d.Labels, l.Name)
	}
	draft, err := json.Marshal(d)
	if err != nil {
		return internalResult(DispositionNotRecorded, err)
	}
	hash := domain.RequestHash("status_labels", in.RepoID, 0, draft)

	a, res, ok := c.checkedAccess(ctx, in.UserID, in.RepoID)
	if !ok {
		return res
	}
	if res, ok := c.replay(ctx, in.UserID, in.Key, hash, a.Fence); ok {
		return res
	}
	if !domain.CanChangeStatusLabels(a.Repo.Repo) {
		return Result{Kind: ResultForbidden, Disposition: DispositionNotRecorded}
	}
	token, res, ok := c.token(ctx, in.UserID)
	if !ok {
		return res
	}

	ctx = context.WithoutCancel(ctx)
	opID, res, ok := c.reserveOnce(ctx, a.Fence, NewOperation{UserID: in.UserID, RepositoryID: in.RepoID, Key: in.Key, Kind: "status_labels",
		RequestHash: hash, Draft: draft})
	if !ok {
		return res
	}
	if res, ok := c.markSent(ctx, in.UserID, opID, a.Fence); !ok {
		return res
	}
	state, out := c.ensureStatusLabels(ctx, in.UserID, token, a.Repo.FullName)
	if err := c.Store.SettleStatusLabels(ctx, opID, in.RepoID, a.Fence, state, out); err != nil {
		return internalResult(DispositionPossiblyRecorded, err)
	}
	return c.respond(ctx, in.UserID, opID, a.Fence, true)
}

// ensureStatusLabels creates each status label, stopping at the first error, then reads all three back: the outcome is
// what the read shows. A label that already exists, in any case, is answered 422 and reused, so nothing here can
// duplicate anything; an ambiguous step only leaves the operation unknown when the read still misses a label.
func (c *Commands) ensureStatusLabels(ctx context.Context, userID int64, token, fullName string) (string, domain.Outcome) {
	out := domain.Outcome{}
	answered := true
	for _, l := range domain.StatusLabels {
		err := c.GitHub.CreateLabel(ctx, token, fullName, l.Name, l.Color)
		var re *RemoteError
		if errors.As(err, &re) && re.Kind == domain.HTTPError && re.Status == 422 { // 422 Unprocessable Entity: it exists
			err = nil
		} else if err == nil {
			out.Created = append(out.Created, l.Name)
		}
		if err != nil {
			c.revokeIfUnauthorized(ctx, userID, err)
			var state string
			state, out.Error = classify(err)
			answered = state == "failed"
			break
		}
	}
	state, out := c.ReadStatusLabels(ctx, token, fullName, out)
	if state == "failed" && !answered {
		state = "unknown"
	}
	return state, out
}

// DisableStatusLabels turns status labels off for an Admin or Maintain user, checking in the same transaction that
// the access fence still holds. Nothing is written to GitHub. Its answers carry no disposition.
func (c *Commands) DisableStatusLabels(ctx context.Context, userID, repoID int64) Result {
	res := c.disableStatusLabels(ctx, userID, repoID)
	res.Disposition = DispositionNone
	return res
}

func (c *Commands) disableStatusLabels(ctx context.Context, userID, repoID int64) Result {
	a, res, ok := c.checkedAccess(ctx, userID, repoID)
	if !ok {
		return res
	}
	if !domain.CanChangeStatusLabels(a.Repo.Repo) {
		return Result{Kind: ResultStatusLabelsForbidden}
	}
	moved, err := c.Store.DisableStatusLabels(ctx, repoID, a.Fence)
	switch {
	case err != nil:
		return internalResult(DispositionNone, err)
	case moved:
		return Result{Kind: ResultDisableAccessMoved}
	}
	return Result{Kind: ResultNoContent}
}

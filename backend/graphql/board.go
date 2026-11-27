package graph

import (
	"context"
	"time"

	"github.com/repoorbit/repoorbit/backend/graphql/model"
	"github.com/repoorbit/repoorbit/backend/internal/board"
	"github.com/repoorbit/repoorbit/backend/internal/readguard"
)

// repositoryBoard reads one selected repository's Board snapshot with repositoryOverview's access rules: null for a
// missing, unselected or denied repository, ACCESS_UNVERIFIED when access is unverified or the fence moves during the
// read. Demo mode reads a demo repository without an access check.
func (r *queryResolver) repositoryBoard(ctx context.Context, id string, include []int) (*model.RepositoryBoard, error) {
	repoID, err := parsePositiveID(&id)
	if err != nil {
		return nil, err
	}
	if _, err := board.ValidateInclude(include); err != nil {
		return nil, err
	}
	ctx, cancel := withReadDeadline(ctx)
	defer cancel()

	reader := &board.Reader{Queries: r.Queries}
	read := func(targetID int64) (*model.RepositoryBoard, error) {
		snap, err := reader.Read(ctx, targetID, r.Demo, include)
		if err != nil || snap == nil {
			return nil, err
		}
		return mapBoard(snap), nil
	}
	if r.Demo {
		return read(repoID)
	}
	auth, err := r.authorizeSelected(ctx, []int64{repoID})
	if err != nil {
		return nil, err
	}
	if len(auth.Allowed) == 0 {
		if auth.Withheld > 0 {
			return nil, unverifiedError(auth.RetryAt)
		}
		return nil, nil
	}
	return readguard.Run(ctx, r.RepoAccess.Pool, auth,
		func() (*model.RepositoryBoard, error) { return read(auth.Allowed[0]) },
		func() (*model.RepositoryBoard, error) { return nil, unverifiedError(time.Time{}) },
	)
}

func mapBoard(snap *board.Snapshot) *model.RepositoryBoard {
	out := &model.RepositoryBoard{
		StatusLabels: snap.StatusLabels,
		Truncated:    snap.Truncated,
		Issues:       make([]*model.BoardIssue, 0, len(snap.Issues)),
	}
	for _, issue := range snap.Issues {
		out.Issues = append(out.Issues, &model.BoardIssue{
			Number:         issue.Number,
			Title:          issue.Title,
			URL:            issue.URL,
			State:          issue.State,
			UpdatedAt:      stamp(issue.UpdatedAt),
			AuthorLogin:    issue.AuthorLogin,
			Labels:         issue.Labels,
			Assignees:      issue.Assignees,
			Status:         issue.Status,
			MultipleStatus: issue.MultipleStatus,
		})
	}
	return out
}

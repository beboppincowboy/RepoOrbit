package graph

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/repoorbit/repoorbit/backend/graphql/model"
	"github.com/repoorbit/repoorbit/backend/internal/db"
	"github.com/repoorbit/repoorbit/backend/internal/readguard"
)

// maxBoardInclude bounds repositoryBoard's include numbers (staged cards the caps may leave out).
const maxBoardInclude = 100

// repositoryBoard reads one selected repository's Board snapshot with repositoryOverview's access rules: null for a
// missing, unselected or denied repository, ACCESS_UNVERIFIED when access is unverified or the fence moves during the
// read. Demo mode reads a demo repository without an access check.
func (r *queryResolver) repositoryBoard(ctx context.Context, id string, include []int) (*model.RepositoryBoard, error) {
	repoID, err := parsePositiveID(&id)
	if err != nil {
		return nil, err
	}
	if len(include) > maxBoardInclude {
		return nil, fmt.Errorf("at most %d include numbers", maxBoardInclude)
	}
	numbers := make([]int64, len(include))
	for i, n := range include {
		numbers[i] = int64(n)
	}
	ctx, cancel := withReadDeadline(ctx)
	defer cancel()
	read := func(repoID int64) (*model.RepositoryBoard, error) {
		// ponytail: the flag and the rows are two statements, so a flag flipped between them shows on the next read.
		on, err := r.Queries.GetBoardStatusLabels(ctx, db.GetBoardStatusLabelsParams{ID: repoID, Demo: r.Demo})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		if err != nil {
			return nil, fmt.Errorf("database unavailable")
		}
		rows, err := r.Queries.ListRepositoryBoard(ctx, db.ListRepositoryBoardParams{RepositoryID: repoID, Demo: r.Demo, Include: numbers})
		if err != nil {
			return nil, fmt.Errorf("database unavailable")
		}
		out := &model.RepositoryBoard{StatusLabels: on, Issues: make([]*model.BoardIssue, 0, len(rows))}
		for _, row := range rows {
			out.Truncated = out.Truncated || row.Truncated
			issue := &model.BoardIssue{
				Number: int(row.Number), Title: row.Title, URL: row.HtmlUrl, State: row.State, UpdatedAt: stamp(row.UpdatedAt.Time),
				Labels: row.Labels, Assignees: row.Assignees, MultipleStatus: row.MultipleStatus,
			}
			if row.AuthorLogin.Valid {
				issue.AuthorLogin = &row.AuthorLogin.String
			}
			if row.Status.Valid {
				issue.Status = &row.Status.String
			}
			out.Issues = append(out.Issues, issue)
		}
		return out, nil
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

// Package board implements the repository Board read use case.
//
// It wraps the status-label flag query, the bounded Board issue query (backed by the issue_board_states view),
// and the 100-number include cap without depending on HTTP or GraphQL transport packages.
package board

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/repoorbit/repoorbit/backend/internal/db"
)

// MaxInclude bounds the number of explicit issue numbers a Board read may request beyond the SQL caps.
const MaxInclude = 100

// Snapshot is one repository's Board read result.
type Snapshot struct {
	StatusLabels bool
	Truncated    bool
	Issues       []Issue
}

// Issue is one card in a Board Snapshot.
type Issue struct {
	Number         int
	Title          string
	URL            string
	State          string
	UpdatedAt      time.Time
	AuthorLogin    *string
	Labels         []string
	Assignees      []string
	Status         *string
	MultipleStatus bool
}

// Reader reads Board snapshots from PostgreSQL.
type Reader struct {
	Queries *db.Queries
}

// ValidateInclude enforces MaxInclude and converts include numbers to int64 for SQL parameters.
func ValidateInclude(include []int) ([]int64, error) {
	if len(include) > MaxInclude {
		return nil, fmt.Errorf("at most %d include numbers", MaxInclude)
	}
	numbers := make([]int64, len(include))
	for i, n := range include {
		numbers[i] = int64(n)
	}
	return numbers, nil
}

// Read loads one repository's Board snapshot using two separate statements (status-label flag, then bounded rows).
// It returns (nil, nil) when the repository does not exist for the requested demo mode.
func (r *Reader) Read(ctx context.Context, repoID int64, demo bool, include []int) (*Snapshot, error) {
	numbers, err := ValidateInclude(include)
	if err != nil {
		return nil, err
	}
	// The flag and the rows are two statements, so a flag flipped between them shows on the next read.
	on, err := r.Queries.GetBoardStatusLabels(ctx, db.GetBoardStatusLabelsParams{ID: repoID, Demo: demo})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("database unavailable")
	}
	rows, err := r.Queries.ListRepositoryBoard(ctx, db.ListRepositoryBoardParams{RepositoryID: repoID, Demo: demo, Include: numbers})
	if err != nil {
		return nil, fmt.Errorf("database unavailable")
	}
	out := &Snapshot{StatusLabels: on, Issues: make([]Issue, 0, len(rows))}
	for _, row := range rows {
		out.Truncated = out.Truncated || row.Truncated
		issue := Issue{
			Number:         int(row.Number),
			Title:          row.Title,
			URL:            row.HtmlUrl,
			State:          row.State,
			UpdatedAt:      row.UpdatedAt.Time,
			Labels:         row.Labels,
			Assignees:      row.Assignees,
			MultipleStatus: row.MultipleStatus,
		}
		if row.AuthorLogin.Valid {
			v := row.AuthorLogin.String
			issue.AuthorLogin = &v
		}
		if row.Status.Valid {
			v := row.Status.String
			issue.Status = &v
		}
		out.Issues = append(out.Issues, issue)
	}
	return out, nil
}

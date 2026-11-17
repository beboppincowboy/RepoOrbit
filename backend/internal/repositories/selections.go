package repositories

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/repoorbit/repoorbit/backend/internal/db"
)

var (
	ErrNotFound = errors.New("repository not found") // missing, no access, or not the user's selection: indistinguishable on purpose
	ErrNotOwned = errors.New("repository is not on your personal account")
)

// UnverifiedError means access could not be checked right now. RetryAt is set when rate limited.
type UnverifiedError struct{ RetryAt time.Time }

func (e *UnverifiedError) Error() string { return "access could not be verified; retry later" }

// Select connects one of the user's own repositories. Access is checked live (never from the cache), the
// repository must be on the user's personal account, and only the explicit choice is stored.
func (s *Service) Select(ctx context.Context, userID, repoID int64) error {
	d := s.CheckOne(ctx, userID, repoID, true)
	if d.Unverified {
		return &UnverifiedError{RetryAt: d.RetryAt}
	}
	if !d.Allowed {
		return ErrNotFound
	}
	ghID, err := s.Q.GetUserGithubID(ctx, userID)
	if err != nil {
		return err
	}
	if d.Repo.Owner.ID != ghID || d.Repo.Owner.Type != "User" {
		return ErrNotOwned
	}
	if _, err = s.Q.UpsertRepository(ctx, db.UpsertRepositoryParams{ID: d.Repo.ID, Name: d.Repo.FullName, Description: d.Repo.Description, IsPrivate: d.Repo.Private}); errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("repository %d collides with a demo row", d.Repo.ID)
	} else if err != nil {
		return err
	}
	return s.Q.UpsertSelection(ctx, db.UpsertSelectionParams{UserID: userID, RepositoryID: d.Repo.ID, Source: "owned", SelectedAt: pgtype.Timestamptz{Time: s.now(), Valid: true}})
}

// Deselect marks the user's own selection deselected. It touches only that user's row.
func (s *Service) Deselect(ctx context.Context, userID, repoID int64) error {
	n, err := s.Q.DeselectRepository(ctx, db.DeselectRepositoryParams{UserID: userID, RepositoryID: repoID, Now: pgtype.Timestamptz{Time: s.now(), Valid: true}})
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// SyncState is what the UI says about a repository's data until ingestion exists (U3).
func SyncState(selected bool) string {
	if selected {
		return "awaiting-sync"
	}
	return "not-synchronized"
}

package repositories

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/repoorbit/repoorbit/backend/internal/accessfence"
	"github.com/repoorbit/repoorbit/backend/internal/db"
	"github.com/repoorbit/repoorbit/backend/internal/githubapp"
)

var (
	ErrNotFound = errors.New("repository not found") // missing, no access, or not the user's selection: indistinguishable on purpose
	ErrNotOwned = errors.New("repository is not on your personal account")
)

// UnverifiedError means access could not be checked right now. RetryAt is set when rate limited.
type UnverifiedError struct{ RetryAt time.Time }

func (e *UnverifiedError) Error() string { return "access could not be verified; retry later" }

// Connect selects one of the user's own repositories (source "owned"). Access is checked live (never from the
// cache) and the repository must be on the user's personal account. Calls GitHub; writes one repository and one
// selection row. Returns ErrNotFound (no access), ErrNotOwned (not personal account) or *UnverifiedError.
func (s *Service) Connect(ctx context.Context, userID, repoID int64) error {
	d := s.CheckOne(ctx, userID, repoID, true)
	if d.Unverified {
		return &UnverifiedError{RetryAt: d.RetryAt}
	}
	if !d.Allowed {
		return ErrNotFound
	}
	ghID, err := s.Queries.GetUserGithubID(ctx, userID)
	if err != nil {
		return err
	}
	if d.Repo.Owner.ID != ghID || d.Repo.Owner.Type != "User" {
		return ErrNotOwned
	}
	return s.storeSelection(ctx, userID, d.Repo, selectionSourceOwned)
}

// How a selection came about; stored in selections.source. "owned" wins when both apply (see UpsertSelection).
const (
	selectionSourceOwned   = "owned"
	selectionSourceStarred = "starred"
)

// storeSelection upserts repo's metadata and the user's selection of it, plus durable scheduling (OnSelect), in one transaction. Fails (loudly) if repo's id collides
// with a demo row, which is never overwritten.
func (s *Service) storeSelection(ctx context.Context, userID int64, repo githubapp.Repo, source string) error {
	return db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		q := s.Queries.WithTx(tx)
		if _, err := q.UpsertRepository(ctx, db.UpsertRepositoryParams{ID: repo.ID, Name: repo.FullName, Description: repo.Description, IsPrivate: repo.Private}); errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("repository %d collides with a demo row", repo.ID)
		} else if err != nil {
			return err
		}
		if err := q.UpsertSelection(ctx, db.UpsertSelectionParams{UserID: userID, RepositoryID: repo.ID, Source: source, SelectedAt: pgtype.Timestamptz{Time: s.now(), Valid: true}}); err != nil {
			return err
		}
		if repo.Private {
			if err := accessfence.BumpAccess(ctx, tx, accessfence.ScopeRepository, repo.ID); err != nil {
				return err
			}
		}
		if s.OnSelect != nil {
			if err := s.OnSelect(ctx, tx, repo.ID); err != nil {
				return err
			}
		}
		if s.OnEligibilityChange != nil { // a reselect may be what unblocks paused work (never dead work)
			if err := s.OnEligibilityChange(ctx, tx, userID); err != nil {
				return err
			}
		}
		return nil
	})
}

// Deselect marks the user's own selection deselected, for either source (also unfollows). It touches only that
// user's row and makes no GitHub call. Returns ErrNotFound when the user has no active selection for repoID.
func (s *Service) Deselect(ctx context.Context, userID, repoID int64) error {
	n, err := s.Queries.DeselectRepository(ctx, db.DeselectRepositoryParams{UserID: userID, RepositoryID: repoID, Now: pgtype.Timestamptz{Time: s.now(), Valid: true}})
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// requestActivityRefresh checks selection and live access before signaling activity in a transaction.
func (s *Service) requestActivityRefresh(ctx context.Context, userID, repoID int64) (string, error) {
	if s.SignalActivity == nil {
		return "", ErrNotFound
	}
	sel, err := s.Queries.ListSelectedIDs(ctx, db.ListSelectedIDsParams{UserID: userID, RepositoryIds: []int64{repoID}})
	if err != nil {
		return "", err
	}
	if len(sel) == 0 {
		return "", ErrNotFound
	}
	d := s.CheckOne(ctx, userID, repoID, true)
	if d.Unverified {
		return "", &UnverifiedError{RetryAt: d.RetryAt}
	}
	if !d.Allowed {
		return "", ErrNotFound
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	outcome, err := s.SignalActivity(ctx, tx, repoID)
	if err != nil {
		return "", err
	}
	return outcome, tx.Commit(ctx)
}

// connectInstallation checks the user's GitHub installations and returns the connection outcome.
// A match in a partial listing can be stored, and the visibility cache is invalidated after commit.
func (s *Service) connectInstallation(ctx context.Context, userID, installationID int64) string {
	token, err := s.TokenSource.AccessToken(ctx, userID)
	if err != nil {
		return "failed"
	}
	installs, complete, err := collect(func(p int) ([]githubapp.Installation, bool, error) {
		return s.GitHub.FetchInstallations(ctx, token, p)
	})
	for _, in := range installs {
		if in.ID == installationID {
			if err := s.storeInstallation(ctx, userID, in); err != nil {
				log.Printf("repositories: store installation: %v", err)
				return "failed"
			}
			s.visibility.invalidate(userID)
			return "ok"
		}
	}
	if err == nil && complete {
		return "rejected"
	}
	return "failed"
}

// storeInstallation saves a connected installation and runs OnEligibilityChange in one transaction, so work paused
// for lack of access resumes with it.
func (s *Service) storeInstallation(ctx context.Context, userID int64, in githubapp.Installation) error {
	return db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		if err := accessfence.BumpAccess(ctx, tx, accessfence.ScopeInstallation, in.ID); err != nil {
			return err
		}
		if err := s.Queries.WithTx(tx).UpsertInstallation(ctx, db.UpsertInstallationParams{UserID: userID, InstallationID: in.ID, AccountLogin: in.Account.Login}); err != nil {
			return err
		}
		if s.OnEligibilityChange != nil {
			return s.OnEligibilityChange(ctx, tx, userID)
		}
		return nil
	})
}

// SyncState is what the UI says about a repository's data until ingestion exists (U3). The string values are
// the wire format in REST (syncState) and GraphQL (Repository.syncState).
type SyncState string

const (
	SyncStateDemo            SyncState = "demo"             // seeded demo repository (Demo mode only)
	SyncStateAwaitingSync    SyncState = "awaiting-sync"    // selected, not yet ingested
	SyncStateNotSynchronized SyncState = "not-synchronized" // accessible but not selected
)

// SyncStateFor returns the state of a real repository given whether the user has selected it.
func SyncStateFor(selected bool) SyncState {
	if selected {
		return SyncStateAwaitingSync
	}
	return SyncStateNotSynchronized
}

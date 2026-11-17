package repositories

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/repoorbit/repoorbit/backend/internal/auth"
	"github.com/repoorbit/repoorbit/backend/internal/db"
	"github.com/repoorbit/repoorbit/backend/internal/githubapp"
)

const (
	starredPerPage = 30
	maxStarPages   = 30 // pages of githubPerPage: 3000 stars; a longer list is not snapshotted, so the previous snapshot is kept
)

// StarredPage is one page of the user's latest completed star snapshot. A failed refresh keeps the previous
// snapshot (RefreshFailed, with its RefreshedAt age) instead of showing an empty list.
type StarredPage struct {
	Repositories  []ListedRepo `json:"repositories"`
	NextPage      int          `json:"nextPage"`    // 0 when there is no next page
	RefreshedAt   *time.Time   `json:"refreshedAt"` // nil until a refresh has ever completed
	RefreshFailed bool         `json:"refreshFailed"`
	Withheld      int          `json:"withheld"` // private entries hidden because access could not be verified
	RetryAt       *time.Time   `json:"retryAt"`
	InstallURL    string       `json:"installUrl"`
}

// Starred serves a page of the snapshot. With refresh it first re-reads the user's stars from GitHub.
// Private entries are shown only after a live access check; public ones come straight from the snapshot.
// Calls GitHub (stars when refresh is set, access checks for private entries) and reads the database. A failed
// refresh is not an error: it is reported in the page (RefreshFailed). Returns ErrSignedOut when GitHub rejects the token.
func (s *Service) Starred(ctx context.Context, userID int64, page int, refresh bool) (StarredPage, error) {
	out := StarredPage{Repositories: []ListedRepo{}, InstallURL: s.InstallURL}
	if refresh {
		if err := s.refreshStars(ctx, userID, &out); err != nil {
			return StarredPage{}, err
		}
	}
	if at, err := s.Queries.GetStarSnapshotRefreshedAt(ctx, userID); err == nil {
		out.RefreshedAt = &at.Time
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return StarredPage{}, err
	}
	rows, err := s.Queries.ListStars(ctx, db.ListStarsParams{UserID: userID, PageSize: starredPerPage + 1, PageOffset: int32((page - 1) * starredPerPage)})
	if err != nil {
		return StarredPage{}, err
	}
	if len(rows) > starredPerPage {
		out.NextPage, rows = page+1, rows[:starredPerPage]
	}
	var privateIDs, ids []int64
	for _, r := range rows {
		ids = append(ids, r.RepositoryID)
		if r.IsPrivate {
			privateIDs = append(privateIDs, r.RepositoryID)
		}
	}
	checks := s.Check(ctx, userID, privateIDs, false)
	selected, err := s.selectedSet(ctx, userID, ids)
	if err != nil {
		return StarredPage{}, err
	}
	for _, r := range rows {
		if d := checks[r.RepositoryID]; r.IsPrivate && !d.Allowed {
			out.Withheld++
			if out.RetryAt == nil && !d.RetryAt.IsZero() {
				t := d.RetryAt
				out.RetryAt = &t
			}
			continue
		}
		out.Repositories = append(out.Repositories, ListedRepo{ID: r.RepositoryID, FullName: r.FullName, Description: r.Description, Private: r.IsPrivate, Selected: selected[r.RepositoryID], SyncState: SyncStateFor(selected[r.RepositoryID])})
	}
	return out, nil
}

// StarFetch is the outcome of reading every page of a user's stars. Repos is meaningful only when Complete; an
// incomplete fetch (transport failure, too many stars, rate limit) never replaces the stored snapshot.
type StarFetch struct {
	Repos    []githubapp.Repo
	Complete bool
	RetryAt  *time.Time // set when GitHub rate limited the read
}

// FetchStars reads all of a user's stars from GitHub. It makes GitHub calls and writes nothing: persisting the result
// is StoreStars, so callers choose the transaction (and any claim fence) it commits under. Returns ErrSignedOut when
// the user has no credential or GitHub rejects it (the user is then revoked); other failures come back as an
// incomplete StarFetch, not an error.
func (s *Service) FetchStars(ctx context.Context, userID int64) (StarFetch, error) {
	token, err := s.TokenSource.AccessToken(ctx, userID)
	if errors.Is(err, auth.ErrUnauthenticated) {
		return StarFetch{}, ErrSignedOut
	}
	if err != nil {
		log.Printf("repositories: token unavailable: %v", err)
		return StarFetch{}, nil
	}
	var all []githubapp.Repo
	for page := 1; ; page++ {
		if page > maxStarPages {
			err = fmt.Errorf("more than %d stars", maxStarPages*githubPerPage)
			break
		}
		var items []githubapp.Repo
		var more bool
		if items, more, err = s.GitHub.FetchStarredRepos(ctx, token, page, githubPerPage); err != nil {
			break
		}
		all = append(all, items...)
		if !more {
			break
		}
	}
	var apiErr *githubapp.APIError
	switch {
	case errors.As(err, &apiErr) && apiErr.Unauthorized():
		s.revoke(ctx, userID)
		return StarFetch{}, ErrSignedOut
	case err != nil:
		f := StarFetch{}
		if errors.As(err, &apiErr) && apiErr.RateLimited() {
			f.RetryAt = &apiErr.RetryAt
		}
		return f, nil
	}
	return StarFetch{Repos: all, Complete: true}, nil
}

// refreshStars replaces the snapshot only when every page was read. Any failure keeps the old one.
func (s *Service) refreshStars(ctx context.Context, userID int64, out *StarredPage) error {
	f, err := s.FetchStars(ctx, userID)
	if err != nil {
		return err
	}
	if !f.Complete {
		out.RefreshFailed, out.RetryAt = true, f.RetryAt
		return nil
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := s.StoreStars(ctx, tx, userID, f.Repos); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// StoreStars replaces the user's star snapshot with repos (a complete FetchStars result) inside the caller's
// transaction, then updates follow eligibility and runs OnEligibilityChange in that same transaction: unstarred follows
// stop being polled (the Selection stays), starred-again ones resume, owned selections never depend on stars. Writes
// only through tx; the caller commits (and may fence the commit). Concurrent stores for one user queue on an
// advisory lock instead of colliding on the primary key.
func (s *Service) StoreStars(ctx context.Context, tx pgx.Tx, userID int64, repos []githubapp.Repo) error {
	p := db.InsertStarsParams{UserID: userID}
	for _, r := range repos {
		p.Ids, p.FullNames, p.Descriptions, p.Privates = append(p.Ids, r.ID), append(p.FullNames, r.FullName), append(p.Descriptions, r.Description), append(p.Privates, r.Private)
	}
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", userID); err != nil {
		return err
	}
	q := s.Queries.WithTx(tx)
	if err := q.DeleteStars(ctx, userID); err != nil {
		return err
	}
	if err := q.InsertStars(ctx, p); err != nil {
		return err
	}
	now := pgtype.Timestamptz{Time: s.now(), Valid: true}
	if err := q.UpsertStarSnapshot(ctx, db.UpsertStarSnapshotParams{UserID: userID, RefreshedAt: now}); err != nil {
		return err
	}
	if err := q.SuspendUnstarredFollows(ctx, db.SuspendUnstarredFollowsParams{UserID: userID, Now: now}); err != nil {
		return err
	}
	if err := q.RestoreStarredFollows(ctx, userID); err != nil {
		return err
	}
	if s.OnEligibilityChange != nil {
		return s.OnEligibilityChange(ctx, tx, userID)
	}
	return nil
}

// Follow selects a starred repository for contribution discovery. It must be in the user's star snapshot, and
// access is checked live: public needs only the user's token, private needs the user and an Installation.
// Returns ErrNotFound (not starred, or no access) or *UnverifiedError (GitHub could not confirm); writes one repository and one selection row.
func (s *Service) Follow(ctx context.Context, userID, repoID int64) error {
	if starred, err := s.Queries.IsStarred(ctx, db.IsStarredParams{UserID: userID, RepositoryID: repoID}); err != nil {
		return err
	} else if !starred {
		return ErrNotFound
	}
	d := s.CheckOne(ctx, userID, repoID, true)
	if d.Unverified {
		return &UnverifiedError{RetryAt: d.RetryAt}
	}
	if !d.Allowed {
		return ErrNotFound
	}
	return s.storeSelection(ctx, userID, d.Repo, selectionSourceStarred)
}

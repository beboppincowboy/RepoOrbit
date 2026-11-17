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

// refreshStars replaces the snapshot only when every page was read. Any failure keeps the old one.
func (s *Service) refreshStars(ctx context.Context, userID int64, out *StarredPage) error {
	token, err := s.TokenSource.AccessToken(ctx, userID)
	if errors.Is(err, auth.ErrUnauthenticated) {
		return ErrSignedOut
	}
	if err != nil {
		log.Printf("repositories: token unavailable: %v", err)
		out.RefreshFailed = true
		return nil
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
		return ErrSignedOut
	case err != nil:
		out.RefreshFailed = true
		if errors.As(err, &apiErr) && apiErr.RateLimited() {
			out.RetryAt = &apiErr.RetryAt
		}
		return nil
	}
	return s.replaceStars(ctx, userID, all)
}

func (s *Service) replaceStars(ctx context.Context, userID int64, repos []githubapp.Repo) error {
	p := db.InsertStarsParams{UserID: userID}
	for _, r := range repos {
		p.Ids, p.FullNames, p.Descriptions, p.Privates = append(p.Ids, r.ID), append(p.FullNames, r.FullName), append(p.Descriptions, r.Description), append(p.Privates, r.Private)
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	// Concurrent refreshes for one user (a double page open) queue here instead of colliding on the primary key.
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
	if err := q.UpsertStarSnapshot(ctx, db.UpsertStarSnapshotParams{UserID: userID, RefreshedAt: pgtype.Timestamptz{Time: s.now(), Valid: true}}); err != nil {
		return err
	}
	// The snapshot is complete here, so follow eligibility changes with it: unstarred follows stop being polled
	// (the Selection stays), starred-again ones resume. Owned selections never depend on stars.
	if err := q.SuspendUnstarredFollows(ctx, db.SuspendUnstarredFollowsParams{UserID: userID, Now: pgtype.Timestamptz{Time: s.now(), Valid: true}}); err != nil {
		return err
	}
	if err := q.RestoreStarredFollows(ctx, userID); err != nil {
		return err
	}
	if s.OnStarsReplaced != nil {
		if err := s.OnStarsReplaced(ctx, tx, userID); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
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

package ingestion

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/repoorbit/repoorbit/backend/internal/accessfence"
	"github.com/repoorbit/repoorbit/backend/internal/auth"
	"github.com/repoorbit/repoorbit/backend/internal/db"
	"github.com/repoorbit/repoorbit/backend/internal/githubapp"
	"github.com/repoorbit/repoorbit/backend/internal/jobs"
	"github.com/repoorbit/repoorbit/backend/internal/repositories"
)

// Safe, fixed codes stored in repository_sync.last_error_code (never raw errors or credential identities).
const (
	CodeNoCredential = "no_credential"
	CodeNotFound     = "not_found"
	CodeRateLimited  = "rate_limited"
	CodeUnverified   = "access_unverified"
	CodeAccessMoved  = "access_changed"
	CodeUpstream     = "upstream_error"
)

// raceRetry is how soon a refresh whose commit was withheld (selection or authorization changed mid-flight) is retried.
const raceRetry = 5 * time.Second

// TokenSource is the part of auth.Service the refresh needs (and repositories.TokenSource).
type TokenSource interface {
	AccessToken(ctx context.Context, userID int64) (string, error)
	Revoke(ctx context.Context, userID int64) error
}

// AccessChecker is repositories.Service's live access check (fresh, never from cache).
type AccessChecker interface {
	CheckOne(ctx context.Context, userID, id int64, fresh bool) repositories.Decision
}

// RepoFetcher reads one repository's metadata with a user token.
type RepoFetcher interface {
	FetchRepo(ctx context.Context, token string, id int64) (githubapp.Repo, error)
}

// Deps are the collaborators of the metadata refresh handler.
type Deps struct {
	Pool   *pgxpool.Pool
	GitHub RepoFetcher
	Tokens TokenSource
	Access AccessChecker
	Clock  jobs.Clock

	mu        sync.Mutex
	throttled map[int64]time.Time // user id -> do not use this credential before
}

// Register wires the handler into a worker.
func (d *Deps) Register(w *jobs.Worker) { w.Register(jobs.KindRepoMetadataRefresh, d.RefreshMetadata) }

func (d *Deps) throttledUntil(userID int64) time.Time {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.throttled[userID]
}

func (d *Deps) throttle(userID int64, until time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.throttled == nil {
		d.throttled = map[int64]time.Time{}
	}
	d.throttled[userID] = until
}

func parseRepoKey(key string) (int64, error) {
	id, err := strconv.ParseInt(strings.TrimPrefix(key, "repo:"), 10, 64)
	if err != nil || !strings.HasPrefix(key, "repo:") {
		return 0, fmt.Errorf("bad job key %q", key)
	}
	return id, nil
}

// RefreshMetadata is the repo_metadata_refresh handler. It refreshes name, description and visibility of one
// repository, shared by every user who selects it, using the first eligible selecting user's credential that
// works. It writes only through fence.Commit, after rechecking the selection and (private repos) the access
// fence, and makes no network call inside the commit. It never imports issues or activity.
func (d *Deps) RefreshMetadata(ctx context.Context, job jobs.Job, fence jobs.Fence) error {
	repoID, err := parseRepoKey(job.Key)
	if err != nil {
		return jobs.Permanent(err)
	}
	q := db.New(d.Pool)
	repo, err := q.GetSyncRepository(ctx, repoID)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && repo.IsDemo) {
		return nil // nothing to refresh
	}
	if err != nil {
		return err
	}
	users, err := q.ListEligibleSelectors(ctx, repoID)
	if err != nil {
		return err
	}
	if len(users) == 0 {
		return nil // final deselection: no ordinary work
	}

	var transient, notFound bool
	for _, uid := range users {
		if until := d.throttledUntil(uid); until.After(d.Clock.Now()) {
			return d.deferred(ctx, fence, repoID, until) // pace the credential; never rotate to dodge a limit
		}
		token, err := d.Tokens.AccessToken(ctx, uid)
		if errors.Is(err, auth.ErrUnauthenticated) {
			continue
		}
		if err != nil {
			log.Printf("ingestion: token unavailable: %v", err)
			transient = true
			continue
		}
		var snap accessfence.Snapshot
		if repo.IsPrivate {
			if snap, err = d.takeFence(ctx, uid, repoID); err != nil {
				return err
			}
			switch dec := d.Access.CheckOne(ctx, uid, repoID, true); {
			case dec.Unverified && !dec.RetryAt.IsZero():
				d.throttle(uid, dec.RetryAt)
				return d.deferred(ctx, fence, repoID, dec.RetryAt)
			case dec.Unverified:
				transient = true
				continue
			case !dec.Allowed:
				continue
			}
		}
		fetched, err := d.GitHub.FetchRepo(ctx, token, repoID)
		var apiErr *githubapp.APIError
		switch {
		case errors.As(err, &apiErr) && apiErr.Unauthorized():
			if rerr := d.Tokens.Revoke(ctx, uid); rerr != nil { // explicit revocation disables this credential
				log.Printf("ingestion: revoke failed: %v", rerr)
			}
			continue
		case errors.As(err, &apiErr) && apiErr.RateLimited():
			d.throttle(uid, apiErr.RetryAt)
			return d.deferred(ctx, fence, repoID, apiErr.RetryAt)
		case errors.As(err, &apiErr) && apiErr.NotFound():
			notFound = true
			continue
		case err != nil:
			d.record(ctx, fence, repoID, CodeUpstream)
			return fmt.Errorf("fetch repository: %w", err)
		}
		return d.commit(ctx, fence, repoID, uid, repo.IsPrivate, snap, fetched)
	}
	switch {
	case transient:
		d.record(ctx, fence, repoID, CodeUnverified)
		return errors.New("credential or access could not be verified")
	case notFound:
		d.record(ctx, fence, repoID, CodeNotFound)
		return &jobs.Pause{Reason: CodeNotFound}
	}
	d.record(ctx, fence, repoID, CodeNoCredential)
	return &jobs.Pause{Reason: CodeNoCredential}
}

func (d *Deps) deferred(ctx context.Context, fence jobs.Fence, repoID int64, until time.Time) error {
	d.record(ctx, fence, repoID, CodeRateLimited)
	return &jobs.Defer{Until: until, Reason: CodeRateLimited}
}

// takeFence snapshots authorization epochs before any private remote read.
func (d *Deps) takeFence(ctx context.Context, userID, repoID int64) (accessfence.Snapshot, error) {
	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		return accessfence.Snapshot{}, err
	}
	defer tx.Rollback(ctx)
	snap, err := accessfence.Take(ctx, tx, userID, repoID)
	if err != nil {
		return accessfence.Snapshot{}, err
	}
	return snap, tx.Commit(ctx)
}

// record stores a safe status code through the fence. Best effort: a lost claim just means someone else owns the job.
func (d *Deps) record(ctx context.Context, fence jobs.Fence, repoID int64, code string) {
	err := fence.Commit(ctx, func(tx pgx.Tx) error {
		return db.New(tx).RecordSyncError(ctx, db.RecordSyncErrorParams{RepositoryID: repoID, LastErrorCode: pgtype.Text{String: code, Valid: true}})
	})
	if err != nil {
		log.Printf("ingestion: record status: %v", err)
	}
}

var (
	errNoSelection = errors.New("no eligible selection")
	errRace        = errors.New("selection or access changed")
)

// commit applies fetched metadata inside the job fence's transaction after rechecking that the credential's
// selection is still eligible and, for private repositories, that no authorization epoch moved since the fetch.
func (d *Deps) commit(ctx context.Context, fence jobs.Fence, repoID, uid int64, wasPrivate bool, snap accessfence.Snapshot, r githubapp.Repo) error {
	privatized := false
	err := fence.Commit(ctx, func(tx pgx.Tx) error {
		q := db.New(tx)
		users, err := q.ListEligibleSelectors(ctx, repoID)
		if err != nil {
			return err
		}
		if len(users) == 0 {
			return errNoSelection
		}
		mine := false
		for _, u := range users {
			mine = mine || u == uid
		}
		if !mine {
			return errRace // the credential's owner stopped following; pick again
		}
		if wasPrivate {
			if ok, err := snap.Valid(ctx, tx); err != nil {
				return err
			} else if !ok {
				return errRace
			}
		} else if r.Private {
			// Became private since last seen: this credential was never verified for private access.
			// Record only the tightened flag and re-run through the private path.
			privatized = true
			return q.MarkRepositoryPrivate(ctx, repoID)
		}
		if err := q.UpdateRepositoryMetadata(ctx, db.UpdateRepositoryMetadataParams{ID: repoID, Name: r.FullName, Description: r.Description, IsPrivate: r.Private}); err != nil {
			return err
		}
		return q.RecordSyncSuccess(ctx, db.RecordSyncSuccessParams{RepositoryID: repoID, LastSuccessAt: pgtype.Timestamptz{Time: d.Clock.Now(), Valid: true}})
	})
	switch {
	case errors.Is(err, errNoSelection):
		return nil
	case errors.Is(err, errRace):
		d.record(ctx, fence, repoID, CodeAccessMoved)
		return &jobs.Defer{Until: d.Clock.Now().Add(raceRetry), Reason: CodeAccessMoved}
	case err == nil && privatized:
		return &jobs.Defer{Until: d.Clock.Now(), Reason: "visibility_changed"}
	}
	return err
}

package ingestion

import (
	"context"
	"crypto/rsa"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
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
	Pool                    *pgxpool.Pool
	GitHub                  RepoFetcher
	Tokens                  TokenSource
	Access                  AccessChecker
	Clock                   jobs.Clock
	Inventory               AppInventory
	AppKey                  *rsa.PrivateKey
	InstallationCredentials *githubapp.InstallationCredentials
}

// Register wires the handler into a worker.
func (d *Deps) Register(w *jobs.Worker) {
	w.Register(jobs.KindRepoMetadataRefresh, d.RefreshMetadata)
	w.Register(jobs.KindActivityCleanup, d.CleanupPrivate)
	w.Register(jobs.KindActivityIssues, d.SyncIssues)
	w.Register(jobs.KindActivityPullRequests, d.SyncPullRequests)
	w.Register(jobs.KindActivityReleases, d.SyncReleases)
	w.Register(jobs.KindActivityIssueEvents, d.SyncIssueEvents)
	w.Register(jobs.KindActivityReviews, d.SyncReviews)
	w.Register(jobs.KindActivityComments, d.SyncComments)
	w.Register(jobs.KindStatusLabelCleanup, d.CleanupStatusLabels)
}

func (d *Deps) throttledUntil(ctx context.Context, userID int64) time.Time {
	return credentialThrottledUntil(ctx, db.New(d.Pool), userID)
}

func (d *Deps) throttle(ctx context.Context, userID int64, until time.Time) {
	throttleCredential(ctx, db.New(d.Pool), userID, until)
}

// parseKey reads the numeric id out of a job key of the form prefix+id ("repo:12", "user:7").
func parseKey(prefix, key string) (int64, error) {
	id, err := strconv.ParseInt(strings.TrimPrefix(key, prefix), 10, 64)
	if err != nil || !strings.HasPrefix(key, prefix) {
		return 0, fmt.Errorf("bad job key %q", key)
	}
	return id, nil
}

func parseRepoKey(key string) (int64, error) { return parseKey("repo:", key) }

// RefreshMetadata is the repo_metadata_refresh handler. It refreshes name, description and visibility of one
// repository, shared by every user who selects it, using the first eligible selecting user's credential that
// works. It writes only through fence.Commit, after rechecking the selection and (private repos) the access
// fence, and makes no network call inside the commit. It never imports issues or activity.
func (d *Deps) RefreshMetadata(ctx context.Context, job jobs.Job, fence jobs.ClaimFence) error {
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
		if until := d.throttledUntil(ctx, uid); until.After(d.Clock.Now()) {
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
			if snap, err = accessfence.Take(ctx, d.Pool, uid, repoID); err != nil {
				return err
			}
			switch dec := d.Access.CheckOne(ctx, uid, repoID, true); {
			case dec.Unverified && !dec.RetryAt.IsZero():
				d.throttle(ctx, uid, dec.RetryAt)
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
			d.throttle(ctx, uid, apiErr.RetryAt)
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

func (d *Deps) deferred(ctx context.Context, fence jobs.ClaimFence, repoID int64, until time.Time) error {
	d.record(ctx, fence, repoID, CodeRateLimited)
	return &jobs.Defer{RetryAt: until, Reason: CodeRateLimited}
}

// record stores a safe status code through the fence. Best effort: a lost claim just means someone else owns the job.
func (d *Deps) record(ctx context.Context, fence jobs.ClaimFence, repoID int64, code string) {
	err := fence.Commit(ctx, func(tx pgx.Tx) error {
		return db.New(tx).RecordSyncError(ctx, db.RecordSyncErrorParams{RepositoryID: repoID, LastErrorCode: pgtype.Text{String: code, Valid: true}})
	})
	if err != nil {
		log.Printf("ingestion: record status: %v", err)
	}
}

// isLockNotAvailable reports PostgreSQL's lock_not_available (55P03), raised by NOWAIT locks.
func isLockNotAvailable(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "55P03"
}

var (
	errNoSelection = errors.New("no eligible selection")
	errRace        = errors.New("selection or access changed")
)

// commit applies fetched metadata inside the job fence's transaction after checking that, for private repositories,
// no authorization epoch moved since the fetch, and, last, that the credential's selection is still eligible.
// Lock order is job row, fence rows, repository rows, then the selection row (NOWAIT): the same repository-then-
// selection order a reselect takes. Taking the selection lock last still precedes the commit, so a deselection cannot
// slip in, and a failed check rolls the repository writes back with the transaction.
func (d *Deps) commit(ctx context.Context, fence jobs.ClaimFence, repoID, uid int64, wasPrivate bool, snap accessfence.Snapshot, r githubapp.Repo) error {
	privatized := false
	err := fence.Commit(ctx, func(tx pgx.Tx) error {
		q := db.New(tx)
		if wasPrivate {
			if ok, err := snap.StillCurrent(ctx, tx); err != nil {
				return err
			} else if !ok {
				return errRace
			}
		}
		if !wasPrivate && r.Private {
			// Became private since last seen: this credential was never verified for private access.
			// Record only the tightened flag and re-run through the private path.
			privatized = true
			if err := q.MarkRepositoryPrivate(ctx, repoID); err != nil {
				return err
			}
			if err := accessfence.InvalidateAccess(ctx, tx, accessfence.ScopeRepository, repoID); err != nil {
				return err
			}
		} else {
			if err := q.UpdateRepositoryMetadata(ctx, db.UpdateRepositoryMetadataParams{ID: repoID, Name: r.FullName, Description: r.Description, IsPrivate: r.Private}); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE repositories SET archived=$2 WHERE id=$1 AND is_demo=false`, repoID, r.Archived); err != nil {
				return err
			}
			if err := q.RecordSyncSuccess(ctx, db.RecordSyncSuccessParams{RepositoryID: repoID, LastSuccessAt: pgtype.Timestamptz{Time: d.Clock.Now(), Valid: true}}); err != nil {
				return err
			}
		}
		return d.requireOwnerStillFollows(ctx, q, repoID, uid)
	})
	switch {
	case errors.Is(err, errNoSelection):
		return nil
	case errors.Is(err, errRace):
		d.record(ctx, fence, repoID, CodeAccessMoved)
		return &jobs.Defer{RetryAt: d.Clock.Now().Add(raceRetry), Reason: CodeAccessMoved}
	case err == nil && privatized:
		return &jobs.Defer{RetryAt: d.Clock.Now(), Reason: "visibility_changed"}
	}
	return err
}

// requireOwnerStillFollows share-locks the credential owner's eligible selection (NOWAIT) so a deselection or
// suspension cannot commit between this check and the end of the transaction. errNoSelection means nobody follows the
// repository any more; errRace means the owner stopped (or is stopping) but someone else follows, so pick again.
func (d *Deps) requireOwnerStillFollows(ctx context.Context, q *db.Queries, repoID, uid int64) error {
	_, err := q.LockEligibleSelection(ctx, db.LockEligibleSelectionParams{RepositoryID: repoID, UserID: uid})
	switch {
	case err == nil:
		return nil
	case errors.Is(err, pgx.ErrNoRows):
		users, err := q.ListEligibleSelectors(ctx, repoID)
		if err != nil {
			return err
		}
		if len(users) == 0 {
			return errNoSelection
		}
		return errRace
	case isLockNotAvailable(err):
		return errRace // a deselection or suspension is in flight; retry shortly instead of waiting
	}
	return err
}

// checkActivityAccess verifies both grants for the repository using the page's user token.
func (d *Deps) checkActivityAccess(ctx context.Context, uid, repoID int64, token string) repositories.Decision {
	client, ok := d.GitHub.(*githubapp.Client)
	if !ok {
		return d.Access.CheckOne(ctx, uid, repoID, true)
	}
	repo, err := client.FetchRepo(ctx, token, repoID)
	var api *githubapp.APIError
	switch {
	case errors.As(err, &api) && api.Unauthorized():
		_ = d.Tokens.Revoke(ctx, uid)
		return repositories.Decision{}
	case errors.As(err, &api) && api.RateLimited():
		return repositories.Decision{Unverified: true, RetryAt: api.RetryAt}
	case errors.As(err, &api) && api.NotFound():
		return repositories.Decision{}
	case err != nil || repo.ID != repoID || repo.FullName == "":
		return repositories.Decision{Unverified: true}
	}
	var installation int64
	if d.AppKey != nil {
		installation, err = client.RepositoryInstallation(ctx, d.AppKey, repo.FullName)
		if err != nil {
			return repositories.Decision{Unverified: true}
		}
	}
	if repo.Private && (installation == 0 || d.InstallationCredentials == nil) {
		return repositories.Decision{}
	}
	return repositories.Decision{Allowed: true, Repo: repo, InstallationID: installation}
}

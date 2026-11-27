// Package repositories decides which GitHub repositories a signed-in user may read and lets them select
// their own. Access is never read from stored rows: every decision comes from GitHub (fail closed).
package repositories

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/repoorbit/repoorbit/backend/internal/auth"
	"github.com/repoorbit/repoorbit/backend/internal/db"
	"github.com/repoorbit/repoorbit/backend/internal/githubapp"
)

const (
	githubPerPage = 100 // largest page GitHub serves
	maxPages      = 10  // per listing; a longer listing is incomplete, so missing repos are unverified, not denied
)

// TokenSource is the part of auth.Service that access checks need.
type TokenSource interface {
	AccessToken(ctx context.Context, userID int64) (string, error)
	Revoke(ctx context.Context, userID int64) error
}

// Service answers "may this user read this repository?" from live GitHub data and serves the repository
// list/selection API. Reads reuse a per-user listing for TTL; writes always look again.
type Service struct {
	Queries     *db.Queries
	Pool        *pgxpool.Pool // for the few multi-statement transactions
	GitHub      *githubapp.Client
	TokenSource TokenSource
	Now         func() time.Time
	// TTL bounds how long one user's visible-repository listing is reused by reads. Writes bypass it. Zero uses 30s.
	TTL        time.Duration
	AppURL     string // browser-facing origin for redirects
	InstallURL string // where users install the GitHub App; empty when no app slug is configured
	// OnSelect runs inside the selection transaction after the repository and selection rows are written, so
	// durable scheduling (ingestion.RequestMetadataRefresh) commits or rolls back with the selection. Nil schedules nothing.
	OnSelect func(ctx context.Context, tx pgx.Tx, repoID int64) error
	// OnEligibilityChange runs inside any transaction that can change what a user may follow or poll with (a replaced star
	// snapshot, a selection, a connected installation), so paused work the change unblocks (ingestion.RecheckEligibility)
	// resumes atomically with it. Nil does nothing.
	OnEligibilityChange func(ctx context.Context, tx pgx.Tx, userID int64) error
	// SignalActivity runs inside the refresh transaction and returns an ingestion.ActivityOutcome string
	// ("accepted", "coalesced", "dead", "ineligible"). Nil makes the refresh route answer 404.
	SignalActivity func(ctx context.Context, tx pgx.Tx, repoID int64) (string, error)

	visibility visibilityCache
}

// Decision is the outcome of one access check. The zero value means denied (not found, or the user was signed out).
type Decision struct {
	Allowed        bool
	Repo           githubapp.Repo // current GitHub metadata; meaningful only when Allowed
	InstallationID int64          // viewer-visible installation covering Repo, when present
	// Unverified means access could not be checked (upstream error, rate limit). Content must be withheld,
	// but this is not a denial. RetryAt is non-zero when a rate limit says when to try again.
	Unverified bool
	RetryAt    time.Time
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// collect reads every page of a listing, up to maxPages. complete is false when it stopped early.
func collect[T any](fetch func(page int) ([]T, bool, error)) (all []T, complete bool, err error) {
	for page := 1; page <= maxPages; page++ {
		items, more, err := fetch(page)
		if err != nil {
			return all, false, err
		}
		all = append(all, items...)
		if !more {
			return all, true, nil
		}
	}
	return all, false, nil
}

// prepare returns a token and the visible set, or verdictForAll (non-nil) when neither is available: the same
// Decision then applies to every repository. Side effect: a user whose token GitHub rejects is revoked
// (signed out everywhere) before the denial is returned.
func (s *Service) prepare(ctx context.Context, userID int64, fresh bool) (token string, set *visibleSet, verdictForAll *Decision) {
	token, err := s.TokenSource.AccessToken(ctx, userID)
	switch {
	case errors.Is(err, auth.ErrUnauthenticated):
		return "", nil, &Decision{}
	case err != nil:
		log.Printf("repositories: token unavailable: %v", err)
		return "", nil, &Decision{Unverified: true}
	}
	set = s.visibility.visible(ctx, s, userID, token, fresh)
	if set.tokenRejected {
		s.revoke(ctx, userID)
		return "", nil, &Decision{}
	}
	return token, set, nil
}

func (s *Service) revoke(ctx context.Context, userID int64) {
	s.visibility.invalidate(userID)
	if err := s.TokenSource.Revoke(ctx, userID); err != nil {
		log.Printf("repositories: revoke failed: %v", err)
	}
}

// Check decides access for a batch of repositories (a list page) from one visible-set lookup. The result has an
// entry for every id in ids. fresh=true skips the TTL cache (use for writes). Calls GitHub, may log, and signs the
// user out everywhere (Revoke) if GitHub rejects their token. A repository
// missing from an incomplete set is unverified. One missing from a complete set is either gone or a public
// repository the user does not own (a followed star), so GitHub is asked directly, once per TTL.
func (s *Service) Check(ctx context.Context, userID int64, ids []int64, fresh bool) map[int64]Decision {
	out := make(map[int64]Decision, len(ids))
	if len(ids) == 0 {
		return out // nothing to decide; do not spend GitHub calls
	}
	token, set, verdictForAll := s.prepare(ctx, userID, fresh)
	for _, id := range ids {
		switch r, ok := set.lookup(id); {
		case verdictForAll != nil:
			out[id] = *verdictForAll
		case ok:
			out[id] = Decision{Allowed: true, Repo: r, InstallationID: set.installations[id]}
		case set.complete:
			// ponytail: serial lookups, at most one page of them per TTL; batch or fan out if a page of follows gets slow.
			out[id] = s.outside(ctx, userID, token, set, id)
		default:
			out[id] = Decision{Unverified: true, RetryAt: set.retryAt}
		}
	}
	return out
}

// outside decides a repository that is not in a complete visible set, caching the answer with the set.
func (s *Service) outside(ctx context.Context, userID int64, token string, set *visibleSet, id int64) Decision {
	set.mu.Lock()
	d, ok := set.direct[id]
	set.mu.Unlock()
	if ok {
		return d
	}
	d = s.lookupOne(ctx, userID, token, set, id)
	set.mu.Lock()
	if set.direct == nil {
		set.direct = map[int64]Decision{}
	}
	set.direct[id] = d
	set.mu.Unlock()
	return d
}

// CheckOne decides access for a single repository. fresh=true skips the TTL cache (use for writes). Same side
// effects as Check (calls GitHub, may revoke the user). Beyond the visible set it asks GitHub directly, which
// is how a public repository the user does not own (and the app is not installed on) is verified.
func (s *Service) CheckOne(ctx context.Context, userID, id int64, fresh bool) Decision {
	token, set, verdictForAll := s.prepare(ctx, userID, fresh)
	if verdictForAll != nil {
		return *verdictForAll
	}
	if set.retryAt.After(s.now()) {
		return Decision{Unverified: true, RetryAt: set.retryAt}
	}
	if r, ok := set.lookup(id); ok {
		return Decision{Allowed: true, Repo: r, InstallationID: set.installations[id]}
	}
	return s.lookupOne(ctx, userID, token, set, id)
}

// lookupOne asks GitHub about one repository that is not in the visible set.
func (s *Service) lookupOne(ctx context.Context, userID int64, token string, set *visibleSet, id int64) Decision {
	r, err := s.GitHub.FetchRepo(ctx, token, id)
	var apiErr *githubapp.APIError
	switch {
	case errors.As(err, &apiErr) && apiErr.Unauthorized():
		s.revoke(ctx, userID)
		return Decision{}
	case errors.As(err, &apiErr) && apiErr.NotFound():
		return Decision{}
	case errors.As(err, &apiErr) && apiErr.RateLimited():
		return Decision{Unverified: true, RetryAt: apiErr.RetryAt}
	case err != nil:
		return Decision{Unverified: true}
	case r.Private: // a private repo outside the user's installations is not readable, whatever GitHub says to others
		if set.complete {
			return Decision{}
		}
		return Decision{Unverified: true, RetryAt: set.retryAt}
	}
	return Decision{Allowed: true, Repo: r}
}

// CheckPrivatePresence checks one user's installation-visible repositories for cleanup. A denial is
// authoritative only for this user; callers must also check every other possible user and installation.
// Missing credentials and incomplete listings are unverified, never evidence for deletion.
func (s *Service) CheckPrivatePresence(ctx context.Context, userID, repoID int64) Decision {
	token, err := s.TokenSource.AccessToken(ctx, userID)
	if err != nil {
		return Decision{Unverified: true}
	}
	set := s.visibility.visible(ctx, s, userID, token, true)
	if set.tokenRejected {
		s.revoke(ctx, userID)
		return Decision{Unverified: true}
	}
	if repo, ok := set.lookup(repoID); ok {
		return Decision{Allowed: true, Repo: repo}
	}
	if !set.complete {
		return Decision{Unverified: true, RetryAt: set.retryAt}
	}
	return Decision{}
}

// Package repositories decides which GitHub repositories a signed-in user may read and lets them select
// their own. Access is never read from stored rows: every decision comes from GitHub (fail closed).
package repositories

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"

	"github.com/repoorbit/repoorbit/backend/internal/auth"
	"github.com/repoorbit/repoorbit/backend/internal/db"
	"github.com/repoorbit/repoorbit/backend/internal/githubapp"
)

const (
	defaultTTL = 30 * time.Second
	maxPages   = 10 // per listing; a longer listing is incomplete, so missing repos are unverified, not denied
)

// Tokens is the part of auth.Service that access checks need.
type Tokens interface {
	AccessToken(ctx context.Context, userID int64) (string, error)
	Revoke(ctx context.Context, userID int64) error
}

type Service struct {
	Q      *db.Queries
	GitHub *githubapp.Client
	Tokens Tokens
	Now    func() time.Time
	// TTL bounds how long one user's visible-repository listing is reused by reads. Writes bypass it. Zero uses 30s.
	TTL        time.Duration
	AppURL     string // browser-facing origin for redirects
	InstallURL string // where users install the GitHub App; empty when no app slug is configured

	mu      sync.Mutex
	entries map[int64]*entry
}

type entry struct {
	mu  sync.Mutex // held during a fetch so concurrent callers share it instead of repeating it
	set *visibleSet
	at  time.Time
}

// visibleSet is everything GitHub says the user can reach with the app: their own repositories plus those in
// their installations (user access intersected with installation). Incomplete means some may be missing.
type visibleSet struct {
	repos    map[int64]githubapp.Repo
	complete bool
	revoked  bool      // GitHub rejected the user's token
	retryAt  time.Time // rate limited until
}

// Decision is the outcome of one access check.
type Decision struct {
	Allowed bool
	Repo    githubapp.Repo // current GitHub metadata; valid when Allowed
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

func (s *Service) ttl() time.Duration {
	if s.TTL != 0 {
		return s.TTL
	}
	return defaultTTL
}

func (s *Service) entry(userID int64) *entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.entries == nil {
		s.entries = map[int64]*entry{}
	}
	if s.entries[userID] == nil {
		s.entries[userID] = &entry{}
	}
	return s.entries[userID]
}

func (s *Service) invalidate(userID int64) {
	e := s.entry(userID)
	e.mu.Lock()
	e.set = nil
	e.mu.Unlock()
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

// visible returns the user's visible set. A TTL-cached copy serves reads; fresh forces a new fetch.
func (s *Service) visible(ctx context.Context, userID int64, token string, fresh bool) *visibleSet {
	e := s.entry(userID)
	e.mu.Lock()
	defer e.mu.Unlock()
	if !fresh && e.set != nil && s.now().Sub(e.at) < s.ttl() {
		return e.set
	}
	set := s.fetchVisible(ctx, userID, token)
	e.set, e.at = nil, s.now()
	if set.complete { // never reuse a failed or partial listing
		e.set = set
	}
	return set
}

// ponytail: the lock is held across several HTTP calls; fine for one local user, switch to singleflight if it contends.
func (s *Service) fetchVisible(ctx context.Context, userID int64, token string) *visibleSet {
	set := &visibleSet{repos: map[int64]githubapp.Repo{}, complete: true}
	fail := func(err error) {
		set.complete = false
		var apiErr *githubapp.APIError
		if errors.As(err, &apiErr) {
			set.revoked = set.revoked || apiErr.Unauthorized()
			if apiErr.RateLimited() {
				set.retryAt = apiErr.RetryAt
			}
		}
	}
	add := func(repos []githubapp.Repo) {
		for _, r := range repos {
			set.repos[r.ID] = r
		}
	}
	owned, ok, err := collect(func(p int) ([]githubapp.Repo, bool, error) { return s.GitHub.OwnedRepos(ctx, token, p, 100) })
	add(owned)
	if err != nil {
		fail(err)
	} else if !ok {
		set.complete = false
	}
	installs, ok, err := collect(func(p int) ([]githubapp.Installation, bool, error) { return s.GitHub.Installations(ctx, token, p) })
	if err != nil {
		fail(err)
		return set
	}
	if !ok {
		set.complete = false
	}
	ids := make([]int64, 0, len(installs))
	for _, in := range installs {
		ids = append(ids, in.ID)
		repos, ok, err := collect(func(p int) ([]githubapp.Repo, bool, error) { return s.GitHub.InstallationRepos(ctx, token, in.ID, p) })
		add(repos)
		if err != nil {
			fail(err)
		} else if !ok {
			set.complete = false
		}
	}
	if set.complete { // only a complete listing proves an installation is gone
		if err := s.syncInstallations(ctx, userID, installs, ids); err != nil {
			log.Printf("repositories: installation sync failed: %v", err)
		}
	}
	return set
}

func (s *Service) syncInstallations(ctx context.Context, userID int64, installs []githubapp.Installation, ids []int64) error {
	if err := s.Q.PruneInstallations(ctx, db.PruneInstallationsParams{UserID: userID, Keep: ids}); err != nil {
		return err
	}
	for _, in := range installs {
		if err := s.Q.UpsertInstallation(ctx, db.UpsertInstallationParams{UserID: userID, InstallationID: in.ID, AccountLogin: in.Account.Login}); err != nil {
			return err
		}
	}
	return nil
}

// prepare returns a token and the visible set, or the decision that applies to every repository when neither
// is available. A revoked user is signed out everywhere before the denial is returned.
func (s *Service) prepare(ctx context.Context, userID int64, fresh bool) (token string, set *visibleSet, all *Decision) {
	token, err := s.Tokens.AccessToken(ctx, userID)
	switch {
	case errors.Is(err, auth.ErrUnauthenticated):
		return "", nil, &Decision{}
	case err != nil:
		log.Printf("repositories: token unavailable: %v", err)
		return "", nil, &Decision{Unverified: true}
	}
	set = s.visible(ctx, userID, token, fresh)
	if set.revoked {
		s.revoke(ctx, userID)
		return "", nil, &Decision{}
	}
	return token, set, nil
}

func (s *Service) revoke(ctx context.Context, userID int64) {
	s.invalidate(userID)
	if err := s.Tokens.Revoke(ctx, userID); err != nil {
		log.Printf("repositories: revoke failed: %v", err)
	}
}

// Check decides access for a batch of repositories (a list page) from one visible-set lookup. A repository
// missing from a complete set is denied; missing from an incomplete one is unverified. It never calls GitHub once per repo.
func (s *Service) Check(ctx context.Context, userID int64, ids []int64, fresh bool) map[int64]Decision {
	out := make(map[int64]Decision, len(ids))
	_, set, all := s.prepare(ctx, userID, fresh)
	for _, id := range ids {
		switch r, ok := set.lookup(id); {
		case all != nil:
			out[id] = *all
		case ok:
			out[id] = Decision{Allowed: true, Repo: r}
		case set.complete:
			out[id] = Decision{}
		default:
			out[id] = Decision{Unverified: true, RetryAt: set.retryAt}
		}
	}
	return out
}

func (v *visibleSet) lookup(id int64) (githubapp.Repo, bool) {
	if v == nil {
		return githubapp.Repo{}, false
	}
	r, ok := v.repos[id]
	return r, ok
}

// CheckOne decides access for a single-repository view. Beyond the visible set it asks GitHub directly, which
// is how a public repository the user does not own (and the app is not installed on) is verified.
func (s *Service) CheckOne(ctx context.Context, userID, id int64, fresh bool) Decision {
	token, set, all := s.prepare(ctx, userID, fresh)
	if all != nil {
		return *all
	}
	if r, ok := set.lookup(id); ok {
		return Decision{Allowed: true, Repo: r}
	}
	r, err := s.GitHub.Repo(ctx, token, id)
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

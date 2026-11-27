package repositories

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/repoorbit/repoorbit/backend/internal/accessfence"
	"github.com/repoorbit/repoorbit/backend/internal/db"
	"github.com/repoorbit/repoorbit/backend/internal/githubapp"
)

const defaultTTL = 30 * time.Second

// visibilityCache holds per-user GitHub listings. Its zero value is ready to use.
type visibilityCache struct {
	mu      sync.Mutex
	entries map[int64]*entry
}

type requestVisibilityKey struct{}

// WithRequestVisibility creates an empty visibility memo for one GraphQL request and its single viewer.
// The first read checks GitHub unless the user is cooling down. Later reads reuse the result while its access fence holds.
func WithRequestVisibility(ctx context.Context) context.Context {
	return context.WithValue(ctx, requestVisibilityKey{}, &entry{})
}

type entry struct {
	mu   sync.Mutex // held during a fetch so concurrent callers share it instead of repeating it
	set  *visibleSet
	at   time.Time
	snap accessfence.Snapshot // fence epochs from before the fetch; a bump since then ends the cached set
}

// visibleSet is everything GitHub says the user can reach with the app: their own repositories plus those in
// their installations (user access intersected with installation). Incomplete means some may be missing.
type visibleSet struct {
	repos         map[int64]githubapp.Repo
	installations map[int64]int64
	complete      bool
	tokenRejected bool      // GitHub answered 401 to the user's token
	retryAt       time.Time // rate limited until

	mu     sync.Mutex
	direct map[int64]Decision // repositories outside the set, looked up one by one; dies with the set
}

func (s *Service) ttl() time.Duration {
	if s.TTL != 0 {
		return s.TTL
	}
	return defaultTTL
}

func (c *visibilityCache) entry(userID int64) *entry {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = map[int64]*entry{}
	}
	if c.entries[userID] == nil {
		c.entries[userID] = &entry{}
	}
	return c.entries[userID]
}

func (c *visibilityCache) invalidate(userID int64) {
	e := c.entry(userID)
	e.mu.Lock()
	e.set = nil
	e.mu.Unlock()
}

// visible returns the user's visible set. A TTL-cached copy (complete or not) serves reads; fresh forces a new fetch.
func (c *visibilityCache) visible(ctx context.Context, s *Service, userID int64, token string, fresh bool) *visibleSet {
	e := c.entry(userID)
	e.mu.Lock()
	defer e.mu.Unlock()
	request, _ := ctx.Value(requestVisibilityKey{}).(*entry)
	if request != nil && request.set != nil && s.fenceHolds(ctx, request.snap) {
		return request.set
	}
	if e.set != nil && e.set.retryAt.After(s.now()) && s.fenceHolds(ctx, e.snap) {
		return &visibleSet{repos: map[int64]githubapp.Repo{}, complete: false, retryAt: e.set.retryAt}
	}
	if !fresh && e.set != nil && s.now().Sub(e.at) < s.ttl() && s.fenceHolds(ctx, e.snap) {
		return e.set
	}
	for attempt := 0; attempt < 2; attempt++ {
		snap, snapErr := accessfence.TakeSnapshotForUser(ctx, s.Pool, userID)
		set, selfBump := c.fetchVisible(ctx, s, userID, token, snap)
		if snapErr == nil && s.fenceHolds(ctx, snap) {
			e.set, e.at, e.snap = set, s.now(), snap
			if request != nil {
				request.set, request.snap = set, snap
			}
			return set
		}
		if !selfBump {
			break
		}
	}
	// An authorization change committed while this listing was being fetched (or the fence could not be read):
	// nothing fetched before it may be served, even once. Withhold as unverified and cache nothing.
	e.set = nil
	return &visibleSet{repos: map[int64]githubapp.Repo{}, complete: false}
}

// fenceHolds reports whether no authorization change happened since snap was taken. Fails closed on a read error.
func (s *Service) fenceHolds(ctx context.Context, snap accessfence.Snapshot) bool {
	ok, err := snap.Current(ctx, s.Pool)
	return ok && err == nil
}

// ponytail: the lock is held across several HTTP calls; fine for one local user, switch to singleflight if it contends.
func (c *visibilityCache) fetchVisible(ctx context.Context, s *Service, userID int64, token string, snap accessfence.Snapshot) (*visibleSet, bool) {
	set := &visibleSet{repos: map[int64]githubapp.Repo{}, installations: map[int64]int64{}, complete: true}
	fail := func(err error) {
		set.complete = false
		var apiErr *githubapp.APIError
		if errors.As(err, &apiErr) {
			set.tokenRejected = set.tokenRejected || apiErr.Unauthorized()
			if apiErr.RateLimited() {
				set.retryAt = apiErr.RetryAt
			}
		}
	}
	owned, ok, err := collect(func(p int) ([]githubapp.Repo, bool, error) {
		return s.GitHub.FetchOwnedRepos(ctx, token, p, githubPerPage)
	})
	for _, repo := range owned {
		if !repo.Private {
			set.repos[repo.ID] = repo
		}
	}
	if err != nil {
		fail(err)
	} else if !ok {
		set.complete = false
	}
	installs, ok, err := collect(func(p int) ([]githubapp.Installation, bool, error) { return s.GitHub.FetchInstallations(ctx, token, p) })
	if err != nil {
		fail(err)
		return set, false
	}
	if !ok {
		set.complete = false
	}
	ids := make([]int64, 0, len(installs))
	for _, in := range installs {
		ids = append(ids, in.ID)
		repos, ok, err := collect(func(p int) ([]githubapp.Repo, bool, error) {
			return s.GitHub.FetchInstallationRepos(ctx, token, in.ID, p)
		})
		for _, repo := range repos {
			set.repos[repo.ID] = repo
			set.installations[repo.ID] = in.ID
		}
		if err != nil {
			fail(err)
		} else if !ok {
			set.complete = false
		}
	}
	if set.complete {
		changed, err := c.syncInstallations(ctx, s, userID, installs, ids, snap)
		if err != nil {
			return &visibleSet{repos: map[int64]githubapp.Repo{}, complete: false}, false
		}
		return set, changed
	}
	return set, false
}

func (c *visibilityCache) syncInstallations(ctx context.Context, s *Service, userID int64, installs []githubapp.Installation, ids []int64, snap accessfence.Snapshot) (bool, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	// The global row serializes membership changes with purge and invalidation.
	if _, err = tx.Exec(ctx, `INSERT INTO access_fences(scope,subject_id) VALUES('global',0) ON CONFLICT DO NOTHING`); err != nil {
		return false, err
	}
	if _, err = tx.Exec(ctx, `SELECT 1 FROM access_fences WHERE scope='global' AND subject_id=0 FOR UPDATE`); err != nil {
		return false, err
	}
	current, err := snap.Current(ctx, tx)
	if err != nil || !current {
		return false, errors.New("access changed during listing")
	}
	rows, err := tx.Query(ctx, `SELECT installation_id FROM installations WHERE user_id=$1`, userID)
	if err != nil {
		return false, err
	}
	existing := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return false, err
		}
		existing[id] = true
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return false, err
	}
	changed := len(existing) != len(ids)
	for _, id := range ids {
		if !existing[id] {
			changed = true
		}
	}
	if changed {
		if err = accessfence.BumpAccess(ctx, tx, accessfence.ScopeUser, userID); err != nil {
			return false, err
		}
	}
	q := s.Queries.WithTx(tx)
	if err = q.PruneInstallations(ctx, db.PruneInstallationsParams{UserID: userID, Keep: ids}); err != nil {
		return false, err
	}
	for _, in := range installs {
		if err = q.UpsertInstallation(ctx, db.UpsertInstallationParams{UserID: userID, InstallationID: in.ID, AccountLogin: in.Account.Login}); err != nil {
			return false, err
		}
	}
	if changed && s.OnEligibilityChange != nil {
		if err := s.OnEligibilityChange(ctx, tx, userID); err != nil {
			return false, err
		}
	}
	return changed, tx.Commit(ctx)
}

func (v *visibleSet) lookup(id int64) (githubapp.Repo, bool) {
	if v == nil {
		return githubapp.Repo{}, false
	}
	r, ok := v.repos[id]
	return r, ok
}

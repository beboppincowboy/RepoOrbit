// Package accessfence is the durable authorization fence.
//
// Every authorization change (revocation, installation removal, repository privatized or deleted) bumps an
// epoch in PostgreSQL before slower cleanup. Remote fetches take a Snapshot first; the commit transaction
// re-checks it, so a result fetched under old authorization is never committed and process-local caches
// cannot outlive a bump. The fence only ever withholds; it never grants access.
package accessfence

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/repoorbit/repoorbit/backend/internal/db"
)

type Scope string

const (
	ScopeUser         Scope = "user"         // GitHub authorization revoked / signed out of GitHub
	ScopeInstallation Scope = "installation" // installation deleted/suspended, repos removed
	ScopeRepository   Scope = "repository"   // repository deleted/privatized/transferred
	// ScopeGlobal is one monotonic permissions version (subject 0) bumped by every InvalidateAccess and tracked by every
	// TakeSnapshot. It is the one row a bump and a private commit always contend on, so they serialize even when the
	// bumped subject was never saved locally.
	ScopeGlobal Scope = "global"
)

var globalKey = key{ScopeGlobal, 0}

// DB is what pgxpool.Pool and pgx.Tx both provide.
type DB interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

const bumpSQL = `INSERT INTO access_fences (scope, subject_id, epoch) VALUES ($1, $2, 1)
	ON CONFLICT (scope, subject_id) DO UPDATE SET epoch = access_fences.epoch + 1`

// InvalidateAccess durably invalidates access for a subject by incrementing its epoch, and the global permissions
// version, inside the caller's transaction (before any slower cleanup). The global row is locked first, then the
// subject row, the same order StillCurrent takes them. Never grants access.
// BumpAccess fences observed authorization restoration without scheduling cleanup.
func BumpAccess(ctx context.Context, tx pgx.Tx, scope Scope, subjectID int64) error {
	if _, err := tx.Exec(ctx, bumpSQL, string(ScopeGlobal), int64(0)); err != nil {
		return err
	}
	if scope == ScopeGlobal {
		return nil
	}
	_, err := tx.Exec(ctx, bumpSQL, string(scope), subjectID)
	return err
}

func InvalidateAccess(ctx context.Context, tx pgx.Tx, scope Scope, subjectID int64) error {
	if err := BumpAccess(ctx, tx, scope, subjectID); err != nil {
		return err
	}
	// Issue-write drafts of the actors who may have lost private access are wiped now, even where other users keep
	// access; the audit outcome and hashes stay. Global bumps (reconciliation) name no actor and wipe nothing.
	if _, err := tx.Exec(ctx, `UPDATE write_operations o SET draft = NULL FROM repositories r
		WHERE r.id = o.repository_id AND o.draft IS NOT NULL AND (
		 ($1::text = 'user' AND r.is_private AND o.user_id = $2) OR
		 ($1::text = 'installation' AND r.is_private AND o.user_id IN (SELECT user_id FROM installations WHERE installation_id = $2)) OR
		 ($1::text = 'repository' AND r.id = $2))`, string(scope), subjectID); err != nil {
		return err
	}
	// A durable row is independent of a worker claim and records concurrent invalidations.
	_, err := tx.Exec(ctx, `WITH targets AS (
		INSERT INTO activity_cleanup (repository_id)
		SELECT id FROM repositories WHERE NOT is_demo AND
        (($1 = 'repository' AND id = $2) OR ($1 <> 'repository' AND is_private))
		ON CONFLICT (repository_id) DO UPDATE SET version = activity_cleanup.version + 1
		RETURNING repository_id
	)
	INSERT INTO jobs (kind, key, state, run_at)
	SELECT 'activity_cleanup', 'repo:' || repository_id, 'queued', now() FROM targets
	ON CONFLICT (kind, key) DO UPDATE SET
	 run_at = CASE WHEN jobs.state = 'queued' THEN LEAST(jobs.run_at, EXCLUDED.run_at) ELSE jobs.run_at END,
	 pending = CASE WHEN jobs.state = 'queued' THEN jobs.pending ELSE true END, updated_at = now()`, string(scope), subjectID)
	return err
}

type key struct {
	scope Scope
	id    int64
}

// Snapshot captures epochs for a (user, repository) read before a remote fetch.
type Snapshot struct {
	epochs map[Scope]map[int64]int64
	coarse *int64 // TakeSnapshotForUser only: sum of every installation and repository epoch (any such bump invalidates)
}

func (s *Snapshot) set(k key, epoch int64) {
	if s.epochs == nil {
		s.epochs = map[Scope]map[int64]int64{}
	}
	if s.epochs[k.scope] == nil {
		s.epochs[k.scope] = map[int64]int64{}
	}
	s.epochs[k.scope][k.id] = epoch
}

func (s Snapshot) keys() (scopes []string, ids []int64) {
	for sc, m := range s.epochs {
		for id := range m {
			scopes, ids = append(scopes, string(sc)), append(ids, id)
		}
	}
	return
}

// current reads the live epoch of every tracked key (absent means 0). lock makes concurrent bumps wait.
func (s Snapshot) current(ctx context.Context, q DB, lock bool) (map[key]int64, error) {
	scopes, ids := s.keys()
	sql := `SELECT scope, subject_id, epoch FROM access_fences WHERE (scope, subject_id) IN (SELECT * FROM unnest($1::text[], $2::bigint[]))`
	if lock {
		sql += ` FOR SHARE`
	}
	rows, err := q.Query(ctx, sql, scopes, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[key]int64{}
	for rows.Next() {
		var sc string
		var id, e int64
		if err := rows.Scan(&sc, &id, &e); err != nil {
			return nil, err
		}
		out[key{Scope(sc), id}] = e
	}
	return out, rows.Err()
}

func (s Snapshot) same(live map[key]int64) bool {
	for sc, m := range s.epochs {
		for id, e := range m {
			if live[key{sc, id}] != e {
				return false
			}
		}
	}
	return true
}

func coarseSum(ctx context.Context, q DB) (int64, error) {
	rows, err := q.Query(ctx, `SELECT COALESCE(sum(epoch), 0)::bigint FROM access_fences WHERE scope IN ('installation', 'repository')`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var n int64
	if rows.Next() {
		err = rows.Scan(&n)
	}
	if err != nil {
		return 0, err
	}
	return n, rows.Err()
}

func userKeys(ctx context.Context, q DB, userID int64) ([]key, error) {
	rows, err := q.Query(ctx, `SELECT installation_id FROM installations WHERE user_id = $1`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ks := []key{{ScopeUser, userID}}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ks = append(ks, key{ScopeInstallation, id})
	}
	return ks, rows.Err()
}

// TakeSnapshot records current epochs for userID, repoID, every installation the user holds, and the global
// permissions version (so a bump of ANY subject, including an installation not yet saved locally, ends it). It creates
// the fence rows it tracks so that StillCurrent can wait on a concurrent InvalidateAccess.
func TakeSnapshot(ctx context.Context, q pgx.Tx, userID, repoID int64) (Snapshot, error) {
	ks, err := userKeys(ctx, q, userID)
	if err != nil {
		return Snapshot{}, err
	}
	ks = append(ks, key{ScopeRepository, repoID}, globalKey)
	var s Snapshot
	for _, k := range ks {
		if _, err := q.Exec(ctx, `INSERT INTO access_fences (scope, subject_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`, string(k.scope), k.id); err != nil {
			return Snapshot{}, err
		}
		s.set(k, 0)
	}
	live, err := s.current(ctx, q, false)
	if err != nil {
		return Snapshot{}, err
	}
	for _, k := range ks {
		s.set(k, live[k])
	}
	return s, nil
}

// Take is TakeSnapshot in its own short transaction: what a job calls before a private remote fetch, so it never
// handles the transaction itself.
func Take(ctx context.Context, pool *pgxpool.Pool, userID, repoID int64) (s Snapshot, err error) {
	err = db.InTx(ctx, pool, func(tx pgx.Tx) (err error) {
		s, err = TakeSnapshot(ctx, tx, userID, repoID)
		return err
	})
	return s, err
}

// TakeSnapshotForUser snapshots what a cached read of everything the user can see depends on: the user, their
// installations, and (coarsely) any installation or repository bump. Check it with Current. ponytail: an installation or repository bump
// drops every user's cached listing once (the user's installation rows may not exist yet on a first fetch);
// per-subject tracking if bumps ever get frequent.
func TakeSnapshotForUser(ctx context.Context, q DB, userID int64) (Snapshot, error) {
	ks, err := userKeys(ctx, q, userID)
	if err != nil {
		return Snapshot{}, err
	}
	var s Snapshot
	for _, k := range ks {
		s.set(k, 0)
	}
	live, err := s.current(ctx, q, false)
	if err != nil {
		return Snapshot{}, err
	}
	for _, k := range ks {
		s.set(k, live[k])
	}
	sum, err := coarseSum(ctx, q)
	if err != nil {
		return Snapshot{}, err
	}
	s.coarse = &sum
	return s, nil
}

// StillCurrent reports, inside the commit transaction, that no tracked epoch (including the global permissions
// version) moved since TakeSnapshot. It share-locks the global row first, then the rest, and so blocks behind any
// in-flight InvalidateAccess (no lock timeout). Private commits and cached private reads must check it; false means withhold the update.
func (s Snapshot) StillCurrent(ctx context.Context, tx pgx.Tx) (bool, error) {
	if _, tracked := s.epochs[ScopeGlobal]; tracked {
		// Global row first, in its own statement: the order InvalidateAccess takes its locks, so the two cannot deadlock.
		if _, err := tx.Exec(ctx, `SELECT 1 FROM access_fences WHERE scope = 'global' AND subject_id = 0 FOR SHARE`); err != nil {
			return false, err
		}
	}
	live, err := s.current(ctx, tx, true)
	if err != nil {
		return false, err
	}
	return s.same(live), nil
}

// Current is StillCurrent without locking, for cached reads outside a transaction.
func (s Snapshot) Current(ctx context.Context, q DB) (bool, error) {
	live, err := s.current(ctx, q, false)
	if err != nil || !s.same(live) {
		return false, err
	}
	if s.coarse != nil {
		sum, err := coarseSum(ctx, q)
		return err == nil && sum == *s.coarse, err
	}
	return true, nil
}

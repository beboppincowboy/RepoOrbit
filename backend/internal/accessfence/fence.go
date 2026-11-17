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
)

type Scope string

const (
	ScopeUser         Scope = "user"         // GitHub authorization revoked / signed out of GitHub
	ScopeInstallation Scope = "installation" // installation deleted/suspended, repos removed
	ScopeRepository   Scope = "repository"   // repository deleted/privatized/transferred
)

// DB is what pgxpool.Pool and pgx.Tx both provide.
type DB interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Bump durably invalidates access for a subject by incrementing its epoch, inside the
// caller's transaction (before any slower cleanup). Never grants access.
func Bump(ctx context.Context, tx pgx.Tx, scope Scope, subjectID int64) error {
	_, err := tx.Exec(ctx, `INSERT INTO access_fences (scope, subject_id, epoch) VALUES ($1, $2, 1)
		ON CONFLICT (scope, subject_id) DO UPDATE SET epoch = access_fences.epoch + 1`, string(scope), subjectID)
	return err
}

type key struct {
	scope Scope
	id    int64
}

// Snapshot captures epochs for a (user, repository) read before a remote fetch.
type Snapshot struct {
	epochs map[Scope]map[int64]int64
	coarse *int64 // TakeForUser only: sum of every installation and repository epoch (any such bump invalidates)
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

// Take records current epochs for userID, repoID and every installation the user holds. It creates the
// fence rows it tracks so that Valid can wait on a concurrent Bump.
func Take(ctx context.Context, q pgx.Tx, userID, repoID int64) (Snapshot, error) {
	ks, err := userKeys(ctx, q, userID)
	if err != nil {
		return Snapshot{}, err
	}
	ks = append(ks, key{ScopeRepository, repoID})
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

// TakeForUser snapshots what a cached read of everything the user can see depends on: the user, their
// installations, and (coarsely) any installation or repository bump. Check it with Current. ponytail: an installation or repository bump
// drops every user's cached listing once (the user's installation rows may not exist yet on a first fetch);
// per-subject tracking if bumps ever get frequent.
func TakeForUser(ctx context.Context, q DB, userID int64) (Snapshot, error) {
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

// Valid reports, inside the commit transaction, that no tracked epoch moved since Take. It waits for any
// in-flight Bump. Private commits and cached private reads must check it; false means withhold the update.
func (s Snapshot) Valid(ctx context.Context, tx pgx.Tx) (bool, error) {
	live, err := s.current(ctx, tx, true)
	if err != nil {
		return false, err
	}
	return s.same(live), nil
}

// Current is Valid without locking, for cached reads outside a transaction.
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

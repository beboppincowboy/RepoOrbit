package graph

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/repoorbit/repoorbit/backend/internal/accessfence"
	"github.com/repoorbit/repoorbit/backend/internal/db"
	"github.com/repoorbit/repoorbit/backend/internal/repositories"
)

type listQueryArgs struct {
	ids   []int64
	limit int
	after int64
}

func parseListArgs(raw []string, first int, after *string) (listQueryArgs, error) {
	var args listQueryArgs
	var err error
	if args.limit, err = pageLimit(first); err != nil {
		return args, err
	}
	if args.after, err = parsePositiveID(after); err != nil {
		return args, err
	}
	if len(raw) > maxRepositoryIDs {
		return args, fmt.Errorf("at most %d repositories", maxRepositoryIDs)
	}
	args.ids = make([]int64, 0, len(raw))
	for _, s := range raw {
		id, err := parsePositiveID(&s)
		if err != nil {
			return args, err
		}
		args.ids = append(args.ids, id)
	}
	return args, nil
}

type guardedAuth struct {
	userID   int64
	ids      []int64
	withheld int
	retryAt  time.Time
	fence    accessfence.Snapshot
	checks   map[int64]repositories.Decision
}

func (a guardedAuth) retryAtString() *string {
	if a.retryAt.IsZero() {
		return nil
	}
	s := stamp(a.retryAt)
	return &s
}

func (r *Resolver) authorizeSelected(ctx context.Context, rawIDs []int64) (guardedAuth, error) {
	var a guardedAuth
	user, err := r.userID(ctx)
	if err != nil {
		return a, err
	}
	if len(rawIDs) == 0 {
		return guardedAuth{userID: user}, nil
	}
	selected, err := r.Queries.ListSelectedIDs(ctx, db.ListSelectedIDsParams{UserID: user, RepositoryIds: rawIDs})
	if err != nil {
		return a, fmt.Errorf("database unavailable")
	}
	if len(selected) == 0 {
		return guardedAuth{userID: user}, nil
	}
	snap, err := accessfence.TakeSnapshotForUser(ctx, r.RepoAccess.Pool, user)
	if err != nil {
		return a, fmt.Errorf("database unavailable")
	}
	return r.finishAuth(ctx, user, selected, snap), nil
}

func (r *Resolver) authorizePreselected(ctx context.Context, user int64, selected []int64) (guardedAuth, error) {
	var a guardedAuth
	snap, err := accessfence.TakeSnapshotForUser(ctx, r.RepoAccess.Pool, user)
	if err != nil {
		return a, fmt.Errorf("database unavailable")
	}
	return r.finishAuth(ctx, user, selected, snap), nil
}

func (r *Resolver) finishAuth(ctx context.Context, user int64, selected []int64, snap accessfence.Snapshot) guardedAuth {
	checks := r.RepoAccess.Check(ctx, user, selected, true)
	allowed := make([]int64, 0, len(selected))
	withheld := 0
	var retryAt time.Time
	for _, id := range selected {
		d := checks[id]
		if d.Allowed {
			allowed = append(allowed, id)
		}
		if d.Unverified {
			withheld++
			if !d.RetryAt.IsZero() && (retryAt.IsZero() || d.RetryAt.After(retryAt)) {
				retryAt = d.RetryAt
			}
		}
	}
	return guardedAuth{
		userID:   user,
		ids:      allowed,
		withheld: withheld,
		retryAt:  retryAt,
		fence:    snap,
		checks:   checks,
	}
}

func guardedRead[T any](
	ctx context.Context,
	pool *pgxpool.Pool,
	auth guardedAuth,
	read func() (T, error),
	onStale func() (T, error),
) (T, error) {
	val, err := read()
	if err != nil {
		var zero T
		return zero, err
	}
	current, err := auth.fence.Current(ctx, pool)
	if err != nil || !current {
		return onStale()
	}
	return val, nil
}

type pageData[N any] struct {
	nodes       []*N
	endCursor   *string
	hasNextPage bool
}

func queryEvidencePage[N any](
	ctx context.Context,
	pool *pgxpool.Pool,
	limit int,
	sql string,
	args []any,
	scanRow func(pgx.Rows) (int64, *N, error),
) (pageData[N], error) {
	rows, err := pool.Query(ctx, sql, args...)
	if err != nil {
		return pageData[N]{}, fmt.Errorf("database unavailable")
	}
	defer rows.Close()
	out := pageData[N]{nodes: make([]*N, 0, limit)}
	for rows.Next() {
		if len(out.nodes) == limit {
			out.hasNextPage = true
			break
		}
		id, node, err := scanRow(rows)
		if err != nil {
			return pageData[N]{}, fmt.Errorf("database unavailable")
		}
		cursor := strconv.FormatInt(id, 10)
		out.endCursor = &cursor
		out.nodes = append(out.nodes, node)
	}
	if rows.Err() != nil {
		return pageData[N]{}, fmt.Errorf("database unavailable")
	}
	return out, nil
}

func readGuardedEvidencePage[N any, C any](
	r *queryResolver,
	ctx context.Context,
	args listQueryArgs,
	sql string,
	scanRow func(pgx.Rows) (int64, *N, error),
	buildConn func(page pageData[N], withheld int, retryAt *string) C,
) (C, error) {
	var zero C
	auth, err := r.authorizeSelected(ctx, args.ids)
	if err != nil {
		return zero, err
	}
	withhold := func() (C, error) {
		return buildConn(pageData[N]{nodes: []*N{}}, auth.withheld+len(auth.ids), auth.retryAtString()), nil
	}
	if len(auth.ids) == 0 {
		return withhold()
	}
	return guardedRead(ctx, r.RepoAccess.Pool, auth, func() (C, error) {
		page, err := queryEvidencePage(ctx, r.RepoAccess.Pool, args.limit, sql, []any{auth.ids, args.after, args.limit + 1}, scanRow)
		if err != nil {
			return zero, err
		}
		return buildConn(page, auth.withheld, auth.retryAtString()), nil
	}, withhold)
}

func readEvidenceList[N any, C any](
	r *queryResolver,
	ctx context.Context,
	ids []string,
	first int,
	after *string,
	sql string,
	scanRow func(pgx.Rows) (int64, *N, error),
	buildConn func(page pageData[N], withheld int, retryAt *string) C,
) (C, error) {
	var zero C
	ctx, cancel := withReadDeadline(ctx)
	defer cancel()
	args, err := parseListArgs(ids, first, after)
	if err != nil {
		return zero, err
	}
	if r.Demo {
		return buildConn(pageData[N]{nodes: []*N{}}, 0, nil), nil
	}
	return readGuardedEvidencePage(r, ctx, args, sql, scanRow, buildConn)
}

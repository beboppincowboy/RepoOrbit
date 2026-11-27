package graph

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/repoorbit/repoorbit/backend/internal/readguard"
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

func retryAtString(t time.Time) *string {
	if t.IsZero() {
		return nil
	}
	s := stamp(t)
	return &s
}

func (r *Resolver) readGuard() *readguard.Guard {
	return &readguard.Guard{Pool: r.RepoAccess.Pool, Queries: r.Queries, Access: r.RepoAccess}
}

func (r *Resolver) authorizeSelected(ctx context.Context, rawIDs []int64) (readguard.Auth, error) {
	user, err := r.userID(ctx)
	if err != nil {
		return readguard.Auth{}, err
	}
	return r.readGuard().AuthorizeSelected(ctx, user, rawIDs)
}

func (r *Resolver) authorizePreselected(ctx context.Context, user int64, selected []int64) (readguard.Auth, error) {
	return r.readGuard().AuthorizePreselected(ctx, user, selected)
}

func (r *Resolver) fenceOnly(ctx context.Context, user int64) (readguard.Auth, error) {
	return r.readGuard().FenceOnly(ctx, user)
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
	rows.Close()
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
		return buildConn(pageData[N]{nodes: []*N{}}, auth.Withheld+len(auth.Allowed), retryAtString(auth.RetryAt)), nil
	}
	if len(auth.Allowed) == 0 {
		return withhold()
	}
	return readguard.Run(ctx, r.RepoAccess.Pool, auth, func() (C, error) {
		page, err := queryEvidencePage(ctx, r.RepoAccess.Pool, args.limit, sql, []any{auth.Allowed, args.after, args.limit + 1}, scanRow)
		if err != nil {
			return zero, err
		}
		return buildConn(page, auth.Withheld, retryAtString(auth.RetryAt)), nil
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

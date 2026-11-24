package graph

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/repoorbit/repoorbit/backend/graphql/model"
	"github.com/repoorbit/repoorbit/backend/internal/db"
)

func actor(id *int64, login, kind *string) *model.EvidenceActor {
	if id == nil || login == nil || kind == nil {
		return nil
	}
	return &model.EvidenceActor{ID: strconv.FormatInt(*id, 10), Login: *login, Type: *kind}
}
func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func (r *queryResolver) issues(ctx context.Context, repositoryIds []string, first int, after *string, number *int) (*model.IssueConnection, error) {
	ctx, cancel := withReadDeadline(ctx)
	defer cancel()
	args, err := parseListArgs(repositoryIds, first, after)
	if err != nil {
		return nil, err
	}
	var issueNumber pgtype.Int8
	if number != nil {
		issueNumber = pgtype.Int8{Int64: int64(*number), Valid: true}
	}
	build := func(rows []db.ListIssuesRow, withheld int, retryAt *string) *model.IssueConnection {
		out := &model.IssueConnection{Nodes: []*model.Issue{}, Withheld: withheld, RetryAt: retryAt}
		out.HasNextPage = len(rows) > args.limit
		rows = rows[:min(len(rows), args.limit)]
		for _, row := range rows {
			id := strconv.FormatInt(row.ID, 10)
			var authorLogin *string
			if row.AuthorLogin.Valid {
				v := row.AuthorLogin.String
				authorLogin = &v
			}
			labels := row.Labels
			if labels == nil {
				labels = []string{}
			}
			assignees := row.Assignees
			if assignees == nil {
				assignees = []string{}
			}
			out.Nodes = append(out.Nodes, &model.Issue{
				ID:           id,
				RepositoryID: strconv.FormatInt(row.RepositoryID, 10),
				Number:       int(row.Number),
				Title:        row.Title,
				State:        row.State,
				URL:          row.HtmlUrl,
				AuthorLogin:  authorLogin,
				UpdatedAt:    stamp(row.UpdatedAt.Time),
				Labels:       labels,
				Assignees:    assignees,
			})
			out.EndCursor = &id
		}
		return out
	}
	if r.Demo {
		rows, err := r.Queries.ListIssues(ctx, db.ListIssuesParams{Demo: true, RepositoryIds: args.ids, After: args.after, Number: issueNumber, PageSize: int32(args.limit + 1)})
		if err != nil {
			return nil, fmt.Errorf("database unavailable")
		}
		return build(rows, 0, nil), nil
	}
	auth, err := r.authorizeSelected(ctx, args.ids)
	if err != nil {
		return nil, err
	}
	withhold := func() (*model.IssueConnection, error) {
		return &model.IssueConnection{Nodes: []*model.Issue{}, Withheld: auth.withheld + len(auth.ids), RetryAt: auth.retryAtString()}, nil
	}
	return guardedRead(ctx, r.RepoAccess.Pool, auth, false, withhold, func() (*model.IssueConnection, error) {
		rows, err := r.Queries.ListIssues(ctx, db.ListIssuesParams{Demo: false, RepositoryIds: auth.ids, After: args.after, Number: issueNumber, PageSize: int32(args.limit + 1)})
		if err != nil {
			return nil, fmt.Errorf("database unavailable")
		}
		return build(rows, auth.withheld, auth.retryAtString()), nil
	}, withhold)
}

func (r *queryResolver) pullRequests(ctx context.Context, ids []string, first int, after *string, newestFirst bool) (*model.PullRequestEvidenceConnection, error) {
	ctx, cancel := withReadDeadline(ctx)
	defer cancel()
	args, err := parseListArgs(ids, first, after)
	if err != nil {
		return nil, err
	}
	// review_state (docs/metrics-definitions.md 5.7): qualifying reviews are the same as for the review backlog; each
	// reviewer's latest APPROVED or CHANGES_REQUESTED decides; null while an open PR's review history has not converged.
	paging := `p.source_id > $2 ORDER BY p.source_id`
	if newestFirst {
		paging = `($2::bigint = 0 OR p.source_id < $2) ORDER BY p.source_id DESC`
	}
	sql := `SELECT p.source_id,p.repository_id,p.number,p.title,p.state,p.html_url,p.created_at,p.updated_at,p.closed_at,p.merged_at,p.author_id,p.author_login,p.author_type,
	  CASE WHEN p.merged_at IS NOT NULL THEN 'merged' WHEN p.state <> 'open' THEN 'closed'
	    WHEN NOT EXISTS (SELECT 1 FROM activity_children c WHERE c.repository_id=p.repository_id AND c.resource='reviews' AND c.parent_id=p.source_id AND c.watermark IS NOT NULL) THEN NULL
	    WHEN 'CHANGES_REQUESTED' = ANY(d.latest) THEN 'changes_requested' WHEN 'APPROVED' = ANY(d.latest) THEN 'approved'
	    WHEN d.reviewed THEN 'commented' ELSE 'awaiting_review' END
	  FROM activity_pull_requests p
	  CROSS JOIN LATERAL (
	    SELECT EXISTS (SELECT 1 FROM activity_reviews r WHERE r.repository_id=p.repository_id AND r.pull_request_id=p.source_id AND r.submitted_at IS NOT NULL
	        AND upper(r.state)<>'PENDING' AND r.author_id IS NOT NULL AND r.author_type<>'Bot' AND r.author_id IS DISTINCT FROM p.author_id) AS reviewed,
	      ARRAY(SELECT DISTINCT ON (r.author_id) upper(r.state) FROM activity_reviews r
	        WHERE r.repository_id=p.repository_id AND r.pull_request_id=p.source_id AND r.submitted_at IS NOT NULL AND upper(r.state) IN ('APPROVED','CHANGES_REQUESTED')
	          AND r.author_id IS NOT NULL AND r.author_type<>'Bot' AND r.author_id IS DISTINCT FROM p.author_id
	        ORDER BY r.author_id, r.submitted_at DESC, r.source_id DESC) AS latest
	  ) d
	  WHERE p.repository_id=ANY($1) AND ` + paging + ` LIMIT $3`
	scan := func(rows pgx.Rows) (int64, *model.PullRequestEvidence, error) {
		var id, repo, number int64
		var aid *int64
		var login, kind *string
		var title, state, url string
		var created, updated time.Time
		var closed, merged *time.Time
		var reviewState *string
		if err := rows.Scan(&id, &repo, &number, &title, &state, &url, &created, &updated, &closed, &merged, &aid, &login, &kind, &reviewState); err != nil {
			return 0, nil, err
		}
		return id, &model.PullRequestEvidence{
			ID:           strconv.FormatInt(id, 10),
			RepositoryID: strconv.FormatInt(repo, 10),
			Number:       int(number),
			Title:        title,
			State:        state,
			URL:          url,
			CreatedAt:    stamp(created),
			UpdatedAt:    stamp(updated),
			ClosedAt:     rfc3339(closed),
			MergedAt:     rfc3339(merged),
			Author:       actor(aid, login, kind),
			ReviewState:  reviewState,
		}, nil
	}
	if r.Demo {
		var demoIDs []int64
		for _, id := range ids {
			repo, err := r.Repository(ctx, id)
			if err != nil {
				return nil, err
			}
			if repo != nil {
				repoID, _ := strconv.ParseInt(repo.ID, 10, 64)
				demoIDs = append(demoIDs, repoID)
			}
		}
		if len(demoIDs) == 0 {
			return &model.PullRequestEvidenceConnection{Nodes: []*model.PullRequestEvidence{}}, nil
		}
		page, err := queryEvidencePage(ctx, r.DemoPool, args.limit, sql, []any{demoIDs, args.after, args.limit + 1}, scan)
		if err != nil {
			return nil, err
		}
		return &model.PullRequestEvidenceConnection{Nodes: page.nodes, EndCursor: page.endCursor, HasNextPage: page.hasNextPage}, nil
	}
	auth, err := r.authorizeSelected(ctx, args.ids)
	if err != nil {
		return nil, err
	}
	withhold := func() (*model.PullRequestEvidenceConnection, error) {
		return &model.PullRequestEvidenceConnection{Nodes: []*model.PullRequestEvidence{}, Withheld: auth.withheld + len(auth.ids), RetryAt: auth.retryAtString()}, nil
	}
	return guardedRead(ctx, r.RepoAccess.Pool, auth, false, withhold, func() (*model.PullRequestEvidenceConnection, error) {
		page, err := queryEvidencePage(ctx, r.RepoAccess.Pool, args.limit, sql, []any{auth.ids, args.after, args.limit + 1}, scan)
		if err != nil {
			return nil, err
		}
		return &model.PullRequestEvidenceConnection{
			Nodes: page.nodes, EndCursor: page.endCursor, HasNextPage: page.hasNextPage,
			Withheld: auth.withheld, RetryAt: auth.retryAtString(),
		}, nil
	}, withhold)
}

func (r *queryResolver) reviews(ctx context.Context, ids []string, first int, after *string) (*model.ReviewEvidenceConnection, error) {
	return readEvidenceList(r, ctx, ids, first, after,
		`SELECT source_id,repository_id,pull_request_id,state,body,html_url,submitted_at,author_id,author_login,author_type FROM activity_reviews WHERE repository_id=ANY($1) AND source_id>$2 AND submitted_at IS NOT NULL AND upper(state)<>'PENDING' ORDER BY source_id LIMIT $3`,
		func(rows pgx.Rows) (int64, *model.ReviewEvidence, error) {
			var id, repo, pr int64
			var aid *int64
			var login, kind *string
			var state, body, url string
			var submitted time.Time
			if err := rows.Scan(&id, &repo, &pr, &state, &body, &url, &submitted, &aid, &login, &kind); err != nil {
				return 0, nil, err
			}
			return id, &model.ReviewEvidence{
				ID:            strconv.FormatInt(id, 10),
				RepositoryID:  strconv.FormatInt(repo, 10),
				PullRequestID: strconv.FormatInt(pr, 10),
				State:         state,
				Body:          body,
				URL:           url,
				SubmittedAt:   stamp(submitted),
				Author:        actor(aid, login, kind),
			}, nil
		},
		func(page pageData[model.ReviewEvidence], withheld int, retryAt *string) *model.ReviewEvidenceConnection {
			return &model.ReviewEvidenceConnection{
				Nodes: page.nodes, EndCursor: page.endCursor, HasNextPage: page.hasNextPage,
				Withheld: withheld, RetryAt: retryAt,
			}
		},
	)
}

func (r *queryResolver) comments(ctx context.Context, ids []string, first int, after *string) (*model.CommentEvidenceConnection, error) {
	return readEvidenceList(r, ctx, ids, first, after,
		`SELECT source_id,repository_id,issue_number,body,html_url,created_at,updated_at,author_id,author_login,author_type FROM activity_comments WHERE repository_id=ANY($1) AND source_id>$2 ORDER BY source_id LIMIT $3`,
		func(rows pgx.Rows) (int64, *model.CommentEvidence, error) {
			var id, repo, num int64
			var aid *int64
			var login, kind *string
			var body, url string
			var created, updated time.Time
			if err := rows.Scan(&id, &repo, &num, &body, &url, &created, &updated, &aid, &login, &kind); err != nil {
				return 0, nil, err
			}
			return id, &model.CommentEvidence{
				ID:           strconv.FormatInt(id, 10),
				RepositoryID: strconv.FormatInt(repo, 10),
				IssueNumber:  int(num),
				Body:         body,
				URL:          url,
				CreatedAt:    stamp(created),
				UpdatedAt:    stamp(updated),
				Author:       actor(aid, login, kind),
			}, nil
		},
		func(page pageData[model.CommentEvidence], withheld int, retryAt *string) *model.CommentEvidenceConnection {
			return &model.CommentEvidenceConnection{
				Nodes: page.nodes, EndCursor: page.endCursor, HasNextPage: page.hasNextPage,
				Withheld: withheld, RetryAt: retryAt,
			}
		},
	)
}

func (r *queryResolver) releases(ctx context.Context, ids []string, first int, after *string) (*model.ReleaseEvidenceConnection, error) {
	return readEvidenceList(r, ctx, ids, first, after,
		`SELECT source_id,repository_id,name,tag_name,body,html_url,created_at,updated_at,published_at,observed_at,prerelease,author_id,author_login,author_type FROM activity_releases WHERE repository_id=ANY($1) AND source_id>$2 ORDER BY source_id LIMIT $3`,
		func(rows pgx.Rows) (int64, *model.ReleaseEvidence, error) {
			var id, repo int64
			var name, tag, body, url string
			var created, published, observed time.Time
			var updated *time.Time
			var prerelease bool
			var aid *int64
			var login, kind *string
			if err := rows.Scan(&id, &repo, &name, &tag, &body, &url, &created, &updated, &published, &observed, &prerelease, &aid, &login, &kind); err != nil {
				return 0, nil, err
			}
			return id, &model.ReleaseEvidence{
				ID:           strconv.FormatInt(id, 10),
				RepositoryID: strconv.FormatInt(repo, 10),
				Name:         name,
				TagName:      tag,
				Body:         body,
				URL:          url,
				CreatedAt:    stamp(created),
				UpdatedAt:    rfc3339(updated),
				PublishedAt:  stamp(published),
				ObservedAt:   stamp(observed),
				Prerelease:   prerelease,
				Author:       actor(aid, login, kind),
			}, nil
		},
		func(page pageData[model.ReleaseEvidence], withheld int, retryAt *string) *model.ReleaseEvidenceConnection {
			return &model.ReleaseEvidenceConnection{
				Nodes: page.nodes, EndCursor: page.endCursor, HasNextPage: page.hasNextPage,
				Withheld: withheld, RetryAt: retryAt,
			}
		},
	)
}

func (r *queryResolver) issueEvents(ctx context.Context, ids []string, first int, after *string) (*model.IssueEventEvidenceConnection, error) {
	return readEvidenceList(r, ctx, ids, first, after,
		`SELECT source_id,repository_id,issue_id,issue_number,event,url,created_at,observed_at,actor_id,actor_login,actor_type,assignee_id,assignee_login,assignee_type,label FROM activity_issue_events WHERE repository_id=ANY($1) AND source_id>$2 ORDER BY source_id LIMIT $3`,
		func(rows pgx.Rows) (int64, *model.IssueEventEvidence, error) {
			var id, repo, issue, num int64
			var event, url string
			var created, observed time.Time
			var aid, sid *int64
			var login, kind, slogin, skind, label *string
			if err := rows.Scan(&id, &repo, &issue, &num, &event, &url, &created, &observed, &aid, &login, &kind, &sid, &slogin, &skind, &label); err != nil {
				return 0, nil, err
			}
			return id, &model.IssueEventEvidence{
				ID:           strconv.FormatInt(id, 10),
				RepositoryID: strconv.FormatInt(repo, 10),
				IssueID:      strconv.FormatInt(issue, 10),
				IssueNumber:  int(num),
				Event:        event,
				URL:          url,
				CreatedAt:    stamp(created),
				ObservedAt:   stamp(observed),
				Actor:        actor(aid, login, kind),
				Assignee:     actor(sid, slogin, skind),
				Label:        label,
			}, nil
		},
		func(page pageData[model.IssueEventEvidence], withheld int, retryAt *string) *model.IssueEventEvidenceConnection {
			return &model.IssueEventEvidenceConnection{
				Nodes: page.nodes, EndCursor: page.endCursor, HasNextPage: page.hasNextPage,
				Withheld: withheld, RetryAt: retryAt,
			}
		},
	)
}

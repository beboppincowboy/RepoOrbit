package graph

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/repoorbit/repoorbit/backend/graphql/model"
	"github.com/repoorbit/repoorbit/backend/internal/accessfence"
	"github.com/repoorbit/repoorbit/backend/internal/db"
	"github.com/repoorbit/repoorbit/backend/internal/platform"
)

type evidencePage struct {
	ids      []int64
	after    int64
	n        int
	withheld int
	retry    *string
	fence    accessfence.Snapshot
}

func (r *queryResolver) evidenceAccess(ctx context.Context, raw []string, first int, after *string) (evidencePage, error) {
	var p evidencePage
	var err error
	if p.n, err = pageLimit(first); err != nil {
		return p, err
	}
	if p.after, err = parsePositiveID(after); err != nil {
		return p, err
	}
	if len(raw) > maxRepositoryIDs {
		return p, fmt.Errorf("at most %d repositories", maxRepositoryIDs)
	}
	for _, s := range raw {
		id, e := parsePositiveID(&s)
		if e != nil {
			return p, e
		}
		p.ids = append(p.ids, id)
	}
	if r.Demo {
		p.ids = nil
		return p, nil
	}
	user, err := r.userID(ctx)
	if err != nil {
		return p, err
	}
	p.fence, err = accessfence.TakeSnapshotForUser(ctx, r.RepoAccess.Pool, user)
	if err != nil {
		return p, fmt.Errorf("database unavailable")
	}
	p.ids, err = r.Queries.ListSelectedIDs(ctx, db.ListSelectedIDsParams{UserID: user, RepositoryIds: p.ids})
	if err != nil {
		return p, fmt.Errorf("database unavailable")
	}
	checks := r.RepoAccess.Check(ctx, user, p.ids, true)
	allowed := make([]int64, 0, len(p.ids))
	for _, id := range p.ids {
		d := checks[id]
		if d.Allowed {
			allowed = append(allowed, id)
		}
		if d.Unverified {
			p.withheld++
			p.retry = laterRetry(p.retry, d.RetryAt)
		}
	}
	p.ids = allowed
	return p, nil
}
func (r *queryResolver) evidenceCurrent(ctx context.Context, p evidencePage) bool {
	current, err := p.fence.Current(ctx, r.RepoAccess.Pool)
	return err == nil && current
}
func actor(id *int64, login, kind *string) *model.EvidenceActor {
	if id == nil || login == nil || kind == nil {
		return nil
	}
	return &model.EvidenceActor{ID: strconv.FormatInt(*id, 10), Login: *login, Type: *kind}
}
func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func (r *queryResolver) pullRequests(ctx context.Context, ids []string, first int, after *string) (*model.PullRequestEvidenceConnection, error) {
	ctx, cancel := platform.WithRequestDeadline(ctx)
	defer cancel()
	p, err := r.evidenceAccess(ctx, ids, first, after)
	if err != nil {
		return nil, err
	}
	out := &model.PullRequestEvidenceConnection{Nodes: []*model.PullRequestEvidence{}, Withheld: p.withheld, RetryAt: p.retry}
	if len(p.ids) == 0 {
		return out, nil
	}
	rows, err := r.RepoAccess.Pool.Query(ctx, `SELECT source_id,repository_id,number,title,state,html_url,created_at,updated_at,closed_at,merged_at,author_id,author_login,author_type FROM activity_pull_requests WHERE repository_id=ANY($1) AND source_id>$2 ORDER BY source_id LIMIT $3`, p.ids, p.after, p.n+1)
	if err != nil {
		return nil, fmt.Errorf("database unavailable")
	}
	defer rows.Close()
	for rows.Next() {
		var id, repo, number int64
		var aid *int64
		var login, kind *string
		var title, state, url string
		var created, updated time.Time
		var closed, merged *time.Time
		if err = rows.Scan(&id, &repo, &number, &title, &state, &url, &created, &updated, &closed, &merged, &aid, &login, &kind); err != nil {
			return nil, fmt.Errorf("database unavailable")
		}
		if len(out.Nodes) == p.n {
			out.HasNextPage = true
			break
		}
		cursor := strconv.FormatInt(id, 10)
		out.EndCursor = &cursor
		out.Nodes = append(out.Nodes, &model.PullRequestEvidence{ID: cursor, RepositoryID: strconv.FormatInt(repo, 10), Number: int(number), Title: title, State: state, URL: url, CreatedAt: stamp(created), UpdatedAt: stamp(updated), ClosedAt: rfc3339(closed), MergedAt: rfc3339(merged), Author: actor(aid, login, kind)})
	}
	if rows.Err() != nil {
		return nil, fmt.Errorf("database unavailable")
	}
	rows.Close()
	if !r.evidenceCurrent(ctx, p) {
		out.Nodes = nil
		out.Nodes = []*model.PullRequestEvidence{}
		out.EndCursor = nil
		out.HasNextPage = false
		out.Withheld += len(p.ids)
	}
	return out, nil
}
func (r *queryResolver) reviews(ctx context.Context, ids []string, first int, after *string) (*model.ReviewEvidenceConnection, error) {
	ctx, cancel := platform.WithRequestDeadline(ctx)
	defer cancel()
	p, err := r.evidenceAccess(ctx, ids, first, after)
	if err != nil {
		return nil, err
	}
	out := &model.ReviewEvidenceConnection{Nodes: []*model.ReviewEvidence{}, Withheld: p.withheld, RetryAt: p.retry}
	if len(p.ids) == 0 {
		return out, nil
	}
	rows, err := r.RepoAccess.Pool.Query(ctx, `SELECT source_id,repository_id,pull_request_id,state,body,html_url,submitted_at,author_id,author_login,author_type FROM activity_reviews WHERE repository_id=ANY($1) AND source_id>$2 AND submitted_at IS NOT NULL AND upper(state)<>'PENDING' ORDER BY source_id LIMIT $3`, p.ids, p.after, p.n+1)
	if err != nil {
		return nil, fmt.Errorf("database unavailable")
	}
	defer rows.Close()
	for rows.Next() {
		var id, repo, pr int64
		var aid *int64
		var login, kind *string
		var state, body, url string
		var submitted time.Time
		if err = rows.Scan(&id, &repo, &pr, &state, &body, &url, &submitted, &aid, &login, &kind); err != nil {
			return nil, fmt.Errorf("database unavailable")
		}
		if len(out.Nodes) == p.n {
			out.HasNextPage = true
			break
		}
		cursor := strconv.FormatInt(id, 10)
		out.EndCursor = &cursor
		out.Nodes = append(out.Nodes, &model.ReviewEvidence{ID: cursor, RepositoryID: strconv.FormatInt(repo, 10), PullRequestID: strconv.FormatInt(pr, 10), State: state, Body: body, URL: url, SubmittedAt: stamp(submitted), Author: actor(aid, login, kind)})
	}
	if rows.Err() != nil {
		return nil, fmt.Errorf("database unavailable")
	}
	rows.Close()
	if !r.evidenceCurrent(ctx, p) {
		out.Nodes = []*model.ReviewEvidence{}
		out.EndCursor = nil
		out.HasNextPage = false
		out.Withheld += len(p.ids)
	}
	return out, nil
}
func (r *queryResolver) comments(ctx context.Context, ids []string, first int, after *string) (*model.CommentEvidenceConnection, error) {
	ctx, cancel := platform.WithRequestDeadline(ctx)
	defer cancel()
	p, err := r.evidenceAccess(ctx, ids, first, after)
	if err != nil {
		return nil, err
	}
	out := &model.CommentEvidenceConnection{Nodes: []*model.CommentEvidence{}, Withheld: p.withheld, RetryAt: p.retry}
	if len(p.ids) == 0 {
		return out, nil
	}
	rows, err := r.RepoAccess.Pool.Query(ctx, `SELECT source_id,repository_id,issue_number,body,html_url,created_at,updated_at,author_id,author_login,author_type FROM activity_comments WHERE repository_id=ANY($1) AND source_id>$2 ORDER BY source_id LIMIT $3`, p.ids, p.after, p.n+1)
	if err != nil {
		return nil, fmt.Errorf("database unavailable")
	}
	defer rows.Close()
	for rows.Next() {
		var id, repo, num int64
		var aid *int64
		var login, kind *string
		var body, url string
		var created, updated time.Time
		if err = rows.Scan(&id, &repo, &num, &body, &url, &created, &updated, &aid, &login, &kind); err != nil {
			return nil, fmt.Errorf("database unavailable")
		}
		if len(out.Nodes) == p.n {
			out.HasNextPage = true
			break
		}
		cursor := strconv.FormatInt(id, 10)
		out.EndCursor = &cursor
		out.Nodes = append(out.Nodes, &model.CommentEvidence{ID: cursor, RepositoryID: strconv.FormatInt(repo, 10), IssueNumber: int(num), Body: body, URL: url, CreatedAt: stamp(created), UpdatedAt: stamp(updated), Author: actor(aid, login, kind)})
	}
	if rows.Err() != nil {
		return nil, fmt.Errorf("database unavailable")
	}
	rows.Close()
	if !r.evidenceCurrent(ctx, p) {
		out.Nodes = []*model.CommentEvidence{}
		out.EndCursor = nil
		out.HasNextPage = false
		out.Withheld += len(p.ids)
	}
	return out, nil
}

func (r *queryResolver) releases(ctx context.Context, ids []string, first int, after *string) (*model.ReleaseEvidenceConnection, error) {
	ctx, cancel := platform.WithRequestDeadline(ctx)
	defer cancel()
	p, err := r.evidenceAccess(ctx, ids, first, after)
	if err != nil {
		return nil, err
	}
	out := &model.ReleaseEvidenceConnection{Nodes: []*model.ReleaseEvidence{}, Withheld: p.withheld, RetryAt: p.retry}
	if len(p.ids) == 0 {
		return out, nil
	}
	rows, err := r.RepoAccess.Pool.Query(ctx, `SELECT source_id,repository_id,name,tag_name,body,html_url,created_at,updated_at,published_at,observed_at,prerelease,author_id,author_login,author_type FROM activity_releases WHERE repository_id=ANY($1) AND source_id>$2 ORDER BY source_id LIMIT $3`, p.ids, p.after, p.n+1)
	if err != nil {
		return nil, fmt.Errorf("database unavailable")
	}
	defer rows.Close()
	for rows.Next() {
		var id, repo int64
		var name, tag, body, url string
		var created, published, observed time.Time
		var updated *time.Time
		var prerelease bool
		var aid *int64
		var login, kind *string
		if rows.Scan(&id, &repo, &name, &tag, &body, &url, &created, &updated, &published, &observed, &prerelease, &aid, &login, &kind) != nil {
			return nil, fmt.Errorf("database unavailable")
		}
		if len(out.Nodes) == p.n {
			out.HasNextPage = true
			break
		}
		cursor := strconv.FormatInt(id, 10)
		out.EndCursor = &cursor
		out.Nodes = append(out.Nodes, &model.ReleaseEvidence{ID: cursor, RepositoryID: strconv.FormatInt(repo, 10), Name: name, TagName: tag, Body: body, URL: url, CreatedAt: stamp(created), UpdatedAt: rfc3339(updated), PublishedAt: stamp(published), ObservedAt: stamp(observed), Prerelease: prerelease, Author: actor(aid, login, kind)})
	}
	if rows.Err() != nil {
		return nil, fmt.Errorf("database unavailable")
	}
	rows.Close()
	if !r.evidenceCurrent(ctx, p) {
		out.Nodes = []*model.ReleaseEvidence{}
		out.EndCursor = nil
		out.HasNextPage = false
		out.Withheld += len(p.ids)
	}
	return out, nil
}

func (r *queryResolver) issueEvents(ctx context.Context, ids []string, first int, after *string) (*model.IssueEventEvidenceConnection, error) {
	ctx, cancel := platform.WithRequestDeadline(ctx)
	defer cancel()
	p, err := r.evidenceAccess(ctx, ids, first, after)
	if err != nil {
		return nil, err
	}
	out := &model.IssueEventEvidenceConnection{Nodes: []*model.IssueEventEvidence{}, Withheld: p.withheld, RetryAt: p.retry}
	if len(p.ids) == 0 {
		return out, nil
	}
	rows, err := r.RepoAccess.Pool.Query(ctx, `SELECT source_id,repository_id,issue_id,issue_number,event,url,created_at,observed_at,actor_id,actor_login,actor_type,assignee_id,assignee_login,assignee_type,label FROM activity_issue_events WHERE repository_id=ANY($1) AND source_id>$2 ORDER BY source_id LIMIT $3`, p.ids, p.after, p.n+1)
	if err != nil {
		return nil, fmt.Errorf("database unavailable")
	}
	defer rows.Close()
	for rows.Next() {
		var id, repo, issue, num int64
		var event, url string
		var created, observed time.Time
		var aid, sid *int64
		var login, kind, slogin, skind, label *string
		if rows.Scan(&id, &repo, &issue, &num, &event, &url, &created, &observed, &aid, &login, &kind, &sid, &slogin, &skind, &label) != nil {
			return nil, fmt.Errorf("database unavailable")
		}
		if len(out.Nodes) == p.n {
			out.HasNextPage = true
			break
		}
		cursor := strconv.FormatInt(id, 10)
		out.EndCursor = &cursor
		out.Nodes = append(out.Nodes, &model.IssueEventEvidence{ID: cursor, RepositoryID: strconv.FormatInt(repo, 10), IssueID: strconv.FormatInt(issue, 10), IssueNumber: int(num), Event: event, URL: url, CreatedAt: stamp(created), ObservedAt: stamp(observed), Actor: actor(aid, login, kind), Assignee: actor(sid, slogin, skind), Label: label})
	}
	if rows.Err() != nil {
		return nil, fmt.Errorf("database unavailable")
	}
	rows.Close()
	if !r.evidenceCurrent(ctx, p) {
		out.Nodes = []*model.IssueEventEvidence{}
		out.EndCursor = nil
		out.HasNextPage = false
		out.Withheld += len(p.ids)
	}
	return out, nil
}

//go:build integration

package graph

import (
	"context"
	"testing"
	"time"

	"github.com/repoorbit/repoorbit/backend/graphql/model"
	"github.com/repoorbit/repoorbit/backend/internal/db"
	"github.com/repoorbit/repoorbit/backend/internal/jobs"
	"github.com/repoorbit/repoorbit/backend/internal/repositories"
	"github.com/repoorbit/repoorbit/backend/internal/testutil"
)

// Statuses attach only to repositories the viewer selected, and carry a safe code, not raw error text.
func TestAttachMetadataStatus(t *testing.T) {
	ctx := context.Background()
	_, pool := testutil.Database(t)
	for _, sql := range []string{
		`INSERT INTO users (id, github_id, login, access_token_enc) VALUES (1, 1, 'u', '\x00')`,
		`INSERT INTO repositories (id, name, description, is_private, is_demo) VALUES (10, 'o/a', '', false, false), (11, 'o/b', '', false, false)`,
		`INSERT INTO selections (user_id, repository_id, source, selected_at) VALUES (1, 10, 'owned', now())`,
		`INSERT INTO repository_sync (repository_id, last_success_at, last_error_code) VALUES (10, now(), 'token ghu_secret leaked')`,
	} {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	r := &Resolver{Queries: db.New(pool)}
	nodes := []*model.Repository{{ID: "10"}, {ID: "11"}}
	if err := r.attachMetadataStatus(ctx, 1, nodes); err != nil {
		t.Fatal(err)
	}
	if nodes[1].MetadataStatus != nil {
		t.Fatal("unselected repository got a status")
	}
	s := nodes[0].MetadataStatus
	if s == nil || s.LastSuccessAt == nil || s.FailureCode == nil || *s.FailureCode != "unknown" || s.State != "not-scheduled" {
		t.Fatalf("%+v", s)
	}
	tx, _ := pool.Begin(ctx)
	defer tx.Rollback(ctx)
	if err := jobs.Enqueue(ctx, tx, jobs.KindRepoMetadataRefresh, "repo:10", time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE jobs SET state='paused'`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.attachMetadataStatus(ctx, 1, nodes[:1]); err != nil || !nodes[0].MetadataStatus.Paused {
		t.Fatalf("%v %+v", err, nodes[0].MetadataStatus)
	}
}

func TestEvidenceCoverageRequiresParentsAndChildren(t *testing.T) {
	ctx := context.Background()
	_, pool := testutil.Database(t)
	_, err := pool.Exec(ctx, `INSERT INTO repositories (id,name,description) VALUES (10,'o/a','');
 INSERT INTO activity_resources(repository_id,resource,boundary,phase,watermark) VALUES (10,'issues',now(),'current',now()),(10,'pull_requests',now(),'catchup',NULL);
 INSERT INTO activity_children(repository_id,resource,parent_id,parent_number,complete,last_error_code,last_success_at) VALUES (10,'reviews',100,1,false,'rate_limited',now());`)
	if err != nil {
		t.Fatal(err)
	}
	r := &Resolver{Queries: db.New(pool), RepoAccess: &repositories.Service{Pool: pool}}
	a := &model.ActivityStatus{Issues: &model.ActivityResourceStatus{Phase: "current", CatchupThrough: ptr("now")}}
	if err = r.attachEvidenceStatus(ctx, 10, a); err != nil {
		t.Fatal(err)
	}
	if a.Reviews.Complete || a.Comments.Complete || a.Reviews.PendingChildren != 1 || a.Reviews.FailureCode == nil || *a.Reviews.FailureCode != "rate_limited" || a.Reviews.LastSuccessAt == nil {
		t.Fatalf("incomplete %+v", a)
	}
	_, err = pool.Exec(ctx, `UPDATE activity_resources SET phase='current',watermark=now() WHERE resource='pull_requests'; UPDATE activity_children SET complete=true WHERE repository_id=10`)
	if err != nil {
		t.Fatal(err)
	}
	if err = r.attachEvidenceStatus(ctx, 10, a); err != nil {
		t.Fatal(err)
	}
	if !a.Reviews.Complete || !a.Comments.Complete || a.Reviews.FailureCode == nil {
		t.Fatalf("completed evidence should retain later error: %+v", a)
	}
	a.Issues.Phase = "catchup"
	if err = r.attachEvidenceStatus(ctx, 10, a); err != nil {
		t.Fatal(err)
	}
	if !a.Reviews.Complete || a.Comments.Complete {
		t.Fatalf("independent coverage: %+v", a)
	}
}
func ptr(s string) *string { return &s }

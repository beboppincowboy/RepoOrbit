//go:build integration

package graph

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/repoorbit/repoorbit/backend/graphql/model"
	"github.com/repoorbit/repoorbit/backend/internal/db"
	"github.com/repoorbit/repoorbit/backend/internal/jobs"
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
	if err := jobs.Enqueue(ctx, tx, jobs.KindRepoMetadataRefresh, "repo:10", time.Now()); errors.Is(err, jobs.ErrNotImplemented) {
		t.Skip("needs U3a queue")
	} else if err != nil {
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

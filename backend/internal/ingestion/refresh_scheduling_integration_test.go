//go:build integration

package ingestion_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/repoorbit/repoorbit/backend/internal/db"
	"github.com/repoorbit/repoorbit/backend/internal/ingestion"
)

func TestRequestRefreshOnlyForEligibleSelections(t *testing.T) {
	e := setup(t)
	uid := e.signIn("alice")
	id := e.gh.AddRepo("bob", "tool", false)
	e.store(uid, id, false)
	for _, stmt := range []string{"UPDATE selections SET deselected_at = now()", "UPDATE selections SET deselected_at = NULL, follow_suspended_at = now()"} {
		if _, err := e.pool.Exec(context.Background(), stmt); err != nil {
			t.Fatal(err)
		}
		var scheduled bool
		if err := db.InTx(context.Background(), e.pool, func(tx pgx.Tx) (err error) {
			scheduled, err = ingestion.RequestMetadataRefresh(context.Background(), tx, id)
			return
		}); err != nil || scheduled {
			t.Fatalf("%s: scheduled=%v err=%v", stmt, scheduled, err)
		}
	}
	if _, err := e.pool.Exec(context.Background(), "UPDATE selections SET follow_suspended_at = NULL"); err != nil {
		t.Fatal(err)
	}
	var scheduled bool
	err := db.InTx(context.Background(), e.pool, func(tx pgx.Tx) (err error) {
		scheduled, err = ingestion.RequestMetadataRefresh(context.Background(), tx, id)
		return
	})
	if err != nil || !scheduled {
		t.Fatalf("scheduled=%v err=%v", scheduled, err)
	}
	var n int
	_ = e.pool.QueryRow(context.Background(), "SELECT count(*) FROM jobs WHERE kind = 'repo_metadata_refresh' AND key = $1", fmt.Sprintf("repo:%d", id)).Scan(&n)
	if n != 1 {
		t.Fatalf("jobs = %d", n)
	}
}

func TestSelectionAndScheduleRollBackTogether(t *testing.T) {
	e := setup(t)
	uid := e.signIn("alice")
	id := e.gh.AddRepo("alice", "mine", false)
	boom := errors.New("enqueue failed")
	e.repo.OnSelect = func(ctx context.Context, tx pgx.Tx, repoID int64) error { return boom }
	e.repo.GitHub = e.ghc
	if err := e.repo.Connect(context.Background(), uid, id); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	var n int
	_ = e.pool.QueryRow(context.Background(), "SELECT (SELECT count(*) FROM selections) + (SELECT count(*) FROM repositories WHERE id = $1)", id).Scan(&n)
	if n != 0 {
		t.Fatal("selection or repository row survived a failed scheduling step")
	}
	// And with the real hook: success commits both (when the queue exists).
	e.repo.OnSelect = func(ctx context.Context, tx pgx.Tx, repoID int64) error {
		_, err := ingestion.RequestMetadataRefresh(ctx, tx, repoID)
		return err
	}
	err := e.repo.Connect(context.Background(), uid, id)
	if err != nil {
		t.Fatal(err)
	}
	_ = e.pool.QueryRow(context.Background(), "SELECT count(*) FROM jobs WHERE key = $1", fmt.Sprintf("repo:%d", id)).Scan(&n)
	if n != 1 {
		t.Fatalf("jobs = %d", n)
	}
}

func TestStartupReconciliationSchedulesPreU3Selections(t *testing.T) {
	e := setup(t)
	uid := e.signIn("alice")
	id1 := e.gh.AddRepo("bob", "one", false)
	id2 := e.gh.AddRepo("bob", "two", false)
	e.store(uid, id1, false)
	e.store(uid, id2, false)
	if _, err := e.pool.Exec(context.Background(), "UPDATE selections SET deselected_at = now() WHERE repository_id = $1", id2); err != nil {
		t.Fatal(err)
	}
	n, err := ingestion.ReconcileSelections(context.Background(), e.pool)
	if err != nil || n != 1 {
		t.Fatalf("n=%d err=%v", n, err)
	}
}

func TestOverlappingRefreshProducesFollowUp(t *testing.T) {
	e := setup(t)
	uid := e.signIn("alice")
	id := e.gh.AddRepo("bob", "tool", false)
	e.store(uid, id, false)
	err := db.InTx(context.Background(), e.pool, func(tx pgx.Tx) error {
		_, err := ingestion.RequestMetadataRefresh(context.Background(), tx, id)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	key := fmt.Sprintf("repo:%d", id)
	if _, err := e.pool.Exec(context.Background(), "UPDATE jobs SET state = 'running' WHERE key = $1", key); err != nil {
		t.Fatal(err)
	}
	if err := db.InTx(context.Background(), e.pool, func(tx pgx.Tx) error {
		_, err := ingestion.RequestMetadataRefresh(context.Background(), tx, id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var pending bool
	_ = e.pool.QueryRow(context.Background(), "SELECT pending FROM jobs WHERE key = $1", key).Scan(&pending)
	if !pending {
		t.Fatal("refresh requested during execution was not retained")
	}
}

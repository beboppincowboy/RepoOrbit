//go:build integration

package ingestion

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/repoorbit/repoorbit/backend/internal/db"
)

func TestEligibleSelectionTruthTableAndQueryMatrix(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	f.user(1)
	f.user(2)
	f.user(5)
	f.user(10)

	f.repo(101)
	f.repo(102)
	f.repo(103)
	f.repo(104)
	f.exec(`INSERT INTO repositories (id, name, description, is_private, is_demo) VALUES ($1, 'o/demo', '', false, true)`, int64(105))
	f.repo(106)

	base := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

	// Repo 101: multiple active selectors to verify (selected_at, user_id) ordering and pool deduplication.
	f.exec(`INSERT INTO selections (user_id, repository_id, source, selected_at) VALUES ($1, $2, 'owned', $3)`, int64(10), int64(101), base)
	f.exec(`INSERT INTO selections (user_id, repository_id, source, selected_at) VALUES ($1, $2, 'owned', $3)`, int64(2), int64(101), base.Add(time.Hour))
	f.exec(`INSERT INTO selections (user_id, repository_id, source, selected_at) VALUES ($1, $2, 'owned', $3)`, int64(5), int64(101), base)

	// Repo 102: active non-demo selected by users 1 and 2.
	f.exec(`INSERT INTO selections (user_id, repository_id, source, selected_at) VALUES ($1, $2, 'owned', $3)`, int64(1), int64(102), base)
	f.exec(`INSERT INTO selections (user_id, repository_id, source, selected_at) VALUES ($1, $2, 'owned', $3)`, int64(2), int64(102), base.Add(time.Minute))

	// Repo 103: deselected non-demo.
	f.exec(`INSERT INTO selections (user_id, repository_id, source, selected_at, deselected_at) VALUES ($1, $2, 'owned', $3, $4)`, int64(1), int64(103), base, base.Add(time.Hour))

	// Repo 104: follow-suspended non-demo.
	f.exec(`INSERT INTO selections (user_id, repository_id, source, selected_at, follow_suspended_at) VALUES ($1, $2, 'starred', $3, $4)`, int64(1), int64(104), base, base.Add(time.Hour))

	// Repo 105: active demo repository.
	f.exec(`INSERT INTO selections (user_id, repository_id, source, selected_at) VALUES ($1, $2, 'owned', $3)`, int64(1), int64(105), base)

	// Repo 106: active non-demo for user 1.
	f.exec(`INSERT INTO selections (user_id, repository_id, source, selected_at) VALUES ($1, $2, 'owned', $3)`, int64(1), int64(106), base)

	t.Run("HasEligibleSelection", func(t *testing.T) {
		for _, tc := range []struct {
			repoID int64
			want   bool
		}{
			{101, true},
			{102, true},
			{103, false},
			{104, false},
			{105, false},
			{106, true},
			{999, false},
		} {
			got, err := f.q.HasEligibleSelection(ctx, tc.repoID)
			if err != nil {
				t.Fatalf("HasEligibleSelection(%d): %v", tc.repoID, err)
			}
			if got != tc.want {
				t.Fatalf("HasEligibleSelection(%d) = %v, want %v", tc.repoID, got, tc.want)
			}
		}
	})

	t.Run("ListEligibleSelectors ordering", func(t *testing.T) {
		got101, err := f.q.ListEligibleSelectors(ctx, 101)
		if err != nil {
			t.Fatalf("ListEligibleSelectors(101): %v", err)
		}
		if want := []int64{5, 10, 2}; !slices.Equal(got101, want) {
			t.Fatalf("ListEligibleSelectors(101) = %v, want %v", got101, want)
		}
		for _, repoID := range []int64{103, 104, 105} {
			got, err := f.q.ListEligibleSelectors(ctx, repoID)
			if err != nil {
				t.Fatalf("ListEligibleSelectors(%d): %v", repoID, err)
			}
			if len(got) != 0 {
				t.Fatalf("ListEligibleSelectors(%d) = %v, want empty", repoID, got)
			}
		}
	})

	t.Run("LockEligibleSelection membership", func(t *testing.T) {
		tx, err := f.begin()
		if err != nil {
			t.Fatal(err)
		}
		qtx := f.q.WithTx(tx)
		uid, err := qtx.LockEligibleSelection(ctx, db.LockEligibleSelectionParams{RepositoryID: 102, UserID: 1})
		if err != nil || uid != 1 {
			t.Fatalf("LockEligibleSelection(102, 1) = (%d, %v), want (1, nil)", uid, err)
		}
		for _, repoID := range []int64{103, 104, 105} {
			_, err := qtx.LockEligibleSelection(ctx, db.LockEligibleSelectionParams{RepositoryID: repoID, UserID: 1})
			if !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("LockEligibleSelection(%d, 1) err = %v, want pgx.ErrNoRows", repoID, err)
			}
		}
	})

	t.Run("ListFollowedRepositoryIDs and ListEligibleRepositoryIDsForUser", func(t *testing.T) {
		followed, err := f.q.ListFollowedRepositoryIDs(ctx)
		if err != nil {
			t.Fatalf("ListFollowedRepositoryIDs: %v", err)
		}
		if want := []int64{101, 102, 106}; !slices.Equal(followed, want) {
			t.Fatalf("ListFollowedRepositoryIDs = %v, want %v", followed, want)
		}
		forUser, err := f.q.ListEligibleRepositoryIDsForUser(ctx, 1)
		if err != nil {
			t.Fatalf("ListEligibleRepositoryIDsForUser(1): %v", err)
		}
		if want := []int64{102, 106}; !slices.Equal(forUser, want) {
			t.Fatalf("ListEligibleRepositoryIDsForUser(1) = %v, want %v", forUser, want)
		}
	})

	t.Run("non-eligibility selection queries include suspended follows", func(t *testing.T) {
		selected, err := f.q.ListSelectedIDs(ctx, db.ListSelectedIDsParams{
			UserID:        1,
			RepositoryIds: []int64{102, 103, 104, 106},
		})
		if err != nil {
			t.Fatalf("ListSelectedIDs: %v", err)
		}
		if !slices.Contains(selected, 104) || !slices.Contains(selected, 102) || slices.Contains(selected, 103) {
			t.Fatalf("ListSelectedIDs = %v, want active + suspended (102,104,106) and not deselected 103", selected)
		}

		allSelected, err := f.q.ListAllSelectedIDs(ctx, 1)
		if err != nil {
			t.Fatalf("ListAllSelectedIDs: %v", err)
		}
		if !slices.Contains(allSelected, 104) || slices.Contains(allSelected, 103) {
			t.Fatalf("ListAllSelectedIDs = %v, want suspended 104 included and deselected 103 excluded", allSelected)
		}
	})

	t.Run("is_eligible_selection direct SQL truth table including NULLs", func(t *testing.T) {
		now := base
		for _, tc := range []struct {
			name      string
			desel     *time.Time
			suspended *time.Time
			isDemo    *bool
			want      bool
		}{
			{name: "active non-demo", isDemo: ptr(false), want: true},
			{name: "deselected", desel: &now, isDemo: ptr(false), want: false},
			{name: "suspended", suspended: &now, isDemo: ptr(false), want: false},
			{name: "demo true", isDemo: ptr(true), want: false},
			{name: "demo null", isDemo: nil, want: false},
			{name: "all set", desel: &now, suspended: &now, isDemo: ptr(true), want: false},
		} {
			var got bool
			if err := f.pool.QueryRow(ctx,
				`SELECT is_eligible_selection($1::timestamptz, $2::timestamptz, $3::boolean)`,
				tc.desel, tc.suspended, tc.isDemo,
			).Scan(&got); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if got != tc.want {
				t.Fatalf("%s: got %v, want %v", tc.name, got, tc.want)
			}
		}
	})
}

func TestLockEligibleSelectionScope(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.user(1)
	f.repo(100)
	f.selection(1, 100, "owned")

	t.Run("uncommitted repository lock does not block LockEligibleSelection", func(t *testing.T) {
		tx1, err := f.begin()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx1.Exec(ctx, `UPDATE repositories SET description = description WHERE id = $1`, int64(100)); err != nil {
			t.Fatal(err)
		}

		tx2, err := f.begin()
		if err != nil {
			t.Fatal(err)
		}
		uid, err := f.q.WithTx(tx2).LockEligibleSelection(ctx, db.LockEligibleSelectionParams{RepositoryID: 100, UserID: 1})
		if err != nil || uid != 1 {
			t.Fatalf("LockEligibleSelection while repositories row locked = (%d, %v), want (1, nil)", uid, err)
		}
		_ = tx2.Rollback(ctx)
		_ = tx1.Rollback(ctx)
	})

	for _, col := range []string{"deselected_at", "follow_suspended_at"} {
		t.Run("uncommitted selection "+col+" update returns 55P03 immediately", func(t *testing.T) {
			tx1, err := f.begin()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := tx1.Exec(ctx, `UPDATE selections SET `+col+` = now() WHERE user_id = $1 AND repository_id = $2`, int64(1), int64(100)); err != nil {
				t.Fatal(err)
			}

			tx2, err := f.begin()
			if err != nil {
				t.Fatal(err)
			}
			_, err = f.q.WithTx(tx2).LockEligibleSelection(ctx, db.LockEligibleSelectionParams{RepositoryID: 100, UserID: 1})
			if !isLockNotAvailable(err) {
				t.Fatalf("LockEligibleSelection during uncommitted %s update err = %v, want SQLSTATE 55P03", col, err)
			}
			_ = tx2.Rollback(ctx)
			_ = tx1.Rollback(ctx)
		})
	}
}

func TestLockEligibleSelectionPostCommitAndExplainInlining(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.user(1)
	f.repo(100)
	f.repo(101)
	f.selection(1, 100, "owned")
	f.selection(1, 101, "owned")

	explain := func(sql string) string {
		t.Helper()
		rows, err := f.pool.Query(ctx, sql)
		if err != nil {
			t.Fatalf("EXPLAIN %q: %v", sql, err)
		}
		defer rows.Close()
		var lines []string
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				t.Fatal(err)
			}
			lines = append(lines, line)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return strings.Join(lines, "\n")
	}

	lockPlan := explain(`EXPLAIN (VERBOSE) SELECT s.user_id FROM selections s JOIN repositories r ON r.id = s.repository_id WHERE s.repository_id = 100 AND s.user_id = 1 AND is_eligible_selection(s.deselected_at, s.follow_suspended_at, r.is_demo) FOR SHARE OF s NOWAIT`)
	forPlan := explain(`EXPLAIN (VERBOSE) SELECT s.repository_id FROM selections s JOIN repositories r ON r.id = s.repository_id WHERE s.user_id = 1 AND is_eligible_selection(s.deselected_at, s.follow_suspended_at, r.is_demo) ORDER BY s.repository_id`)

	for name, plan := range map[string]string{
		"LockEligibleSelection":            lockPlan,
		"ListEligibleRepositoryIDsForUser": forPlan,
	} {
		if strings.Contains(plan, "is_eligible_selection") {
			t.Fatalf("%s plan did not inline is_eligible_selection:\n%s", name, plan)
		}
		if !strings.Contains(plan, "follow_suspended_at IS NULL") {
			t.Fatalf("%s plan missing inlined follow_suspended_at IS NULL:\n%s", name, plan)
		}
		if !strings.Contains(plan, "selections_user_active") && !strings.Contains(plan, "deselected_at IS NULL") {
			t.Fatalf("%s plan missing deselected_at IS NULL / selections_user_active predicate:\n%s", name, plan)
		}
	}

	if !strings.Contains(lockPlan, "LockRows") {
		t.Fatalf("LockEligibleSelection plan missing LockRows:\n%s", lockPlan)
	}
	if strings.Count(lockPlan, "on public.selections") != 1 {
		t.Fatalf("LockEligibleSelection plan should scan selections once:\n%s", lockPlan)
	}
	if !strings.Contains(lockPlan, "s.ctid") {
		t.Fatalf("LockEligibleSelection plan did not project s.ctid for row locking:\n%s", lockPlan)
	}

	// Post-commit deselection must return pgx.ErrNoRows.
	f.exec(`UPDATE selections SET deselected_at = now() WHERE user_id = $1 AND repository_id = $2`, int64(1), int64(100))
	tx, err := f.begin()
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.q.WithTx(tx).LockEligibleSelection(ctx, db.LockEligibleSelectionParams{RepositoryID: 100, UserID: 1})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("post-commit deselection LockEligibleSelection err = %v, want pgx.ErrNoRows", err)
	}
	_ = tx.Rollback(ctx)

	// Post-commit follow suspension must also return pgx.ErrNoRows.
	f.exec(`UPDATE selections SET follow_suspended_at = now() WHERE user_id = $1 AND repository_id = $2`, int64(1), int64(101))
	tx2, err := f.begin()
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.q.WithTx(tx2).LockEligibleSelection(ctx, db.LockEligibleSelectionParams{RepositoryID: 101, UserID: 1})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("post-commit suspension LockEligibleSelection err = %v, want pgx.ErrNoRows", err)
	}
}

func ptr[T any](v T) *T {
	return &v
}

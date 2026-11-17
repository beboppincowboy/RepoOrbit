package ingestion

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// applyDeletion turns a signed, deduplicated deletion delivery into a tombstone plus removal of the stored copy.
// Only issues, issue comments and releases are deletable by signal; everything else is repaired by polling. A
// repository the app never stored has nothing to protect or remove; a stored one is handled whether or not anyone
// still selects it. The tombstone is written even if a follow-up lookup would be unavailable: the signature is the evidence.
func applyDeletion(ctx context.Context, tx pgx.Tx, repoID int64, event, action string, p webhookPayload) error {
	if action != "deleted" {
		return nil
	}
	var resource string
	var id int64
	switch event {
	case "issues":
		resource, id = "issues", p.Issue.ID
	case "issue_comment":
		resource, id = "comments", p.Comment.ID
	case "release":
		resource, id = "releases", p.Release.ID
	default:
		return nil
	}
	if id <= 0 {
		return errWebhookPayload{}
	}
	// Gate on the repository being stored, not on a current selection: deselecting keeps the evidence, so a
	// deletion must still reach it. A repository this app never stored has nothing to remove (and no row for the
	// tombstone to reference).
	var stored bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM repositories WHERE id=$1 AND NOT is_demo)`, repoID).Scan(&stored); err != nil || !stored {
		return err
	}
	exec := func(sql string, args ...any) error {
		_, err := tx.Exec(ctx, sql, args...)
		return err
	}
	if err := exec(`INSERT INTO activity_tombstones(repository_id,resource,source_id) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, repoID, resource, id); err != nil {
		return err
	}
	switch resource {
	case "issues":
		if p.Issue.Number <= 0 {
			return errWebhookPayload{}
		}
		for _, stmt := range []struct {
			sql  string
			args []any
		}{
			{`DELETE FROM activity_issue_labels WHERE issue_id=$1`, []any{id}},
			{`DELETE FROM activity_issue_assignees WHERE issue_id=$1`, []any{id}},
			{`DELETE FROM activity_issue_events WHERE repository_id=$1 AND issue_id=$2`, []any{repoID, id}},
			{`DELETE FROM activity_comments WHERE repository_id=$1 AND issue_number=$2`, []any{repoID, p.Issue.Number}},
			{`DELETE FROM activity_children WHERE repository_id=$1 AND resource='comments' AND parent_id=$2`, []any{repoID, p.Issue.Number}},
			{`DELETE FROM activity_issues WHERE repository_id=$1 AND source_id=$2`, []any{repoID, id}},
		} {
			if err := exec(stmt.sql, stmt.args...); err != nil {
				return err
			}
		}
		return nil
	case "comments":
		return exec(`DELETE FROM activity_comments WHERE repository_id=$1 AND source_id=$2`, repoID, id)
	default: // releases
		return exec(`DELETE FROM activity_releases WHERE repository_id=$1 AND source_id=$2`, repoID, id)
	}
}

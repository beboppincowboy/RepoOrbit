package ingestion

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/repoorbit/repoorbit/backend/internal/db"
	"github.com/repoorbit/repoorbit/backend/internal/githubapp"
	"github.com/repoorbit/repoorbit/backend/internal/jobs"
)

// isStatusLabel matches SQL status_label_rank: the three status labels of ADR 0003, case-insensitively.
func isStatusLabel(name string) bool {
	switch strings.ToLower(name) {
	case "status:todo", "status:in-progress", "status:review":
		return true
	}
	return false
}

func hasLabel(issue githubapp.Issue, match func(string) bool) bool {
	for _, l := range issue.Labels {
		if match(l.Name) {
			return true
		}
	}
	return false
}

// enqueueStatusCleanup schedules the app's status label cleanup for a repository with status labels on when the issue
// just projected inside tx is closed and still carries a status label (R20). Only issues a sync, webhook or write
// actually projects trigger it, so turning status labels on never sweeps old closed issues.
func enqueueStatusCleanup(ctx context.Context, tx pgx.Tx, repoID int64, issue githubapp.Issue) error {
	if issue.State != "closed" || !hasLabel(issue, isStatusLabel) {
		return nil
	}
	var on bool
	err := tx.QueryRow(ctx, `SELECT status_labels FROM repositories WHERE id=$1 AND NOT is_demo`, repoID).Scan(&on)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && !on {
		return nil
	}
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO jobs (kind, key, state, run_at) VALUES ($1, $2, 'queued', now())
		ON CONFLICT (kind, key) DO UPDATE SET run_at = CASE WHEN jobs.state = 'queued' THEN LEAST(jobs.run_at, EXCLUDED.run_at) ELSE jobs.run_at END,
		pending = CASE WHEN jobs.state = 'queued' THEN jobs.pending ELSE true END, updated_at = now()`,
		string(jobs.KindStatusLabelCleanup), fmt.Sprintf("repo:%d", repoID))
	return err
}

// CleanupStatusLabels is the jobs.KindStatusLabelCleanup handler (Key "repo:<repositories.id>"): the one RepoOrbit
// write with no user behind it (R20, KTD3). For each stored closed issue that still has a status label in a
// repository with status labels on, it re-reads the issue with the installation token and, only if GitHub still shows
// it closed with that label, removes the label, records the removal in app_operations as done by the app, and projects
// the issue as read back. Nothing is written without an installation that holds Issues write. Removing a label is
// idempotent, so a removal left unknown is resolved by the job's own retry rather than a Recheck.
func (d *Deps) CleanupStatusLabels(ctx context.Context, job jobs.Job, fence jobs.ClaimFence) error {
	repoID, err := parseKey("repo:", job.Key)
	if err != nil {
		return jobs.Permanent(err)
	}
	stale, err := db.New(d.Pool).ListStaleStatusLabels(ctx, repoID)
	if err != nil || len(stale) == 0 {
		return err
	}
	client, ok := d.GitHub.(*githubapp.Client)
	if !ok || d.AppKey == nil || d.InstallationCredentials == nil {
		return nil // no GitHub App configured: labels stay, and closed issues still show in Done
	}
	var fullName string
	if err := d.Pool.QueryRow(ctx, `SELECT name FROM repositories WHERE id=$1`, repoID).Scan(&fullName); err != nil {
		return err
	}
	installation, issuesWrite, err := client.RepositoryInstallationGrant(ctx, d.AppKey, fullName)
	var api *githubapp.APIError
	if errors.As(err, &api) && api.RateLimited() {
		return &jobs.Defer{RetryAt: api.RetryAt, Reason: CodeRateLimited}
	}
	if err != nil {
		return fmt.Errorf("status cleanup: installation lookup: %w", err)
	}
	if installation == 0 || !issuesWrite {
		return nil
	}
	token, err := d.InstallationCredentials.AccessToken(ctx, installation)
	if err != nil {
		return d.installationFetchError(installation, token, err)
	}
	var numbers []int64
	for _, s := range stale {
		if !slices.Contains(numbers, s.Number) {
			numbers = append(numbers, s.Number)
		}
	}
	unresolved := false
	for _, number := range numbers {
		issue, err := client.FetchIssue(ctx, token, fullName, number)
		if errors.As(err, &api) && api.NotFound() {
			continue // gone or transferred; a deletion webhook or the next sync removes it
		}
		if err != nil {
			return d.installationFetchError(installation, token, err)
		}
		if issue.PullRequest != nil {
			continue
		}
		var labels []string
		if issue.State == "closed" {
			for _, l := range issue.Labels {
				if isStatusLabel(l.Name) {
					labels = append(labels, l.Name)
				}
			}
		}
		type removal struct {
			opID  int64
			label string
			err   error
		}
		var removals []removal
		for _, label := range labels {
			var opID int64
			if err := fence.Commit(ctx, func(tx pgx.Tx) (err error) {
				opID, err = db.New(tx).StartAppOperation(ctx, db.StartAppOperationParams{RepositoryID: repoID, IssueNumber: number, Label: label})
				return err
			}); err != nil {
				return err
			}
			removals = append(removals, removal{opID, label, client.RemoveLabel(ctx, token, fullName, number, label)})
		}
		if len(labels) > 0 {
			if issue, err = client.FetchIssue(ctx, token, fullName, number); err != nil {
				// The removals stay unknown; the retry finds the labels gone (or removes them again) and resolves them.
				return d.installationFetchError(installation, token, err)
			}
		}
		err = fence.Commit(ctx, func(tx pgx.Tx) error {
			q := db.New(tx)
			for _, r := range removals {
				state, msg := removalOutcome(issue, r.label, r.err)
				if state == "unknown" {
					unresolved = true
					continue
				}
				body := []byte(`{}`)
				if msg != "" {
					body, _ = json.Marshal(map[string]string{"error": msg})
				}
				if err := q.FinishAppOperation(ctx, db.FinishAppOperationParams{ID: r.opID, State: state, Outcome: body}); err != nil {
					return err
				}
			}
			// Only refresh an issue that is still stored: a private cleanup may have purged the repository's issues.
			if err := LockIssue(ctx, tx, repoID, number); err != nil {
				return err
			}
			var stored bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM activity_issues WHERE repository_id=$1 AND number=$2)`, repoID, number).Scan(&stored); err != nil || !stored {
				return err
			}
			return ProjectConfirmedIssue(ctx, tx, repoID, issue)
		})
		if err != nil {
			return err
		}
	}
	if unresolved {
		return errors.New("status cleanup: a label removal has no reliable answer yet")
	}
	if len(stale) == 100 {
		return &jobs.Defer{RetryAt: d.Clock.Now(), Reason: "continue"}
	}
	return nil
}

// removalOutcome decides a label removal from the issue as read back afterwards: succeeded once the label is gone,
// whatever the removal answered; failed when it is still there and the removal was definitely refused (or was
// accepted yet the label is back); unknown when it is still there and the removal got no reliable answer.
func removalOutcome(issue githubapp.Issue, label string, removeErr error) (state, msg string) {
	if !hasLabel(issue, func(name string) bool { return strings.EqualFold(name, label) }) {
		return "succeeded", ""
	}
	var api *githubapp.APIError
	switch {
	case removeErr == nil:
		return "failed", "GitHub accepted the removal but the label is still on the issue"
	case errors.Is(removeErr, githubapp.ErrNotSent):
		return "failed", "the request could not be prepared; nothing was sent to GitHub"
	case errors.As(removeErr, &api) && api.Status >= 400 && api.Status < 500:
		return "failed", fmt.Sprintf("GitHub rejected the request (status %d)", api.Status)
	}
	return "unknown", ""
}

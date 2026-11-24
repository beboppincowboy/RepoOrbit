package ingestion

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

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
	return jobs.Enqueue(ctx, tx, jobs.KindStatusLabelCleanup, fmt.Sprintf("repo:%d", repoID), time.Now())
}

// errStatusLabelsOff stops a cleanup run when status labels were turned off while it ran.
var errStatusLabelsOff = errors.New("status labels turned off")

// errIssueBusy means another transaction holds the issue: the cleanup never waits for it while it holds its job claim
// (a sync or an issue write may be waiting on that claim to enqueue this very job), so it retries shortly instead.
var errIssueBusy = errors.New("issue busy")

// cleanupRetry is how long the cleanup waits after temporary GitHub trouble. A Defer spends no attempt, so an outage
// never leaves the job dead (later syncs cannot revive a dead job; only an operator can).
// ponytail: fixed interval with no backoff; an outage costs one pass per affected repository every 15 minutes.
const cleanupRetry = 15 * time.Minute

// githubError decides the cleanup's result for a failed GitHub call. Calls made with the installation token pass its
// installation, so an API error is recorded on it; app-JWT calls pass 0. Temporary trouble (no HTTP answer, 5xx, 401,
// a rate limit) defers without spending an attempt. Anything else (another 4xx, a malformed answer) is a plain error
// that spends one, so a permanent refusal still ends in a dead job an operator sees.
func (d *Deps) githubError(installation int64, token string, err error) error {
	var api *githubapp.APIError
	isAPI := errors.As(err, &api)
	if isAPI && installation != 0 {
		d.InstallationCredentials.RecordAPIError(installation, token, api)
	}
	var transport *url.Error
	switch {
	case isAPI && api.RateLimited():
		return &jobs.Defer{RetryAt: api.RetryAt, Reason: CodeRateLimited}
	case errors.As(err, &transport), isAPI && (api.Unauthorized() || api.Status >= 500):
		log.Printf("ingestion: status cleanup deferred: %v", err)
		return &jobs.Defer{RetryAt: d.Clock.Now().Add(cleanupRetry), Reason: CodeUpstream}
	}
	return fmt.Errorf("status cleanup: %w", err)
}

// CleanupStatusLabels is the jobs.KindStatusLabelCleanup handler (Key "repo:<repositories.id>"): the one RepoOrbit
// write with no user behind it (R20, KTD3). In a repository with status labels on, for each stored closed issue that
// still has a status label (and each issue with a removal left unknown), it re-reads the issue with the installation
// token and, only if GitHub still shows it closed with that label, removes the label, records the removal in
// app_operations as done by the app, and projects the issue as read back. Nothing is written without an installation
// that holds Issues write and covers this repository's ID, or after status labels are turned off. Removing a label is
// idempotent, so a removal left unknown is resolved by a later run rather than a Recheck. Temporary GitHub trouble and
// unknown removals defer the job (githubError); only permanent refusals and database or fence errors spend attempts.
func (d *Deps) CleanupStatusLabels(ctx context.Context, job jobs.Job, fence jobs.ClaimFence) error {
	repoID, err := parseKey("repo:", job.Key)
	if err != nil {
		return jobs.Permanent(err)
	}
	candidates, err := db.New(d.Pool).ListStatusCleanupCandidates(ctx, repoID)
	if err != nil || len(candidates) == 0 {
		return err
	}
	client, ok := d.GitHub.(*githubapp.Client)
	if !ok || d.AppKey == nil || d.InstallationCredentials == nil {
		return nil // no GitHub App configured: labels stay, and closed issues still show in Done
	}
	var storedName string
	if err := d.Pool.QueryRow(ctx, `SELECT name FROM repositories WHERE id=$1`, repoID).Scan(&storedName); err != nil {
		return err
	}
	installation, issuesWrite, err := client.RepositoryInstallationGrant(ctx, d.AppKey, storedName)
	if err != nil {
		return d.githubError(0, "", err)
	}
	if installation == 0 || !issuesWrite {
		return nil
	}
	token, err := d.InstallationCredentials.AccessToken(ctx, installation)
	if err != nil {
		return d.githubError(installation, token, err)
	}
	// The stored name may be stale (a rename or transfer): read the repository by ID, so every write below goes to this
	// repository's live name.
	repo, err := client.FetchRepo(ctx, token, repoID)
	var api *githubapp.APIError
	if errors.As(err, &api) && api.NotFound() {
		return nil
	}
	if err != nil {
		return d.githubError(installation, token, err)
	}
	if repo.ID != repoID || repo.FullName == "" {
		return nil
	}
	fullName := repo.FullName
	if !strings.EqualFold(fullName, storedName) {
		// The installation was found under a stale name and may belong to another repository: it must also be the one
		// that covers this repository under its live name.
		live, liveWrite, err := client.RepositoryInstallationGrant(ctx, d.AppKey, fullName)
		if err != nil {
			return d.githubError(0, "", err)
		}
		if live != installation || !liveWrite {
			return nil
		}
	}
	unresolved, progressed := false, false
	for _, c := range candidates {
		issue, err := client.FetchIssue(ctx, token, fullName, c.Number)
		if errors.As(err, &api) && (api.NotFound() || api.Status == http.StatusGone) {
			continue // missing or deleted; a deletion webhook or the next sync removes it
		}
		if err != nil {
			return d.githubError(installation, token, err)
		}
		if issue.PullRequest != nil || issue.ID != c.SourceID {
			continue // not the issue that was projected under this number
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
			err := fence.Commit(ctx, func(tx pgx.Tx) error {
				var on bool
				if err := tx.QueryRow(ctx, `SELECT status_labels FROM repositories WHERE id=$1 FOR SHARE`, repoID).Scan(&on); err != nil {
					return err
				}
				if !on {
					return errStatusLabelsOff
				}
				var err error
				opID, err = db.New(tx).StartAppOperation(ctx, db.StartAppOperationParams{RepositoryID: repoID, IssueNumber: c.Number, Label: label})
				return err
			})
			if errors.Is(err, errStatusLabelsOff) {
				return nil
			}
			if err != nil {
				return err
			}
			removeErr := client.RemoveLabel(ctx, token, fullName, c.Number, label)
			if errors.As(removeErr, &api) && api.RateLimited() {
				// Stop the batch for the cooldown; this removal stays unknown and a later run resolves it.
				return d.githubError(installation, token, removeErr)
			}
			removals = append(removals, removal{opID, label, removeErr})
		}
		if len(labels) > 0 {
			if issue, err = client.FetchIssue(ctx, token, fullName, c.Number); err != nil {
				// The removals stay unknown; a later run finds the labels gone (or removes them again) and resolves them.
				return d.githubError(installation, token, err)
			}
		}
		err = fence.Commit(ctx, func(tx pgx.Tx) error {
			var locked bool
			if err := tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock(hashtextextended(format('issue-write:%s:%s', $1::bigint, $2::bigint), 0))`, repoID, c.Number).Scan(&locked); err != nil {
				return err
			}
			if !locked {
				return errIssueBusy
			}
			// Lock the stored row so a concurrent private purge cannot delete it under this refresh (which would turn the
			// refresh into an insert of purged data); a purge that got there first leaves nothing to refresh.
			var stored bool
			err := tx.QueryRow(ctx, `SELECT true FROM activity_issues WHERE source_id=$1 AND repository_id=$2 FOR UPDATE NOWAIT`, c.SourceID, repoID).Scan(&stored)
			switch {
			case isLockNotAvailable(err):
				return errIssueBusy
			case errors.Is(err, pgx.ErrNoRows):
			case err != nil:
				return err
			}
			q := db.New(tx)
			finish := func(id int64, state, msg string) error {
				body := []byte(`{}`)
				if msg != "" {
					body, _ = json.Marshal(map[string]string{"error": msg})
				}
				return q.FinishAppOperation(ctx, db.FinishAppOperationParams{ID: id, State: state, Outcome: body})
			}
			sent := map[int64]bool{}
			for _, r := range removals {
				sent[r.opID] = true
				state, msg := removalOutcome(issue, r.label, r.err)
				if state == "unknown" {
					unresolved = true
					continue
				}
				progressed = progressed || state == "succeeded"
				if err := finish(r.opID, state, msg); err != nil {
					return err
				}
			}
			// Removals an earlier run left unknown: settled by what GitHub shows now.
			earlier, err := q.ListUnknownAppOperations(ctx, db.ListUnknownAppOperationsParams{RepositoryID: repoID, IssueNumber: c.Number})
			if err != nil {
				return err
			}
			for _, o := range earlier {
				if sent[o.ID] {
					continue
				}
				onIssue := hasLabel(issue, func(name string) bool { return strings.EqualFold(name, o.Label) })
				switch {
				case !onIssue:
					err = finish(o.ID, "succeeded", "")
				case issue.State == "open":
					err = finish(o.ID, "failed", "the issue was reopened with the label still on it")
				default:
					unresolved = true
					continue
				}
				if err != nil {
					return err
				}
				progressed = true
			}
			if !stored {
				return nil
			}
			if issue.State == "open" || !hasLabel(issue, isStatusLabel) {
				progressed = true // the issue leaves the candidates
			}
			// Not the same-second variant: this readback was taken before the lock, so it must not replace a copy an
			// issue write projected in the same second. It never enqueues this job again, so a refused removal cannot
			// make the cleanup schedule itself forever.
			return projectIssue(ctx, tx, repoID, issue, false, false)
		})
		if errors.Is(err, errIssueBusy) {
			return &jobs.Defer{RetryAt: d.Clock.Now().Add(raceRetry), Reason: CodeAccessMoved}
		}
		if err != nil {
			return err
		}
	}
	if unresolved {
		// A removal with no reliable answer yet: retry later without spending an attempt. The retried removal reuses
		// its unknown app_operations row, so waiting out an outage adds no rows.
		return &jobs.Defer{RetryAt: d.Clock.Now().Add(cleanupRetry), Reason: CodeUpstream}
	}
	if len(candidates) == 100 && progressed {
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

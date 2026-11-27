package analytics

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/repoorbit/repoorbit/backend/internal/db"
)

// AttentionItem is one concrete problem in a counted repository. Kind is "pr_awaiting_review", "issue_no_response",
// "stale_issue", or "failed_workflow_run" (docs/metrics-definitions.md 5.8 & 10). Since is when the item was opened,
// or for "stale_issue" / "failed_workflow_run" when it was last updated. RunID is set for "failed_workflow_run".
type AttentionItem struct {
	Kind         string
	RepositoryID int64
	FullName     string
	Number       int64
	Title        string
	URL          string
	Since        time.Time
	RunID        *int64
}

// AssignedIssue is one open issue assigned to the viewer.
type AssignedIssue struct {
	RepositoryID int64
	FullName     string
	Number       int64
	Title        string
	URL          string
	UpdatedAt    time.Time
	Status       *string // Board status (issue_board_states); nil when the repository's status labels are off
}

// CrossDailyPoint is one UTC day summed over the counted repositories; a column is nil when none was counted.
type CrossDailyPoint struct {
	Day                time.Time
	IssuesOpened       *int64
	PullRequestsMerged *int64
}

// CrossOverview summarizes several repositories with the per-repository metric definitions of LoadRepositoryOverview.
//
// A repository is counted when its issue and PR streams have converged for the window (IssuesOpened and
// PullRequestsMerged covered); otherwise it is syncing and left out of totals, the trend and NeedsAttention, never added as zero.
// A counted repository whose review histories or comment stream are still importing stays counted: metrics that need them
// become unknown (ReviewBacklog) or omit that repository's rows (NeedsAttention), exactly as its workspace shows.
type CrossOverview struct {
	Days                int
	WindowStart         time.Time
	Now                 time.Time
	RepositoriesCounted int
	RepositoriesSyncing int
	// OpenIssues and ReviewBacklog (the sum of UnreviewedOpenPullRequests) are sums over counted repositories, with
	// Through the oldest input watermark. Not covered (not_synchronized) when no repository is counted; ReviewBacklog is
	// also not covered (children_incomplete) when any counted repository's UnreviewedOpenPullRequests is unknown.
	OpenIssues    Count
	ReviewBacklog Count
	// FailingWorkflows and BuildSuccessRate aggregate the fixed 30-day window across counted repositories whose
	// actions stream is covered (docs/metrics-definitions.md 10).
	FailingWorkflows Count
	BuildSuccessRate Rate
	// NeedsAttention holds up to 25 items, oldest Since first, one row per item: PR rows from counted repositories whose
	// UnreviewedOpenPullRequests is covered, issue rows from those whose UnansweredOpenIssues is covered.
	NeedsAttention []AttentionItem
	// AssignedToMe holds up to 25 open issues, most recently updated first, from repositories whose issue stream is
	// covered (counted or not).
	AssignedToMe []AssignedIssue
	Daily        []CrossDailyPoint
}

// LoadCrossRepositoryOverview aggregates already authorized repositories in one read-only REPEATABLE READ transaction.
// The 7-, 30-, or 90-day UTC window includes the calendar date of now. The GitHub user ID scopes AssignedToMe.
// An empty repository list returns an overview with uncovered totals. Invalid windows and database failures return no overview.
func LoadCrossRepositoryOverview(ctx context.Context, pool *pgxpool.Pool, repositoryIDs []int64, assigneeGitHubID int64, days int, now time.Time) (*CrossOverview, error) {
	if err := ValidateMetricWindowDays(days); err != nil {
		return nil, err
	}
	w := newWindow(days, now)
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	q := db.New(tx)

	out := &CrossOverview{
		Days: days, WindowStart: w.start, Now: w.now, NeedsAttention: []AttentionItem{}, AssignedToMe: []AssignedIssue{},
		OpenIssues:       count(0, uncovered(ReasonNotSynchronized)),
		ReviewBacklog:    count(0, uncovered(ReasonNotSynchronized)),
		FailingWorkflows: count(0, uncovered(ReasonNotSynchronized)),
		BuildSuccessRate: rate(0, 0, uncovered(ReasonNotSynchronized)),
	}
	w30 := newWindow(30, now)
	var counted, withIssues, pullRows, issueRows, actionsRows []int64
	var openIssues, backlog, failingWorkflows, succeededRuns, completedRuns int64
	actionsCov := uncovered(ReasonNotSynchronized)
	daily := make([]CrossDailyPoint, days)
	for i := range daily {
		daily[i].Day = w.start.AddDate(0, 0, i)
	}
	for _, id := range repositoryIDs {
		one, _, err := loadMetrics(ctx, q, id, w)
		if err != nil {
			return nil, err
		}
		if one.OpenIssues.Coverage.Covered {
			withIssues = append(withIssues, id)
		}
		if !one.IssuesOpened.Coverage.Covered || !one.PullRequestsMerged.Coverage.Covered {
			out.RepositoriesSyncing++
			continue
		}
		counted = append(counted, id)
		openIssues += *one.OpenIssues.Value
		out.OpenIssues.Coverage = oldest(out.OpenIssues.Coverage, one.OpenIssues.Coverage)
		if b := one.UnreviewedOpenPullRequests; b.Coverage.Covered {
			pullRows = append(pullRows, id)
			backlog += *b.Value
			if len(pullRows) == len(counted) {
				out.ReviewBacklog.Coverage = oldest(out.ReviewBacklog.Coverage, b.Coverage)
			}
		} else {
			out.ReviewBacklog.Coverage = b.Coverage
		}
		if one.UnansweredOpenIssues.Coverage.Covered {
			issueRows = append(issueRows, id)
		}
		actCovRow, err := q.GetActionsCoverage(ctx, id)
		if err != nil {
			return nil, err
		}
		repoActCov := actionsStreamCoverage(actCovRow.ActionsWatermark, actCovRow.ActionsBoundary, actCovRow.ActionsErrorCode, w30.start)
		if repoActCov.Covered {
			actionsRows = append(actionsRows, id)
			am, err := q.GetActionsMetrics(ctx, db.GetActionsMetricsParams{
				RepositoryID: id, WindowStart: w30.startArg(), Now: w30.nowArg(),
			})
			if err != nil {
				return nil, err
			}
			failingWorkflows += am.FailingWorkflows
			succeededRuns += am.SucceededRuns
			completedRuns += am.CompletedRuns
			if len(actionsRows) == len(counted) {
				actionsCov = oldest(actionsCov, repoActCov)
			}
		} else {
			actionsCov = repoActCov
		}
		for i, d := range one.Daily {
			daily[i].IssuesOpened = add(daily[i].IssuesOpened, *d.IssuesOpened)
			daily[i].PullRequestsMerged = add(daily[i].PullRequestsMerged, *d.PullRequestsMerged)
		}
	}
	out.RepositoriesCounted = len(counted)
	out.OpenIssues = count(openIssues, out.OpenIssues.Coverage)
	out.ReviewBacklog = count(backlog, out.ReviewBacklog.Coverage)
	out.FailingWorkflows = count(failingWorkflows, actionsCov)
	out.BuildSuccessRate = rate(succeededRuns, completedRuns, actionsCov)
	out.Daily = daily

	if len(pullRows)+len(issueRows) > 0 {
		rows, err := q.ListNeedsAttention(ctx, db.ListNeedsAttentionParams{
			PullRequestRepositoryIds: pullRows, IssueRepositoryIds: issueRows, Now: w.nowArg(), StaleBefore: w.staleBefore(),
		})
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			out.NeedsAttention = append(out.NeedsAttention, AttentionItem{
				Kind: r.Kind, RepositoryID: r.RepositoryID, FullName: r.FullName, Number: r.Number, Title: r.Title, URL: r.Url, Since: r.Since.Time,
			})
		}
	}
	if len(actionsRows) > 0 {
		failedRuns, err := q.ListCrossFailedWorkflowRuns(ctx, db.ListCrossFailedWorkflowRunsParams{
			RepositoryIds: actionsRows, WindowStart: w30.startArg(), Now: w30.nowArg(),
		})
		if err != nil {
			return nil, err
		}
		if len(failedRuns) > 0 {
			failedItems := make([]AttentionItem, 0, len(failedRuns))
			for _, r := range failedRuns {
				runID := r.RunID
				failedItems = append(failedItems, AttentionItem{
					Kind: "failed_workflow_run", RepositoryID: r.RepositoryID, FullName: r.FullName,
					Number: r.RunNumber, Title: r.WorkflowName, URL: r.HtmlUrl, Since: r.UpdatedAt.Time, RunID: &runID,
				})
			}
			out.NeedsAttention = append(failedItems, out.NeedsAttention...)
			if len(out.NeedsAttention) > 25 {
				out.NeedsAttention = out.NeedsAttention[:25]
			}
		}
	}
	if len(withIssues) > 0 {
		rows, err := q.ListAssignedOpenIssues(ctx, db.ListAssignedOpenIssuesParams{RepositoryIds: withIssues, AssigneeGithubID: assigneeGitHubID})
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			issue := AssignedIssue{
				RepositoryID: r.RepositoryID, FullName: r.FullName, Number: r.Number, Title: r.Title, URL: r.Url, UpdatedAt: r.UpdatedAt.Time,
			}
			if r.Status.Valid {
				issue.Status = &r.Status.String
			}
			out.AssignedToMe = append(out.AssignedToMe, issue)
		}
	}
	return out, tx.Commit(ctx)
}

// oldest merges the coverage of one more counted repository: covered, with the earlier Through.
func oldest(total, one Coverage) Coverage {
	if !total.Covered || one.Through.Before(*total.Through) {
		return one
	}
	return total
}

func add(sum *int64, v int64) *int64 {
	if sum != nil {
		v += *sum
	}
	return &v
}

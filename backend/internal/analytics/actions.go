package analytics

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/repoorbit/repoorbit/backend/internal/db"
)

// Rate is nil unless covered; when covered with a zero denominator, Rate is nil while Numerator and Denominator are 0.
type Rate struct {
	Rate        *float64
	Numerator   *int64
	Denominator *int64
	Coverage    Coverage
}

// ActionsDailyPoint is one UTC day of completed workflow runs; columns are nil when actions coverage is not met.
type ActionsDailyPoint struct {
	Day       time.Time
	Succeeded *int64
	Failed    *int64
	Other     *int64
}

// WorkflowFailureSummary summarizes one workflow with at least one failed run in the active window.
type WorkflowFailureSummary struct {
	WorkflowID       int64
	WorkflowName     string
	FailedRuns       int64
	TotalRuns        int64
	LatestRunID      int64
	LatestStatus     string
	LatestConclusion string
	LatestAt         time.Time
	HTMLURL          string
}

// WorkflowRunSummary is one GitHub Actions workflow run with derived timing fields.
type WorkflowRunSummary struct {
	ID              int64
	WorkflowID      int64
	WorkflowName    string
	RunNumber       int64
	Event           string
	Status          string
	Conclusion      string
	HeadBranch      string
	HeadSHA         string
	HTMLURL         string
	CreatedAt       time.Time
	RunStartedAt    *time.Time
	UpdatedAt       time.Time
	DurationSeconds *int64
	QueueSeconds    *int64
}

// WorkflowJobDetail is one job inside a workflow run with derived duration and queue wait.
type WorkflowJobDetail struct {
	ID              int64
	RunID           int64
	Name            string
	Status          string
	Conclusion      string
	HTMLURL         string
	CreatedAt       time.Time
	StartedAt       *time.Time
	CompletedAt     *time.Time
	DurationSeconds *int64
	QueueSeconds    *int64
}

// WorkflowRunDetail pairs a workflow run with its synchronized jobs.
type WorkflowRunDetail struct {
	Run  WorkflowRunSummary
	Jobs []WorkflowJobDetail
}

// DeploymentSummary is one GitHub Deployment record.
type DeploymentSummary struct {
	ID          int64
	Environment string
	SHA         string
	Ref         string
	State       string
	Description string
	HTMLURL     string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// RepositoryActions holds one repository's CI/CD metrics, runs, and Deployment history for one 7/30/90-day window.
type RepositoryActions struct {
	Days                  int
	WindowStart           time.Time
	Now                   time.Time
	ActionsCoverage       Coverage
	DeploymentsCoverage   Coverage
	BuildSuccessRate      Rate
	FailedJobs            Count
	RunningJobs           Count
	DurationP50           Median
	DurationP95           Median
	QueueWaitP50          Median
	Daily                 []ActionsDailyPoint
	FailuresByWorkflow    []WorkflowFailureSummary
	RecentRuns            []WorkflowRunSummary
	Deployments           []DeploymentSummary
	DeploymentUnavailable bool
}

// DeliveryCheck is one GitHub Actions workflow run associated with a pull request, including a failing job link when present.
type DeliveryCheck struct {
	RunID          int64
	WorkflowName   string
	Status         string
	Conclusion     string
	HTMLURL        string
	UpdatedAt      time.Time
	FailingJobName *string
	FailingJobURL  *string
}

// LinkedPullRequestDelivery is one confirmed GitHub-linked pull request with its review state, CI checks, and Deployments.
type LinkedPullRequestDelivery struct {
	Number      int64
	Title       string
	State       string
	URL         string
	ReviewState *string
	HeadSHA     string
	Checks      []DeliveryCheck
	Deployments []DeploymentSummary
}

// IssueDelivery connects one repository issue to its confirmed linked pull requests, CI checks, and Deployments.
type IssueDelivery struct {
	IssueNumber           int64
	ActionsCoverage       Coverage
	DeploymentsCoverage   Coverage
	DeploymentUnavailable bool
	PullRequests          []LinkedPullRequestDelivery
}

// LoadRepositoryActions reads one authorized repository's CI/CD metrics and Deployments in a single read-only
// REPEATABLE READ transaction.
func LoadRepositoryActions(ctx context.Context, pool *pgxpool.Pool, repositoryID int64, days int, now time.Time) (*RepositoryActions, error) {
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

	covRow, err := q.GetActionsCoverage(ctx, repositoryID)
	if err != nil {
		return nil, err
	}
	actCov := actionsStreamCoverage(covRow.ActionsWatermark, covRow.ActionsBoundary, covRow.ActionsErrorCode, w.start)
	depCov := actionsStreamCoverage(covRow.DeploymentsWatermark, covRow.DeploymentsBoundary, covRow.DeploymentsErrorCode, time.Time{})

	m, err := q.GetActionsMetrics(ctx, db.GetActionsMetricsParams{
		RepositoryID: repositoryID, WindowStart: w.startArg(), Now: w.nowArg(),
	})
	if err != nil {
		return nil, err
	}

	out := &RepositoryActions{
		Days:                  days,
		WindowStart:           w.start,
		Now:                   w.now,
		ActionsCoverage:       actCov,
		DeploymentsCoverage:   depCov,
		BuildSuccessRate:      rate(m.SucceededRuns, m.CompletedRuns, actCov),
		FailedJobs:            count(m.FailedJobs, actCov),
		RunningJobs:           count(m.RunningJobs, actCov),
		DurationP50:           median(m.DurationP50Seconds, m.DurationSample, actCov),
		DurationP95:           median(m.DurationP95Seconds, m.DurationSample, actCov),
		QueueWaitP50:          median(m.QueueWaitP50Seconds, m.QueueWaitSample, actCov),
		Daily:                 []ActionsDailyPoint{},
		FailuresByWorkflow:    []WorkflowFailureSummary{},
		RecentRuns:            []WorkflowRunSummary{},
		Deployments:           []DeploymentSummary{},
		DeploymentUnavailable: true,
	}

	dailyRows, err := q.ListActionsDaily(ctx, db.ListActionsDailyParams{
		RepositoryID: repositoryID, Days: int32(days), WindowStart: w.startArg(), Now: w.nowArg(),
	})
	if err != nil {
		return nil, err
	}
	out.Daily = make([]ActionsDailyPoint, 0, len(dailyRows))
	for _, row := range dailyRows {
		out.Daily = append(out.Daily, ActionsDailyPoint{
			Day:       row.Day.Time,
			Succeeded: count(row.Succeeded, actCov).Value,
			Failed:    count(row.Failed, actCov).Value,
			Other:     count(row.Other, actCov).Value,
		})
	}

	if actCov.Covered {
		failures, err := q.ListWorkflowFailures(ctx, db.ListWorkflowFailuresParams{
			RepositoryID: repositoryID, WindowStart: w.startArg(), Now: w.nowArg(),
		})
		if err != nil {
			return nil, err
		}
		for _, f := range failures {
			out.FailuresByWorkflow = append(out.FailuresByWorkflow, WorkflowFailureSummary{
				WorkflowID:       f.WorkflowID,
				WorkflowName:     f.WorkflowName,
				FailedRuns:       f.FailedRuns,
				TotalRuns:        f.TotalRuns,
				LatestRunID:      f.LatestRunID,
				LatestStatus:     f.LatestStatus,
				LatestConclusion: f.LatestConclusion,
				LatestAt:         f.LatestAt.Time,
				HTMLURL:          f.HtmlUrl,
			})
		}

		runs, err := q.ListRecentWorkflowRuns(ctx, db.ListRecentWorkflowRunsParams{
			RepositoryID: repositoryID, WindowStart: w.startArg(), Now: w.nowArg(),
		})
		if err != nil {
			return nil, err
		}
		for _, r := range runs {
			out.RecentRuns = append(out.RecentRuns, toWorkflowRunSummary(r))
		}
	}

	if depCov.Covered {
		deps, err := q.ListDeployments(ctx, repositoryID)
		if err != nil {
			return nil, err
		}
		for _, d := range deps {
			out.Deployments = append(out.Deployments, toDeploymentSummary(d))
		}
		out.DeploymentUnavailable = len(out.Deployments) == 0
	}

	return out, tx.Commit(ctx)
}

// LoadWorkflowRunDetail reads one workflow run and its jobs for an authorized repository.
// Returns (nil, nil) when the run does not exist in repositoryID.
func LoadWorkflowRunDetail(ctx context.Context, pool *pgxpool.Pool, repositoryID, runID int64) (*WorkflowRunDetail, error) {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	q := db.New(tx)

	run, err := q.GetWorkflowRun(ctx, db.GetWorkflowRunParams{RepositoryID: repositoryID, SourceID: runID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	jobs, err := q.ListWorkflowJobs(ctx, db.ListWorkflowJobsParams{RepositoryID: repositoryID, RunID: runID})
	if err != nil {
		return nil, err
	}
	out := &WorkflowRunDetail{
		Run:  toWorkflowRunSummary(run),
		Jobs: make([]WorkflowJobDetail, 0, len(jobs)),
	}
	for _, j := range jobs {
		out.Jobs = append(out.Jobs, toWorkflowJobDetail(j))
	}
	return out, tx.Commit(ctx)
}

// LoadIssueDelivery reads confirmed linked pull requests, CI checks (with failing job links), and Deployments
// for one issue in an authorized repository.
func LoadIssueDelivery(ctx context.Context, pool *pgxpool.Pool, repositoryID, issueNumber int64) (*IssueDelivery, error) {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	q := db.New(tx)

	covRow, err := q.GetActionsCoverage(ctx, repositoryID)
	if err != nil {
		return nil, err
	}
	actCov := actionsStreamCoverage(covRow.ActionsWatermark, covRow.ActionsBoundary, covRow.ActionsErrorCode, time.Time{})
	depCov := actionsStreamCoverage(covRow.DeploymentsWatermark, covRow.DeploymentsBoundary, covRow.DeploymentsErrorCode, time.Time{})

	hasDep := false
	if depCov.Covered {
		hasDep, err = q.HasDeployment(ctx, repositoryID)
		if err != nil {
			return nil, err
		}
	}

	out := &IssueDelivery{
		IssueNumber:           issueNumber,
		ActionsCoverage:       actCov,
		DeploymentsCoverage:   depCov,
		DeploymentUnavailable: !depCov.Covered || !hasDep,
		PullRequests:          []LinkedPullRequestDelivery{},
	}

	prs, err := q.ListLinkedPullRequestsForIssue(ctx, db.ListLinkedPullRequestsForIssueParams{
		RepositoryID: repositoryID, IssueNumber: issueNumber,
	})
	if err != nil {
		return nil, err
	}
	for _, p := range prs {
		prItem, err := loadPullRequestDelivery(ctx, q, repositoryID, p, actCov.Covered, depCov.Covered)
		if err != nil {
			return nil, err
		}
		out.PullRequests = append(out.PullRequests, prItem)
	}

	return out, tx.Commit(ctx)
}

func loadPullRequestDelivery(ctx context.Context, q *db.Queries, repositoryID int64, p db.ListLinkedPullRequestsForIssueRow, actionsCovered, deploymentsCovered bool) (LinkedPullRequestDelivery, error) {
	prItem := LinkedPullRequestDelivery{
		Number:      p.Number,
		Title:       p.Title,
		State:       p.State,
		URL:         p.HtmlUrl,
		HeadSHA:     p.HeadSha,
		Checks:      []DeliveryCheck{},
		Deployments: []DeploymentSummary{},
	}
	if p.ReviewState != "" {
		rs := p.ReviewState
		prItem.ReviewState = &rs
	}
	if actionsCovered {
		runs, err := q.ListWorkflowRunsForPullRequest(ctx, db.ListWorkflowRunsForPullRequestParams{
			RepositoryID: repositoryID, PullRequestNumber: p.Number, HeadSha: p.HeadSha,
		})
		if err != nil {
			return LinkedPullRequestDelivery{}, err
		}
		for _, run := range runs {
			chk := DeliveryCheck{
				RunID:        run.SourceID,
				WorkflowName: run.WorkflowName,
				Status:       run.Status,
				Conclusion:   run.Conclusion,
				HTMLURL:      run.HtmlUrl,
				UpdatedAt:    run.UpdatedAt.Time,
			}
			if run.HasFailingJob {
				name := run.FailingJobName
				url := run.FailingJobUrl
				chk.FailingJobName = &name
				chk.FailingJobURL = &url
			}
			prItem.Checks = append(prItem.Checks, chk)
		}
	}
	if deploymentsCovered && p.HeadSha != "" {
		deps, err := q.ListDeploymentsForSHA(ctx, db.ListDeploymentsForSHAParams{
			RepositoryID: repositoryID, Sha: p.HeadSha,
		})
		if err != nil {
			return LinkedPullRequestDelivery{}, err
		}
		for _, d := range deps {
			prItem.Deployments = append(prItem.Deployments, toDeploymentSummary(d))
		}
	}
	return prItem, nil
}

func actionsStreamCoverage(watermark, boundary pgtype.Timestamptz, errorCode string, start time.Time) Coverage {
	if errorCode == ReasonPermissionDenied {
		return uncovered(ReasonPermissionDenied)
	}
	return streamCoverage(watermark, boundary, start)
}

func rate(num, den int64, c Coverage) Rate {
	out := Rate{Coverage: c}
	if c.Covered {
		out.Numerator = &num
		out.Denominator = &den
		if den > 0 {
			v := float64(num) / float64(den)
			out.Rate = &v
		}
	}
	return out
}

func toWorkflowRunSummary(r db.WorkflowRun) WorkflowRunSummary {
	s := WorkflowRunSummary{
		ID:           r.SourceID,
		WorkflowID:   r.WorkflowID,
		WorkflowName: r.WorkflowName,
		RunNumber:    r.RunNumber,
		Event:        r.Event,
		Status:       r.Status,
		Conclusion:   r.Conclusion,
		HeadBranch:   r.HeadBranch,
		HeadSHA:      r.HeadSha,
		HTMLURL:      r.HtmlUrl,
		CreatedAt:    r.CreatedAt.Time,
		UpdatedAt:    r.UpdatedAt.Time,
	}
	if r.RunStartedAt.Valid {
		t := r.RunStartedAt.Time
		s.RunStartedAt = &t
		if !t.Before(r.CreatedAt.Time) {
			q := int64(t.Sub(r.CreatedAt.Time).Seconds())
			s.QueueSeconds = &q
		}
		if r.Status == "completed" && !r.UpdatedAt.Time.Before(t) {
			d := int64(r.UpdatedAt.Time.Sub(t).Seconds())
			s.DurationSeconds = &d
		}
	}
	return s
}

func toWorkflowJobDetail(j db.WorkflowJob) WorkflowJobDetail {
	d := WorkflowJobDetail{
		ID:         j.SourceID,
		RunID:      j.RunID,
		Name:       j.Name,
		Status:     j.Status,
		Conclusion: j.Conclusion,
		HTMLURL:    j.HtmlUrl,
		CreatedAt:  j.CreatedAt.Time,
	}
	if j.StartedAt.Valid {
		t := j.StartedAt.Time
		d.StartedAt = &t
		if !t.Before(j.CreatedAt.Time) {
			q := int64(t.Sub(j.CreatedAt.Time).Seconds())
			d.QueueSeconds = &q
		}
	}
	if j.CompletedAt.Valid {
		t := j.CompletedAt.Time
		d.CompletedAt = &t
		if j.StartedAt.Valid && !t.Before(j.StartedAt.Time) {
			dur := int64(t.Sub(j.StartedAt.Time).Seconds())
			d.DurationSeconds = &dur
		}
	}
	return d
}

func toDeploymentSummary(d db.Deployment) DeploymentSummary {
	return DeploymentSummary{
		ID:          d.SourceID,
		Environment: d.Environment,
		SHA:         d.Sha,
		Ref:         d.Ref,
		State:       d.State,
		Description: d.Description,
		HTMLURL:     d.HtmlUrl,
		CreatedAt:   d.CreatedAt.Time,
		UpdatedAt:   d.UpdatedAt.Time,
	}
}

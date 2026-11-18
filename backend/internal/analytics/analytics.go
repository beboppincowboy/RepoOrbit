// Package analytics computes one repository's activity metrics (docs/metrics-definitions.md) from synchronized
// evidence in a single read-only REPEATABLE READ snapshot.
//
// Precondition: Load performs no access check. The caller must take the access fence, verify the user may read
// repositoryID, call Load, and discard the result if the fence moved (graphql/evidence.go read order).
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

// ErrInvalidDays is returned for a window other than 7, 30 or 90 days.
var ErrInvalidDays = errors.New("days must be 7, 30 or 90")

// Reason explains why a metric is not covered. It is empty when covered.
type Reason = string

// Reason codes, in precedence order (the first failing one is reported).
const (
	ReasonNotSynchronized      Reason = "not_synchronized"
	ReasonWindowBeforeBoundary Reason = "window_before_boundary"
	ReasonHistoricalGap        Reason = "historical_gap"
	ReasonChildrenIncomplete   Reason = "children_incomplete"
)

// Coverage says whether the evidence behind a metric is complete. Through is the minimum input watermark,
// set only when Covered.
type Coverage struct {
	Covered bool
	Reason  *Reason
	Through *time.Time
}

// Count is nil unless covered; a covered count with no qualifying rows is 0.
type Count struct {
	Value    *int64
	Coverage Coverage
}

// Median: not covered => both nil; covered with no samples => Seconds nil, SampleSize 0.
type Median struct {
	Seconds    *int64
	SampleSize *int64
	Coverage   Coverage
}

// Contributor is one active human in the window. Positions are an ordering for scanning, not a ranking.
type Contributor struct {
	Login              string
	PullRequestsOpened int64
	ReviewsSubmitted   int64
}

// Actor is nil on a TimelineEntry for a deleted (ghost) account.
type Actor struct {
	Login string
	IsBot bool
}

// TimelineEntry: Title is the issue/PR title or release name; Label is set for labeled/unlabeled events;
// Number is nil for releases.
type TimelineEntry struct {
	Kind       string
	OccurredAt time.Time
	SourceID   int64
	Actor      *Actor
	Title      string
	Label      *string
	Number     *int64
	URL        string
}

// DailyPoint is one UTC day; a column is nil on every day when its count is not covered.
type DailyPoint struct {
	Day                time.Time
	IssuesOpened       *int64
	IssuesClosed       *int64
	PullRequestsOpened *int64
	PullRequestsMerged *int64
}

// Overview is every first-release metric for one repository and window.
type Overview struct {
	Days              int
	WindowStart       time.Time
	Now               time.Time
	EventsCoveredFrom *time.Time

	OpenIssues                 Count
	OpenPullRequests           Count
	OpenIssuesAssigned         Count
	StaleOpenIssues            Count
	StaleOpenPullRequests      Count
	UnreviewedOpenPullRequests Count
	UnansweredOpenIssues       Count

	IssuesOpened       Count
	IssuesClosed       Count
	IssuesReopened     Count
	PullRequestsOpened Count
	PullRequestsMerged Count
	ReleasesPublished  Count

	FirstResponseTime         Median
	IssuesWithoutResponse     Count
	FirstReviewTime           Median
	PullRequestsNeverReviewed Count
	MergeTime                 Median

	ActiveContributors    Count
	ActiveBotContributors Count
	Contributors          []Contributor
	ContributorsCoverage  Coverage

	Timeline         []TimelineEntry
	TimelineCoverage TimelineCoverage

	Daily []DailyPoint
}

// TimelineCoverage reports evidence coverage for each timeline group.
type TimelineCoverage struct {
	PullRequestMerged Coverage
	ReleasePublished  Coverage
	IssueOpened       Coverage
	IssueEvents       Coverage
}

// ValidateDays checks the supported UTC calendar windows.
func ValidateDays(days int) error {
	switch days {
	case 7, 30, 90:
		return nil
	default:
		return ErrInvalidDays
	}
}

// Load computes the overview for repositoryID over the last days (7, 30 or 90) UTC days ending at now.
func Load(ctx context.Context, pool *pgxpool.Pool, repositoryID int64, days int, now time.Time) (*Overview, error) {
	if err := ValidateDays(days); err != nil {
		return nil, err
	}
	now = now.UTC()
	today := now.Truncate(24 * time.Hour)
	start := today.AddDate(0, 0, 1-days)
	startArg := pgtype.Timestamptz{Time: start, Valid: true}
	nowArg := pgtype.Timestamptz{Time: now, Valid: true}
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	q := db.New(tx)
	c, err := q.GetMetricCoverage(ctx, db.GetMetricCoverageParams{RepositoryID: repositoryID, WindowStart: startArg, Now: nowArg})
	if err != nil {
		return nil, err
	}
	m, err := q.GetMetricCounts(ctx, db.GetMetricCountsParams{
		RepositoryID: repositoryID, WindowStart: startArg, Now: nowArg,
		StaleBefore: pgtype.Timestamptz{Time: today.AddDate(0, 0, -29), Valid: true},
	})
	if err != nil {
		return nil, err
	}

	out := assemble(days, start, now, c, m)
	participants := out.ContributorsCoverage
	coverage := out.TimelineCoverage
	if participants.Covered {
		rows, err := q.ListMetricContributors(ctx, db.ListMetricContributorsParams{RepositoryID: repositoryID, WindowStart: startArg, Now: nowArg})
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			out.Contributors = append(out.Contributors, Contributor{
				Login: row.Login, PullRequestsOpened: row.PullRequestsOpened, ReviewsSubmitted: row.ReviewsSubmitted,
			})
		}
	}
	timeline, err := q.ListMetricTimeline(ctx, db.ListMetricTimelineParams{
		RepositoryID: repositoryID, WindowStart: startArg, Now: nowArg,
		IncludeReleases: coverage.ReleasePublished.Covered, IncludeMerged: coverage.PullRequestMerged.Covered,
		IncludeIssues: coverage.IssueOpened.Covered, IncludeEvents: coverage.IssueEvents.Covered, EventsFrom: c.EventsCoveredFrom,
	})
	if err != nil {
		return nil, err
	}
	for _, row := range timeline {
		entry := TimelineEntry{
			Kind: row.Kind, OccurredAt: row.OccurredAt.Time, SourceID: row.SourceID, Title: row.Title, URL: row.Url,
		}
		if row.ActorID.Valid {
			entry.Actor = &Actor{Login: row.ActorLogin.String, IsBot: row.ActorType.String == "Bot"}
		}
		if row.Label.Valid {
			entry.Label = &row.Label.String
		}
		if row.Number.Valid {
			entry.Number = &row.Number.Int64
		}
		out.Timeline = append(out.Timeline, entry)
	}
	daily, err := q.ListMetricDaily(ctx, db.ListMetricDailyParams{RepositoryID: repositoryID, Days: int32(days), WindowStart: startArg, Now: nowArg})
	if err != nil {
		return nil, err
	}
	out.Daily = make([]DailyPoint, 0, len(daily))
	for _, row := range daily {
		out.Daily = append(out.Daily, DailyPoint{
			Day: row.Day.Time, IssuesOpened: count(row.IssuesOpened, out.IssuesOpened.Coverage).Value,
			IssuesClosed:       count(row.IssuesClosed, out.IssuesClosed.Coverage).Value,
			PullRequestsOpened: count(row.PullRequestsOpened, out.PullRequestsOpened.Coverage).Value,
			PullRequestsMerged: count(row.PullRequestsMerged, out.PullRequestsMerged.Coverage).Value,
		})
	}
	return out, tx.Commit(ctx)
}

// UnsynchronizedOverview returns the window with unknown metrics and no evidence.
func UnsynchronizedOverview(days int, now time.Time) *Overview {
	now = now.UTC()
	start := now.Truncate(24*time.Hour).AddDate(0, 0, 1-days)
	out := assemble(days, start, now, db.GetMetricCoverageRow{}, db.GetMetricCountsRow{})
	for day := range days {
		out.Daily = append(out.Daily, DailyPoint{Day: start.AddDate(0, 0, day)})
	}
	return out
}

func assemble(days int, start, now time.Time, c db.GetMetricCoverageRow, m db.GetMetricCountsRow) *Overview {
	issues := streamCoverage(c.IssuesWatermark, c.IssuesBoundary, time.Time{})
	pulls := streamCoverage(c.PullRequestsWatermark, c.PullRequestsBoundary, time.Time{})
	windowIssues := streamCoverage(c.IssuesWatermark, c.IssuesBoundary, start)
	windowPulls := streamCoverage(c.PullRequestsWatermark, c.PullRequestsBoundary, start)
	releases := streamCoverage(c.ReleasesWatermark, c.ReleasesBoundary, start)
	events := streamCoverage(c.IssueEventsWatermark, c.IssueEventsBoundary, time.Time{})
	eventTimeline := combineCoverage(windowIssues, events)
	if eventTimeline.Covered && !c.EventsCoveredFrom.Valid {
		eventTimeline = uncovered(ReasonHistoricalGap)
	}
	reopens := eventTimeline
	if reopens.Covered && c.EventsCoveredFrom.Time.After(start) {
		reopens = uncovered(ReasonHistoricalGap)
	}
	openComments := childCoverage(issues, c.OpenIssueCommentsMissing, c.OpenIssueCommentsThrough)
	openReviews := childCoverage(pulls, c.OpenPrReviewsMissing, c.OpenPrReviewsThrough)
	createdComments := childCoverage(windowIssues, c.CreatedIssueCommentsMissing, c.CreatedIssueCommentsThrough)
	createdReviews := childCoverage(windowPulls, c.CreatedPrReviewsMissing, c.CreatedPrReviewsThrough)
	participants := childCoverage(windowPulls, c.UpdatedPrReviewsMissing, c.UpdatedPrReviewsThrough)

	out := &Overview{
		Days: days, WindowStart: start, Now: now,
		OpenIssues:                 count(m.OpenIssues, issues),
		OpenPullRequests:           count(m.OpenPullRequests, pulls),
		OpenIssuesAssigned:         count(m.OpenIssuesAssigned, issues),
		StaleOpenIssues:            count(m.StaleOpenIssues, issues),
		StaleOpenPullRequests:      count(m.StaleOpenPullRequests, pulls),
		UnreviewedOpenPullRequests: count(m.UnreviewedOpenPullRequests, openReviews),
		UnansweredOpenIssues:       count(m.UnansweredOpenIssues, openComments),
		IssuesOpened:               count(m.IssuesOpened, windowIssues),
		IssuesClosed:               count(m.IssuesClosed, windowIssues),
		IssuesReopened:             count(m.IssuesReopened, reopens),
		PullRequestsOpened:         count(m.PullRequestsOpened, windowPulls),
		PullRequestsMerged:         count(m.PullRequestsMerged, windowPulls),
		ReleasesPublished:          count(m.ReleasesPublished, releases),
		FirstResponseTime:          median(m.FirstResponseSeconds, m.FirstResponseSample, createdComments),
		IssuesWithoutResponse:      count(m.IssuesWithoutResponse, createdComments),
		FirstReviewTime:            median(m.FirstReviewSeconds, m.FirstReviewSample, createdReviews),
		PullRequestsNeverReviewed:  count(m.PullRequestsNeverReviewed, createdReviews),
		MergeTime:                  median(m.MergeSeconds, m.PullRequestsMerged, windowPulls),
		ActiveContributors:         count(m.ActiveContributors, participants),
		ActiveBotContributors:      count(m.ActiveBotContributors, participants),
		ContributorsCoverage:       participants,
		TimelineCoverage: TimelineCoverage{
			PullRequestMerged: windowPulls, ReleasePublished: releases,
			IssueOpened: windowIssues, IssueEvents: eventTimeline,
		},
	}
	if events.Covered && c.EventsCoveredFrom.Valid {
		out.EventsCoveredFrom = &c.EventsCoveredFrom.Time
	}
	return out
}

func uncovered(reason Reason) Coverage { return Coverage{Reason: &reason} }

func streamCoverage(watermark, boundary pgtype.Timestamptz, start time.Time) Coverage {
	if !watermark.Valid {
		return uncovered(ReasonNotSynchronized)
	}
	if !start.IsZero() && boundary.Time.After(start) {
		return uncovered(ReasonWindowBeforeBoundary)
	}
	return Coverage{Covered: true, Through: &watermark.Time}
}

// The first missing stream takes precedence over an insufficient window.
func combineCoverage(a, b Coverage) Coverage {
	if !b.Covered && (a.Covered || *b.Reason == ReasonNotSynchronized) {
		return b
	}
	if !a.Covered {
		return a
	}
	if b.Through.Before(*a.Through) {
		return b
	}
	return a
}

func childCoverage(parent Coverage, missing int64, through pgtype.Timestamptz) Coverage {
	if !parent.Covered {
		return parent
	}
	if missing != 0 {
		return uncovered(ReasonChildrenIncomplete)
	}
	if through.Valid && through.Time.Before(*parent.Through) {
		parent.Through = &through.Time
	}
	return parent
}

func count(value int64, c Coverage) Count {
	out := Count{Coverage: c}
	if c.Covered {
		out.Value = &value
	}
	return out
}

func median(seconds, size int64, c Coverage) Median {
	out := Median{Coverage: c}
	if c.Covered {
		out.SampleSize = &size
		if size > 0 {
			out.Seconds = &seconds
		}
	}
	return out
}

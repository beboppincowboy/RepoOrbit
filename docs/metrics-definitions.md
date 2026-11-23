# Metric definitions

These definitions apply to `repositoryOverview` (one repository, U5b) and to the cross-repository queries in U5d, which apply them separately to each repository. Every value is computed with indexed SQL over the activity projections (`backend/migrations/00011`–`00014`). No schema migration is needed. The values are activity indicators. There is no productivity score and no ranking of people, and no generated narrative.

## 1. Clock and windows

- `now` is a single instant taken once per request from the injected clock (the `Clock` already used by ingestion). Tests pass a fixed instant. Every window, threshold and coverage check in one response uses that same instant.
- `today = date_trunc('day', now AT TIME ZONE 'UTC')`, the start of the current UTC day.
- A window of N days, where N ∈ {7, 30, 90}, is `[windowStart, now]`, with `windowStart = today − (N−1) days`. It covers N UTC calendar days and today counts as a partial day. No other window lengths are accepted.
- An event belongs to the window when `windowStart <= ts AND ts <= now`.
- Why not `now − N days`: ingestion fixes `boundary = scheduledAt − 90 days` (`RequestActivity`, schedule.go). Because `today − 89 days > now − 90 days >= boundary` whenever `now >= scheduledAt`, a 90-day window always fits inside imported history, even on the day of scheduling. A plain `now − 90 days` start would fall before `boundary` on day one.
- Stale threshold (fixed, not configurable): an open item is stale when `updated_at < today − 29 days`. That means no update inside the 30-day window.

## 2. Coverage

A metric is always returned together with its coverage, `{ covered, reason, through }`:

- `covered`: whether the evidence is sufficient for the metric and window.
- `reason`: null when covered. Otherwise one code from the closed set below.
- `through`: the RFC 3339 instant the evidence is complete up to. It is the minimum of the watermarks of every input. Data after `through` may be incomplete, and the UI shows it as synchronization age. `through` is always in the past, so coverage never requires it to equal `now`.

### Reason codes (closed set; checked in this order, first match wins)

| Code | Meaning |
|---|---|
| `not_synchronized` | An input stream has no `activity_resources` row, or its `watermark IS NULL`: it has not finished a single converged scan. This covers unselected repositories, streams still in their first backfill, and streams reset by private-evidence cleanup (cleanup.go sets `watermark = NULL`). |
| `window_before_boundary` | `windowStart < boundary` for a windowed input. With the window rule above this should not happen, but it is checked anyway. |
| `historical_gap` | The metric depends on issue events, and the window starts before the issue-event coverage start (section 2.3). |
| `children_incomplete` | At least one qualifying parent has a review or comment history that is missing or has never converged. |

Access is not one of these codes. Access is decided for the whole response (section 6).

### 2.1 Stream coverage (`activity_resources`)

Applies to the streams `issues`, `pull_requests`, `releases` and `issue_events`:

```
covered(stream, windowed) :=
  row exists AND watermark IS NOT NULL
  AND (NOT windowed OR boundary <= windowStart)
through(stream) := watermark
```

- `phase` is deliberately not used. `SignalActivity` (schedule.go) moves every caught-up stream from `current` back to `catchup` on each webhook, poll (every 15 min) and Refresh, and it keeps `watermark`. Requiring `phase = 'current'` would turn every metric unknown on every poll. The `watermark` is the durable fact that a full scan converged.
- `failureCode`, `jobState` and freshness keep being reported by the existing `activity` status (`synchronized`, `failureCode`). A later failure does not null values that are already covered. The UI shows the failure next to `through`.
- Snapshot metrics (current open items) need `watermark IS NOT NULL` but no boundary check. Ingestion keeps every open issue and open PR whatever its age (sync.go and evidence_pull_requests.go skip only closed items whose timestamps are all before `boundary`).
- Releases are imported without a boundary filter, so the boundary check always passes for them.

### 2.2 Child coverage (`activity_children`)

Applies to reviews and comments, and only for a metric's qualifying parents:

```
covered(children, parents) := for every qualifying parent p:
  a row exists in activity_children with repository_id = p.repository_id, resource = R, parent_id = key(p),
  AND watermark IS NOT NULL
through(children) := min(watermark) over those rows
```

| R | `key(p)` |
|---|---|
| `reviews` | `activity_pull_requests.source_id` |
| `comments` | `activity_issues.number`, or `activity_pull_requests.number` (issues and PRs share one number space, so a number belongs to exactly one of them) |

- The repository-wide `ChildResourceStatus.complete` is not used. `registerChild` sets `complete = false` each time a parent page is scanned again, which happens on every catch-up, but it leaves `watermark` alone. `watermark` is set only when that parent's history scan converges.
- When there are no qualifying parents, child coverage holds trivially. The metric still requires its parent stream to be covered.

### 2.3 Issue-event coverage start

GitHub keeps issue events for a limited time and never emits an "opened" event (`historicalGap` is always true). The traversal applies no `boundary`. No column records when event history became complete, so the start is derived:

```
eventsCoveredFrom := min(observed_at) FROM activity_issue_events WHERE repository_id = $repo
```

`observed_at` is the start of the scan that first stored a row, and later conflicting inserts never change it (`ON CONFLICT DO NOTHING`). Every later catch-up scan traverses the whole retained history again, so any event created after the first scan began is observed, provided retention is longer than the poll interval. The rule holds because the traversal is the only writer of `activity_issue_events` (no webhook insert), and a coverage reset (cleanup.go) deletes the repository's events together with its watermark. A signed issue deletion removes that issue's events, which can only move the start later, the safe direction. An event-derived metric is covered only when the `issue_events` stream is covered (2.1) and `eventsCoveredFrom IS NOT NULL AND eventsCoveredFrom <= windowStart`. Otherwise it is unknown with `historical_gap`. A repository with no stored events therefore has no event coverage. This is an inference, recorded as an open question in section 8.

### 2.4 Metric coverage

A metric's coverage is the weakest of its inputs: `covered` is the AND of its inputs, `reason` is the first failing code in the order above, and `through` is the minimum of the input watermarks.

## 3. Known zero vs unknown

- **Count**: `0` only when the metric is covered and no qualifying rows exist. When it is not covered, `value = null` and `reason` is set. A count never reflects partial rows.
- **Median** (`{ seconds, sampleSize }`):
  - not covered: `seconds = null`, `sampleSize = null`, and `reason` is set.
  - covered but no qualifying records: `seconds = null`, `sampleSize = 0`.
  - otherwise: `seconds = floor(percentile_cont(0.5) WITHIN GROUP (ORDER BY duration_seconds))` over the qualifying records. With an even sample this is the mean of the two middle values. A median is never computed from daily medians.
- Lists (contributors, timeline) carry the coverage of their inputs. When not covered they return no entries, so a list with entries missing is never shown as complete.

## 4. Shared conventions

| Concern | Rule |
|---|---|
| Bot | `author_type = 'Bot'` (`actor_type` for events). Any other non-null type (`User`, `Organization`, `Mannequin`) counts as human. |
| Null actor | A null `author_id` is a deleted or ghost account (allowed since migration 00012, also on `activity_issues`). It is excluded from contributor counts and lists. A null-author comment or review is never a first response or first review: it cannot be shown to be non-bot or someone other than the author. A null-author issue or PR still counts as opened, merged, open or stale. |
| Qualifying review | `submitted_at IS NOT NULL AND upper(state) <> 'PENDING'`. Ingestion already drops pending reviews; this repeats the filter used by `reviewEvidence`. |
| Real issues only | `activity_issues` never holds PR-shaped issues (sync.go skips `issue.PullRequest != nil`). `activity_comments` has no `issue_id`, so join on `(repository_id, issue_number) = (repository_id, activity_issues.number)`. Because the number space is shared, this join excludes PR conversation comments. `activity_issue_events` does include PR events (its insert has no PR filter), so join `activity_issue_events.issue_id = activity_issues.source_id`. |
| Tombstones | A signed deletion removes the row, and `activity_tombstones` prevents it from being imported again (issues, comments, releases). Metrics read only the live rows. A deleted item is absent, not counted. |
| Drafts | Draft releases are never imported. The schema has no PR draft flag, so draft PRs count as open PRs. |
| Archived | `repositories.archived` is shown beside the metrics. It does not change any definition. |
| Repository scope | Every query has `WHERE repository_id = $repo` and uses the existing `(repository_id, source_id)` indexes. |

## 5. First-release metrics

Notation: `W` means in the window (section 1). `S(x)` is stream coverage, with `ᵂ` marking a windowed check. `C(R, parents)` is child coverage. Comments are one repository-wide stream holding comments updated since its boundary, so comment coverage is `S(comments)`. Qualifying first comment for issue `i`: a comment `c` with `c.repository_id = i.repository_id AND c.issue_number = i.number AND c.author_id IS NOT NULL AND c.author_type <> 'Bot' AND c.author_id IS DISTINCT FROM i.author_id`. Qualifying review for PR `p`: a qualifying review `r` with `r.pull_request_id = p.source_id AND r.author_id IS NOT NULL AND r.author_type <> 'Bot' AND r.author_id IS DISTINCT FROM p.author_id`.

### 5.1 Snapshots (current state, not windowed)

| GraphQL name | Definition (source) | Bots | Coverage |
|---|---|---|---|
| `openIssues` | `count(*)` from `activity_issues` where `state = 'open'` | included | `S(issues)` |
| `openPullRequests` | `count(*)` from `activity_pull_requests` where `state = 'open'` | included | `S(pull_requests)` |
| `openIssuesAssigned` | open issues with at least one row in `activity_issue_assignees` | included | `S(issues)` |
| `staleOpenIssues` | open issues with `updated_at < today − 29d` | included | `S(issues)` |
| `staleOpenPullRequests` | open PRs with `updated_at < today − 29d` | included | `S(pull_requests)` |
| `unreviewedOpenPullRequests` | open PRs with no qualifying review | bot reviews do not count | `S(pull_requests)` + `C(reviews, open PRs)` |
| `unansweredOpenIssues` | open issues with no qualifying first comment | bot comments do not count | `S(issues)` + `S(comments)`, and no open issue created before the comments boundary (its older replies were never fetched; otherwise `windowBeforeBoundary`) |

`updated_at` is GitHub's own value and changes on any edit, including bot activity, so stale means "GitHub records no update of any kind".

### 5.2 Windowed counts (an event inside `W`)

| GraphQL name | Qualifying rows | Bots | Coverage |
|---|---|---|---|
| `issuesOpened` | `activity_issues.created_at ∈ W` | included | `S(issues)ᵂ` |
| `issuesClosed` | `activity_issues.state = 'closed' AND closed_at ∈ W` | included | `S(issues)ᵂ` |
| `issuesReopened` | `activity_issue_events.event = 'reopened' AND created_at ∈ W`, joined to `activity_issues` on `issue_id = source_id` | included | `S(issues)ᵂ` + `S(issue_events)` + event start ≤ `windowStart` (2.3) |
| `pullRequestsOpened` | `activity_pull_requests.created_at ∈ W` | included | `S(pull_requests)ᵂ` |
| `pullRequestsMerged` | `merged_at ∈ W` | included | `S(pull_requests)ᵂ` |
| `releasesPublished` | `activity_releases.published_at ∈ W` (prereleases included; drafts never stored) | included | `S(releases)ᵂ` |

`issuesClosed` comes from the issue's current `closed_at`, which is the latest closure only. An issue closed, reopened and closed again within `W` counts once, and an issue that is open again now does not count. The closure events themselves appear in the timeline only within event coverage. Nothing infers a closure or reopen that was not observed.

### 5.3 Responsiveness medians

| GraphQL name | Qualifying records → duration | Companion count | Coverage |
|---|---|---|---|
| `firstResponseTime` | issues `i` with `i.created_at ∈ W` and a qualifying first comment → `min(c.created_at) − i.created_at` | `issuesWithoutResponse`: issues created in `W` with no qualifying comment | `S(issues)ᵂ` + `S(comments)ᵂ` |
| `firstReviewTime` | PRs `p` with `p.created_at ∈ W` and a qualifying review → `min(r.submitted_at) − p.created_at` | `pullRequestsNeverReviewed`: PRs created in `W` with no qualifying review, whether open, closed or merged | `S(pull_requests)ᵂ` + `C(reviews, PRs created in W)` |
| `mergeTime` | PRs with `merged_at ∈ W` → `merged_at − created_at` | none (`sampleSize` = `pullRequestsMerged`) | `S(pull_requests)ᵂ` |

- Closing an issue without a comment is not a response.
- Items with no response or review yet are not in the median (it would be right-censored), so the companion count is always shown next to it.
- `mergeTime` includes bot-authored PRs: it measures PRs, not people.

### 5.4 Contributor participation

Active humans in `W` are the distinct non-null `author_id` values, with `author_type <> 'Bot'`, from two sources:
(a) the authors of PRs with `created_at ∈ W`;
(b) the authors of qualifying reviews with `submitted_at ∈ W`, on any PR in the repository.

Issue authors and commenters are not counted (KTD6). Commenting alone does not put someone in `contributors`; comments are counted only for the participants already listed.

| GraphQL name | Definition | Coverage |
|---|---|---|
| `activeContributors` | count of active humans | `S(pull_requests)ᵂ` + `C(reviews, PRs with updated_at >= windowStart)` (a submitted review moves the PR's `updated_at`, so these are the only PRs that can hold a review in `W`) |
| `activeBotContributors` | the same count with `author_type = 'Bot'` | same |
| `contributors` | the active participants (humans and bots), up to 20 humans followed by up to 5 bots: `{ login, isBot, pullRequestsOpened, reviewsSubmitted, commentsCreated }` (all in `W`). `commentsCreated` counts their `activity_comments` with `created_at ∈ W` on any issue or PR of the repository. Within each group, ordered by `pullRequestsOpened + reviewsSubmitted + commentsCreated DESC, login ASC`. `login` comes from that `author_id`'s most recent PR or review row (greatest `created_at` / `submitted_at`). Rows with a null `author_type` are left out, as in the counts. | same + `S(issues)ᵂ` + `S(comments)ᵂ` |

The order shows participation for scanning the list. It is not a ranking: no score is computed or shown, and the UI must not label positions.

### 5.5 Activity timeline

`timeline` holds at most 50 entries, ordered by `occurredAt DESC, kind ASC, sourceId DESC`. Each entry has `kind`, `occurredAt`, `actor` (`login`, `isBot`, or null for a ghost account), `title` or `label`, `number` and `url`.

| `kind` | Source | `occurredAt` | `url` | Included when |
|---|---|---|---|---|
| `pull_request_merged` | `activity_pull_requests`, `merged_at ∈ W` | `merged_at` | `html_url` | `S(pull_requests)ᵂ` |
| `release_published` | `activity_releases`, `published_at ∈ W` | `published_at` | `html_url` | `S(releases)ᵂ` |
| `issue_opened` | `activity_issues`, `created_at ∈ W` (a recorded timestamp, not an event) | `created_at` | `html_url` | `S(issues)ᵂ` |
| `issue_closed`, `issue_reopened`, `issue_assigned`, `issue_unassigned`, `issue_labeled`, `issue_unlabeled` | `activity_issue_events` joined to `activity_issues`; `created_at ∈ W` and `>= eventsCoveredFrom` | `created_at` | `url` | `S(issues)ᵂ` + `S(issue_events)` with `eventsCoveredFrom` set |

- Each `kind` group is included or left out according to its own coverage, and `timeline.coverage` lists coverage per group.
- The response also returns `eventsCoveredFrom`. The UI draws a gap marker there whenever it is later than `windowStart`.
- Issue events from before `eventsCoveredFrom` are never shown.
- No entry is ever synthesized from current state (for example, a closed issue with no stored `closed` event gets no `issue_closed` entry).

### 5.6 Daily series (trend chart)

`daily` has one row per UTC day in `W` (N rows, today partial): `{ day, issuesOpened, issuesClosed, pullRequestsOpened, pullRequestsMerged }`, bucketed on `date_trunc('day', ts AT TIME ZONE 'UTC')` with a `generate_series` fill so that days with no activity are zeros. Each column uses the same rows and coverage as its count in 5.2. A column that is not covered is null for every day. There is no daily median.

### 5.7 Pull request review state (`pullRequestEvidence.reviewState`)

| Value | Rule |
|---|---|
| `merged` | `merged_at IS NOT NULL` |
| `closed` | `state <> 'open'` and not merged |
| null | open, and the PR's `activity_children` `reviews` row is missing or has `watermark IS NULL` |
| `changes_requested` | open, and some reviewer's latest `APPROVED` or `CHANGES_REQUESTED` review is `CHANGES_REQUESTED` |
| `approved` | open, and otherwise some reviewer's latest such review is `APPROVED` |
| `commented` | open, with at least one qualifying review but none of the two decisive states |
| `awaiting_review` | open, with no qualifying review (exactly the PRs counted by `unreviewedOpenPullRequests`) |

Only qualifying reviews (section 5, non-null human author other than the PR author) count. `DISMISSED` and `COMMENTED` reviews never decide. The schema has no PR draft flag, so draft PRs can appear as `awaiting_review`.

### 5.8 Cross-repository overview (U5d)

`crossRepositoryOverview` runs the same coverage, count and daily queries as `repositoryOverview` (`loadMetrics`) for each authorized selected repository, in one REPEATABLE READ snapshot.

- **Counted vs syncing.** A repository is counted when its issue and PR streams have converged for the window (`issuesOpened` and `pullRequestsMerged` covered, which implies `openIssues`). Otherwise it is syncing and contributes only to Assigned to me if its issues are covered. Review and comment histories do not decide this: `registerChild` gives every new issue or PR a child row with a NULL watermark until that parent's own scan converges, so gating on them would move a healthy repository to "syncing" whenever something new arrives.
- **Totals.** `openIssues` = Σ `openIssues` over counted repositories. `reviewBacklog` = Σ `unreviewedOpenPullRequests` (open PRs with no qualifying review; drafts cannot be distinguished). It is null with `children_incomplete` while any counted repository's own value is unknown, which is what that repository's workspace shows at the same moment. Each total is covered only when at least one repository is counted, with `through` = the minimum over counted repositories. Otherwise the value is null with `not_synchronized`.
- **Daily.** `issuesOpened` and `pullRequestsMerged` summed per UTC day over counted repositories; null when none is counted.
- **Needs attention** (up to 25 rows, ordered by `since ASC, kind, repository, number`; one row per item). PR rows come from counted repositories whose `unreviewedOpenPullRequests` is covered, and issue rows from those whose `unansweredOpenIssues` is covered. A repository whose histories are still importing contributes no rows of that kind instead of incomplete ones.
  - `pr_awaiting_review`: an open PR with no qualifying review; `since = created_at`.
  - `issue_no_response`: an open issue with no qualifying first comment; `since = created_at`.
  - `stale_issue`: an answered open issue with `updated_at < today − 29d`; `since = updated_at`.

  An issue that is both unanswered and stale is listed once, as `issue_no_response`. The predicates are copied from `GetMetricCounts`, so a repository's uncapped `pr_awaiting_review` and `issue_no_response` rows equal its `unreviewedOpenPullRequests` and `unansweredOpenIssues`.
- **Assigned to me.** Up to 25 open issues with an `activity_issue_assignees.source_id` equal to the viewer's GitHub user ID, from authorized repositories whose `issues` stream is covered (counted or syncing), ordered by `updated_at DESC`.
- **Access.** As in section 6, item 5: only repositories that pass the check are read. `withheld` counts unverified access decisions without naming those repositories. Definitive denials are excluded.
- **Bounds.** At most 100 selected repositories (otherwise the API returns `OVERVIEW_SELECTION_LIMIT`), 3 queries per repository plus 2 list queries, under the request deadline.

### Not supported in the first release

- Historical backlog, meaning open counts on past days. Rebuilding them from present state is a non-goal (KTD6), because `state`, `closed_at` and assignees hold only the latest value and issue events have a permanent gap.
- Closure and reopen counts drawn from anything other than observed rows: no counting of closures that were not observed.
- Per-person scores, leaderboards, and "most productive" views.

## 6. Access

1. `repositoryOverview` follows the existing evidence read order (graphql/evidence.go): `accessfence.TakeSnapshotForUser`, then `RepoAccess.Check` for the repository with private reads re-verified, then all the SQL above, then `fence.Current`.
2. An unverified check withholds the repository and records it in the withheld count. A definitive denial excludes the repository.
3. If the fence moved during the read, every value is discarded. There is no partial overview.
4. Every query is scoped to the one checked `repository_id`. No join, contributor list or timeline reads rows of another repository.
5. U5d runs the same check per repository and includes only the repositories that passed in any total. When a repository is withheld, its metrics are left out of the totals, not counted as zero.

## 7. Bounds (KTD7)

- One request reads one repository and one window.
- The contributor list is capped at 20 humans plus 5 bots and the timeline at 50 (each fetched with `LIMIT cap`).
- Medians and counts are single aggregate queries over indexed `repository_id` scans.
- Every query runs under the request's deadline. If the deadline passes, the request fails; it does not return partial values.

## 8. Decisions and known limits

- **Coverage gate.** Metrics gate on `watermark`, not `phase = 'current'` (2.1, 2.2). The `activity` status in `api.md` and `status.go` answers a different question, whether the stream is fresh right now, so it keeps using `phase` and `complete`. A metric answers how far back history is covered, which a reconcile signal does not undo.
- **Assignments.** `openIssuesAssigned` is a current-state snapshot (KTD6 "current … assignments"). Assignments as windowed events appear only in the timeline, within event coverage.
- **Issue-event coverage start.** `eventsCoveredFrom` is derived from `min(observed_at)`. Storing it explicitly, for example as a first-convergence timestamp on `activity_resources`, needs a migration, which is out of scope for U5b. Until then, a repository with no stored issue events reports `historical_gap` for `issuesReopened`. This errs toward reporting unknown.

## 9. Local performance baseline

On an Apple M2 Pro with PostgreSQL 17.5 in Docker Desktop, the 90-day analytics service measured **35.0 ms P95** over 100 serial warm reads after five warm-ups. The requested repository held about 2,000 issues, 2,000 PRs, 6,000 comments and 4,000 reviews. A second repository held four times that volume. The measurement includes the read transaction and SQL round trips and excludes GraphQL and GitHub authorization. The fixture is synthetic and the measurement describes one client.

| Query | PostgreSQL execution | Plan |
|---|---:|---|
| Coverage | 2.502 ms | Repository bitmap index scans on parents and child coverage |
| Counts and medians | 11.603 ms | Repository bitmap index scans, hash joins for first comments and reviews, ordered-set aggregates |
| Contributors | 5.026 ms | Repository bitmap index scans, participant aggregation, and top-N sort capped at 20 |
| Timeline | 1.351 ms | Repository bitmap index scans, event join, and top-N sort capped at 50 |
| Daily series | 2.086 ms | Repository bitmap index scans, per-day aggregates and joins to the generated UTC days |

Run the baseline from `backend/` with the integration database available. It logs each full `EXPLAIN (ANALYZE, BUFFERS)` plan.

```sh
DATABASE_URL=postgres://repoorbit:repoorbit@localhost:54329/repoorbit?sslmode=disable \
REPOORBIT_ANALYTICS_BASELINE=1 go test -tags=integration ./internal/analytics \
  -run '^TestRepositoryOverviewBaseline$' -v -count=1
```

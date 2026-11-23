# API

## GraphQL: repository status

`Repository` reports two independent facts. Do not infer one from the other.

- `metadataStatus` (null when the viewer has not selected the repository, and in Demo mode): the shared repository metadata refresh.
  - `state`: `not-scheduled`, `queued`, `running`, `paused`, or `dead`.
  - `lastSuccessAt`: RFC 3339 time of the last completed refresh, or null.
  - `nextAttemptAt`: RFC 3339 time a queued refresh is due (a retry or the next poll); null unless `queued`.
  - `paused`: waiting for a usable credential or access. No remote calls are made and no retry attempts are spent while paused.
  - `failureCode`: a short machine code (lowercase letters, digits, underscores) for the last failed attempt, or null. It is never an error message; anything else is reported as `unknown`.
  - `followSuspended`: the viewer unstarred a followed repository. The selection is kept, but is not used for synchronization or credentials until the star returns.
- `activity` (always present) reports issues separately from metadata. `issues` is null before scheduling; otherwise it gives the fixed `boundary`, `phase` (`backfill`, `catchup`, or `current`), `catchupThrough`, `lastSuccessAt`, `failureCode`, `jobState`, and `nextAttemptAt`. A failure or incomplete phase means issue coverage is unknown, even when some issues are readable. `futureResources` names activity streams not yet imported. `synchronized` stays false while those streams are missing. Demo content is labeled and is not live GitHub data.

The `issues` query returns imported issues and existing sample data only after viewer access is checked. An optional `number` narrows it to one issue number. Private reads require a current user-and-installation intersection; each activity read performs a fresh check. A failed access check withholds content. Stored private evidence is removed only after two matching complete app installation inventories show no remaining installation, with a preserved boundary and reset coverage for a later rebuild. GitHub can change between checks; the fence covers observed invalidations, while periodic inventory repairs missed signals.

Status is shared across selecting users and exposes no credential identities. `dead` means an operator must requeue the work (see operations).

`pullRequestEvidence`, `reviewEvidence`, and `commentEvidence` accept the same bounded `repositoryIds`, `first` (1–100), and `after` source-ID cursor as `issues`. They return stored source IDs, timestamps, links and actor ID/login/type; PRs include merge and close times, reviews include their PR ID and submission state, and comments include their issue or PR number. The `author` is null when GitHub supplies no actor (for example, a deleted account); actor details are never fabricated. Only submitted reviews are shared. Each PR carries a `reviewState`: `merged`, `closed`, or for an open PR `awaiting_review`, `commented`, `approved` or `changes_requested` (metric definitions 5.7); it is null while that open PR's review history has not been imported. These queries require a selected repository, a fresh viewer access check and an unchanged access fence; unverified repositories are counted in `withheld`, with `retryAt` when available. Pass `newestFirst: true` to `pullRequestEvidence` to page in descending source-ID order. Use the same order when passing its cursor to the next page. In Demo mode, the PR query returns sample evidence for demo repository IDs. Partial rows remain readable while import is incomplete.

`activity.pullRequests` has the same independent traversal fields as `issues`. `activity.reviews` and `activity.comments` report `complete`, `pendingChildren`, `lastSuccessAt`, `failureCode`, and `nextAttemptAt`. Reviews require completed PR traversal and all discovered review histories. Comments are one repository-wide stream of comments updated since its boundary: `complete` means that stream has caught up, `pendingChildren` is always 0, and failure and retry come from the stream. Consequently, an empty query with `complete: true` is different from an incomplete import. Last success, a later failure and the next retry are separate fields; neither a prior success nor readable rows establishes current freshness. `activity.releases` has the same traversal fields; drafts are never imported. `activity.issueEvents` reports `traversal` plus `historicalGap`, which is always true: GitHub retains issue events for a limited window and emits no opening event, so earlier history is a permanent gap and current state is never turned into synthetic history. Only closed, reopened, assigned, unassigned, labeled and unlabeled events are stored, with source id, actor, link, source time and observation time. `releaseEvidence` and `issueEventEvidence` are paged, access-checked reads like the other evidence queries. 

## GraphQL repository overview

`repositoryOverview(id: ID!, days: Int! = 30)` reads a single selected repository over a 7-, 30-, or 90-day UTC window. It returns the workspace name and sync status, coverage-bearing counts and medians, a daily series, and bounded contributor and activity lists. Contributors are PR authors and reviewers in the window, up to 20 humans followed by up to 5 bots (`isBot`), each with `pullRequestsOpened`, `reviewsSubmitted` and `commentsCreated`. Metric coverage is based on converged evidence and includes a reason when the value is unknown. See [metric definitions and the local SQL baseline](metrics-definitions.md).

In real mode the resolver takes the viewer's access fence, re-verifies access, and checks the fence again before returning workspace data. A denied, missing, or unselected repository returns null. If access cannot be verified or changes during the read, the response is null with `ACCESS_UNVERIFIED`. In Demo mode, only demo repository IDs resolve, and their metrics come from the sample evidence seeded by `testdata/demo.sql` (timestamps relative to the Demo start).

## GraphQL cross-repository overview

`crossRepositoryOverview(days: Int! = 30)` summarizes every repository the viewer has selected (at most 100) over the same 7-, 30- or 90-day UTC windows. It applies `repositoryOverview`'s per-repository metric definitions, so its totals equal the sum of the workspace figures for the same window. A repository is counted (`repositoriesCounted`) once its issue and PR streams have converged for the window. Otherwise it is still syncing (`repositoriesSyncing`) and is left out of totals, the trend and Needs attention; it is never counted as zero. A counted repository whose review or comment histories are still importing stays counted. While that lasts, `reviewBacklog` is unknown (`children_incomplete`) and that repository contributes no needs-attention rows of the affected kind. `withheld` counts selected repositories whose access could not be verified, without naming them (`retryAt` when rate limited).

It returns:

- `openIssues` and `reviewBacklog` (the sum of `unreviewedOpenPullRequests`): coverage-bearing counts.
- `needsAttention`: up to 25 rows, oldest first, of kind `pr_awaiting_review`, `issue_no_response` or `stale_issue`.
- `assignedToMe`: up to 25 open issues assigned to the signed-in GitHub user, matched by GitHub user ID, from authorized repositories whose issue stream is covered even if their PR stream is still importing.
- `daily`: issues opened and PRs merged, summed over the counted repositories (null when none is counted).

More than 100 selections returns `OVERVIEW_SELECTION_LIMIT`. Access follows `repositoryOverview`: fence, live check, fence again. A fence move returns null with `ACCESS_UNVERIFIED`, and the read that first discovers a revocation fails this way once. An operation may request at most one of `repositoryOverview` and `crossRepositoryOverview`, and the complexity budget is 1200. Demo mode summarizes the demo repositories, and issues are assigned to the Demo viewer.

## REST: issue writes

Issue writes are reviewed commands: the client shows the draft, the user confirms it, and only then the client submits it. All routes sit behind the session middleware. State-changing requests need a same-origin `Origin` and the `X-CSRF-Token`; Demo viewers are refused. Every command and every operation read rechecks the viewer's live GitHub access first. Commands and operation reads (including recheck) always check against GitHub fresh; capabilities may reuse the fenced 30-second listing. An operation read whose access changed while it loaded answers `503` and reveals nothing; retry it. A denied repository, a repository with no local row, and another user's operation all answer `404`. Access that cannot be verified answers `503` (with `Retry-After` when rate limited). Writes use only the actor's own GitHub user token, never an installation token.

A repository `{id}` is the repository's GitHub ID; an operation `{id}` is the operation's `id`. "Metadata" means labels and assignees. An operation is *unresolved* while its `state` is `pending` or `unknown`.

| Method and path | Purpose | Success |
| --- | --- | --- |
| `GET /api/repositories/{id}/assignees` | `{assignees: [login]}`: the users GitHub accepts as assignees (first 100), read with the viewer's token; empty when the viewer cannot assign. | `200` |
| `GET /api/repositories/{id}/issue-capabilities` | `{canCreateIssue, canCreateIssueWithMetadata, canManageIssues}` for the viewer, from current GitHub permissions. `canManageIssues` covers label and assignee updates and close/reopen. | `200` |
| `POST /api/repositories/{id}/issues` | Create an issue: `{title, body?, labels?, assignees?}`. | `201`, or `202` when `unknown`; `200` for a replayed key or a create failed before sending (see Idempotency) |
| `POST /api/repositories/{id}/issues/{number}/updates` | Update an issue: `{labels?: {add?, remove?}, assignees?: {add?, remove?}, state?: "open" \| "closed"}`. | `201`, or `202` when `unknown`; `200` for a replayed key |
| `GET /api/repositories/{id}/write-operations` | `{operations: [operation]}`: the viewer's own operations on that repository, newest first, at most 100, no paging. | `200` |
| `GET /api/write-operations/{id}` | One of the viewer's operations. | `200` |
| `POST /api/write-operations/{id}/recheck` | Resolve an `unknown` operation without resending: `{"issueNumber": n}` for a create, `{}` for an update. | `200`; `409` while it stays `unknown` |

Error statuses: `400` invalid input or `Idempotency-Key`; `401` the viewer's GitHub token is gone (sign in again); `403` the viewer's GitHub permission does not allow the action (nothing recorded); `404` a demo viewer, a denied or unstored repository, another user's or a missing operation, or an update target that is missing or a pull request; `409` a duplicate create (see Idempotency), or a recheck that could not resolve; `422` a reused key with a different request, or an assignee who cannot be assigned; `503` access unverifiable or changed mid-request, or GitHub unreachable before anything was sent (`Retry-After` when rate limited); `500` a database error. Without a session the middleware answers `401` first. Error bodies are plain text with no machine-readable code, except the duplicate-create `409`, which is `{"error": string, "operationId": n}`, and a recheck's `409`, which is the unchanged operation.

**Permissions** follow GitHub's per-action rules. Anyone who can read the repository may create an issue with only a title and body. Labels or assignees on a new issue need push access, because GitHub silently drops them otherwise. Later label or assignee changes, and close or reopen, need triage or a higher role, except that an issue's author may close or reopen their own issue with read access alone (an update that also touches labels or assignees still needs triage). Archived repositories and repositories with issues turned off accept no writes. A command without the needed permission answers `403` and records nothing. Capabilities reflect the user's role only. If GitHub still refuses a write, for example because the app is not installed on the repository, the operation is recorded as `failed`.

**Input** is validated strictly, and every violation answers `400`:

- the body is a single JSON object with no unknown fields and nothing after it, at most 1 MiB;
- `title` is 1–256 characters after trimming; `body` is at most 65,536 characters;
- each label or assignee list has at most 10 entries, with no duplicates;
- label names are 1–50 characters with no surrounding spaces;
- logins are valid GitHub logins;
- no value is both added and removed, and an update must request something.

Every assignee is checked with GitHub's assignee endpoint before anything is sent. An assignee who cannot be assigned answers `422` and nothing is recorded.

**Idempotency.** A command requires an `Idempotency-Key` header: 1–128 letters, digits or `._:-`, scoped to the user. Reusing a key with the same request returns the existing operation (`200`, whatever its state) and sends nothing. Reusing it with a different request answers `422`. A new create whose title and body match a `pending` or `unknown` create by the same user in the same repository answers `409` with `operationId`. It is refused even under a fresh key and even when both are submitted at once, and the client should recheck that operation instead. A `pending` create older than one minute was never sent: it is marked `failed` and the new create proceeds. If the blocking create resolves while the new one is being submitted, it may have been sent and succeeded, so nothing is sent: the answer is still `409`, with `operationId` naming the latest create of the same title and body (any state), or `503` if none can be found. A create that a concurrent stale-pending recovery failed before it was sent answers `200` with that `failed` operation. The request hash that decides "same request" covers the kind, repository, issue number and validated body.

**Operation** responses contain `{id, repositoryId, kind, state, issueNumber, draft, outcome, actor, searchUrl?, createdAt, updatedAt}`:

- `kind` is `create` or `update`.
- `actor` is the initiating user's GitHub login.
- `state` is the operation's state (an issue's own state is `outcome.issue.state`), one of:
  - `pending`: recorded, not yet sent.
  - `unknown`: sent, outcome not known.
  - `succeeded`
  - `failed`
- `draft` is the reviewed request as validated. When the actor loses access it is *wiped* (set to null) and, in the same step, the outcome is *redacted* (reduced to the shape below).
- `outcome` holds:
  - `issue`: `{number, url, title, state, labels, assignees}` as GitHub returned or re-read it.
  - `unapplied`: requested changes the confirmed issue does not show, each `"add label X"`, `"remove label X"`, `"add assignee X"`, `"remove assignee X"` or `"state open|closed"` (a wiped create whose metadata cannot be verified has one fixed sentence instead).
  - `error`: a summary with no credentials in it.
  - `allStepsAnswered` (updates only): every step got a definite answer (including one that failed before it was sent), so no request of the update can still land and a recheck may fail it.

  When the draft is wiped the outcome is redacted to `{issue: {number, url, state}, unappliedCount, error, redacted: true}`.
- `succeeded` and `failed` operations are kept for 30 days after `createdAt`, then deleted. Unresolved operations older than 30 days are kept with the draft wiped and the outcome redacted, so a retry still cannot duplicate an issue.
- `searchUrl` appears only on `unknown` creates. It links to a GitHub issue search for the actor's issue with that title.

A new command answers `201` once its outcome is recorded, even when that outcome is `failed` (including partly applied), or `202` when the outcome is `unknown`. If access changed while the command ran, the recorded operation is withheld with `503` (on the command, a replay of its key, or a recheck). It may have been sent: retry with the same key, which reads it under a fresh access check, never with a fresh key.

**Outcomes:**

- `succeeded` means GitHub's returned or re-read issue shows every requested change.
- A create that GitHub accepted but that is missing a requested label or assignee is `failed`. The issue is still linked (`issueNumber` and `outcome.issue`), so the follow-up is an update of that issue, never a second create.
- A create that GitHub rejects with a 4xx is `failed`.
- A create that times out, loses its connection, gets a 5xx, or gets an unreadable answer is `unknown`. It is never resent automatically.
- An update applies each add or remove separately (assignee removals before additions, so a swap fits GitHub's 10-assignee limit), stops at the first error, and then re-reads the issue. The re-read decides `succeeded` or `failed`, except that after a step with no reliable answer (timeout, lost connection, 5xx) a re-read that does not yet show every change leaves the update `unknown`: that request may still land. If the re-read fails, the update is `unknown`. A partly applied update (some `unapplied` entries) is `failed`, never `succeeded`.
- Updates never replace label or assignee sets, so values other users added concurrently survive.

**Recheck** only resolves an `unknown` operation. On any other state it returns the operation unchanged (`200`) and contacts GitHub not at all. Like every operation read it first rechecks live access: lost access answers `404`. For a create, the client supplies the number of the issue it found on GitHub. That issue is linked only when it passes every check:

- it is an issue in this repository (not a pull request);
- the actor authored it;
- its title and body match the reviewed draft (compared by hash, so this works even after the draft is wiped);
- GitHub created it no earlier than 5 seconds (clock-skew allowance) before the request was sent.

On any mismatch, or when the issue is missing or already linked to another operation, the answer is `409`, and the operation stays `unknown` with its draft. If the draft was wiped and the create asked for labels or assignees, the issue is linked but the operation is `failed` with one unverifiable unapplied change: the assignment cannot be confirmed.

An update operation is re-read and verified against its draft. It becomes `succeeded` once every requested change shows. It becomes `failed` only when `allStepsAnswered` is true (no request can still land); otherwise it stays `unknown` and recheck answers `409`. An update whose draft was wiped cannot be verified and also answers `409`.

A confirmed issue is written to the local activity projection in the same transaction as the outcome, but only while the access fence is unchanged. Otherwise the next sync imports it.

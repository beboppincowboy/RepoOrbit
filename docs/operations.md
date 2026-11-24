# Operations

## Migration rollback

Rolling back migration `00012_review_evidence.sql` fails safely if any `activity_issues` row has a null author ID, login, or type: the previous schema requires all three. Restore a compatible backup or resolve those rows before retrying the rollback. Do not delete evidence or invent an author to make the rollback succeed.

Before running the Down step of `00015_write_operations.sql`, roll back the API and worker binaries. Current binaries reference `write_operations` whenever access is revoked (`accessfence.InvalidateAccess`) and during private projection cleanup, so on the older schema every revocation would fail. Rolling back also deletes the issue-write audit history.

Migration `00016_repo_wide_comments.sql` changes the `activity_comments` job key from `repo:N:parent:M` to `repo:N`. Stop every worker before running it, in either direction, and start only the matching build afterwards. A worker from before 00016 marks the new `repo:N` comment jobs dead, and `SignalActivity` never revives dead work, so a rolling deploy needs an operator requeue afterwards. (Newer workers finish leftover `repo:N:parent:M` comment jobs as no-ops.) The Down step does not restore per-issue comment progress: older code rescans comments only on issues and pull requests that change again.

## Job queue

Background work lives in the PostgreSQL `jobs` table, one row per `(kind, key)`. Rows are written inside the same transaction as the change that needs them. A job that finishes with no new intent deletes its row, so the table only holds work that is waiting, running, parked, or dead.

States: `queued` (due at `run_at`), `running` (leased, `lease_until`), `paused` (no usable credential; parked until something resumes it), `dead` (permanent failure or five failed attempts).

Limits: two concurrent jobs, 60 s lease renewed by a heartbeat every 20 s, five minutes maximum per attempt, at most five failed attempts with exponential backoff (30 s doubling, capped at one hour, plus up to 20% jitter). Rate-limit deferrals and no-credential pauses never spend an attempt. A worker that loses its lease (crash, stall) is fenced out: its heartbeats, outcome, and `Fence.Commit` transactions are rejected, and the job is reclaimed by another worker. A job whose workers keep dying goes dead after the fifth reclaim. On shutdown the worker stops claiming, waits 30 s for in-flight jobs, then cancels and releases the rest without spending an attempt.

### Inspect

```sh
psql "$DATABASE_URL" -c "SELECT id, kind, key, state, attempts, pending, run_at, lease_until, last_error FROM jobs ORDER BY state, run_at"
psql "$DATABASE_URL" -c "SELECT id, kind, key, attempts, pending, last_error, updated_at FROM jobs WHERE state = 'dead'"
psql "$DATABASE_URL" -c "SELECT id, kind, key, last_error FROM jobs WHERE state = 'paused'"
```

`last_error` is for operators only (truncated to 200 characters). Do not copy it into shared responses or tickets without checking it for secrets.

### Requeue a dead job

A dead job is a barrier. Selection changes, polls, and ordinary webhooks keep recording `pending = true` on it but never revive it, so a failing repository is not retried in a loop. Security invalidation is unaffected.

After fixing the cause, requeue explicitly. This resets the attempt budget to five and clears `pending` and `last_error`; the handler rechecks eligibility when it next runs.

```sh
psql "$DATABASE_URL" -c "UPDATE jobs SET state = 'queued', run_at = now(), pending = false, attempts = 0, lease_until = NULL, last_error = '', updated_at = now() WHERE kind = 'repo_metadata_refresh' AND key = 'repo:123' AND state = 'dead'"
```

`UPDATE 0` means the job is not dead (it may already be running or have been deleted after finishing). The same operation is available in code as `jobs.RequeueDead(ctx, tx, kind, key, runAt)`, which returns `jobs.ErrNotDead` in that case. Do not requeue by deleting the row or by editing `generation`; the generation is what fences stale workers.

A paused job is not dead: it resumes automatically when eligibility returns (`jobs.Resume`), so leave it alone unless the cause is understood.

## Webhooks

Endpoint: `POST /webhooks/github` on the API (not the web app). Real mode requires `WEBHOOK_SECRET`; Demo mode has no endpoint and does no remote ingestion.

Setup: in the GitHub App, set Webhook active, URL `<public API origin>/webhooks/github`, and a secret (`openssl rand -hex 32`) equal to `WEBHOOK_SECRET`. Subscribe to Installation, Installation repositories, Repository, Push, Issues and Pull request. GitHub App authorization events are always delivered.

Supported events (all others that are validly signed get `200` with no work and no stored row; `ping` is one of them):

| Event | Action | Effect |
|-------|--------|--------|
| `push`, `issues`, `pull_request` | any | metadata refresh if the repository has an active selection |
| `repository` | edited, renamed, publicized, archived, unarchived | metadata refresh if selected |
| `repository` | privatized, deleted, transferred | access fence bump (repository), then refresh if selected |
| `installation`, `installation_repositories` | any | access fence bump (installation); only actions that can end access record fallback draft-wipe targets (`created`, `unsuspend` and `added` with no repositories removed do not) |
| `github_app_authorization` | any (revoked) | access fence bump (user, matched by GitHub user id), then that user's sessions are deleted and stored tokens cleared in the same transaction, so jobs find no credential and pause without spending attempts |

Responses: `200` accepted, duplicate, or unsupported; `401` bad or missing signature; `400` missing headers or an id-less payload; `413` body over 1 MiB; `5xx` not durably stored, so GitHub should redeliver (Advanced tab, Recent Deliveries). A redelivery of a failed delivery is processed once; a duplicate delivery id never schedules work twice.

Payload contents are never trusted for metadata; a refresh re-reads GitHub. Live webhook delivery is untested until the app is registered and a delivery is observed.

## Reconciliation and status

The worker calls `ingestion.StartScheduler` once; the loop runs at startup and then on fixed, jittered (+-10%) periods:

| Duty | Period | What it does |
|------|--------|--------------|
| Metadata poll | 15 minutes | Enqueues `repo_metadata_refresh` for every repository with an active, follow-eligible selection (due times spread over about one minute) and resumes paused ones. |
| Star reconcile | 1 hour | Enqueues `star_reconcile` for each user who follows starred repositories. |

The jobs worker runs the work, two jobs at a time. Periods are constants, not settings.

Star reconcile fetches the user's stars first (no write), then one claim-fenced commit stores the snapshot, updates eligibility, and resumes paused work, so a worker that lost its lease writes nothing. Only a **completed** fetch changes eligibility: a followed starred repository that is no longer starred gets `selections.follow_suspended_at` set (the Selection is kept; owned selections are never suspended), and a star that returns clears it. A failed or incomplete fetch keeps the previous snapshot and every selection as it was. Other eligible selectors keep the repository's synchronization running; with none left it leaves the poll set. The interactive `/starred` refresh stores through the same `StoreStars` step, so it updates eligibility too.

Recovery:

- Paused work (no usable credential or access) makes no remote calls and spends no attempts. `RecheckEligibility` resumes it in the transaction of sign-in (`auth.Service.OnSignIn`), reselection, a completed star store, and connecting an installation (`repositories.Service.OnEligibilityChange`), and the 15-minute poll resumes anything a missed signal left paused. A job that returns Pause while a refresh request is pending goes back to `queued` instead of parking, so the request is not lost. A resume that lands while the job is still running counts as a pending request.
- Polls and events never revive **dead** work. Only the operator requeue does (see "Job queue"), which rechecks eligibility.
- A rate-limited star refresh defers until GitHub's retry time without spending an attempt; other failures retry with backoff.

Inspecting status: the GraphQL `Repository.metadataStatus` (see `docs/api.md`). In SQL, join `jobs` (kind `repo_metadata_refresh`, key `repo:<id>`) with `repository_sync` for last success and the safe error code.

Worker wiring: construct `ingestion.SchedulerDeps{Pool, Worker, Stars, Clock}` and call `ingestion.StartScheduler(ctx, deps)` before `Worker.Run`. `Stars` is an `ingestion.StarRefresher`: `FetchStars` (adapt `repositories.Service.FetchStars`; `repositories.ErrSignedOut` maps to `ingestion.ErrNoCredential`) and `StoreStars` (`repositories.Service.StoreStars`). Set `repositories.Service.OnEligibilityChange` and `auth.Service.OnSignIn` to `ingestion.RecheckEligibility` in both binaries.

## Authorization fence, locks and throttle

- **Global permissions version.** `access_fences` row `global`/`0` is incremented by every `InvalidateAccess` and tracked by every private-commit snapshot. A private metadata commit therefore fails (and is retried a few seconds later) after *any* authorization change since its snapshot, including the removal of an installation the system never saved locally. This is deliberately conservative: an unrelated bump costs one retry. The cached-listing check is unchanged.
- **Lock order.** One order everywhere a transaction touches these rows: **job row, then fence rows (global row first, then subject rows), then repository rows, then the selection row**. A private refresh commit takes the job row (claim check), checks the permissions version, writes the repository rows, and takes the credential owner's selection lock last (`FOR SHARE NOWAIT`: a deselection or suspension in flight makes the commit defer instead of wait; the lock still precedes the commit, and a failed check rolls the writes back). The repository security webhook enqueues on the job row before it bumps the fence. A reselect locks the repository row and then the selection row, the same relative order. Writers that suspend selections and then resume repository jobs take the opposite order, which is safe because the refresh never waits for a selection lock. Tests pin the pairs: bump versus private commit, repository webhook versus refresh commit, reselect versus refresh commit, star store versus refresh commit.
- **Credential throttle.** `credential_throttle(user_id, retry_after)` is the one retry-after for a user's credential, set by a rate limit in either the metadata refresh or the star reconcile and checked by both before any GitHub call. A row whose time has passed is ignored; to clear a throttle by hand, `DELETE FROM credential_throttle WHERE user_id = <id>`.

Real-mode API and worker processes require a readable `GITHUB_APP_PRIVATE_KEY_FILE` containing the GitHub App RSA private key. In Compose, mount its directory with `GITHUB_APP_KEY_DIR` and use `/run/repoorbit-secrets/<filename>` as the file path. Provision this before upgrading; invalid or missing keys stop startup. The app inventory uses this key to evaluate private projection cleanup; errors or suspended installations leave cleanup pending for retry. Rotate keys by provisioning the replacement and restarting both processes.

### Activity reconciliation

Signed `issues`, `issue_comment`, `pull_request`, `pull_request_review`, `pull_request_review_comment` and `release` deliveries are reconciliation signals for selected repositories only: they reopen every caught-up stream (issues, pull requests, releases, issue events; reviews and comments follow their parents) for a catch-up scan from the retained boundary, and a repeated delivery id does nothing. Payload content is never imported. A background activity poll repeats the same signal for every followed repository every 15 minutes (jittered) and resumes paused streams, which repairs missed webhooks for public and private repositories alike; access is re-verified by the jobs, not the signal. Paused work resumes when authorization returns; rate-limit deferrals are never shortened by a signal or a Refresh. A dead stream stays dead (signals and Refresh only record intent; an operator requeues it) while the repository's other streams keep reconciling, and the Refresh action answers `409 dead`.

Signed `deleted` actions for issues, issue comments and releases are the one exception to "re-read GitHub": they write a tombstone (`activity_tombstones`) and remove the stored copy immediately, even if GitHub is unavailable. A database trigger refuses to insert a tombstoned id again, so stale pages cannot resurrect it. Other deletions, ambiguous 404s and partial pages delete nothing, and no scan prunes rows that GitHub no longer returns: a deletion whose signed webhook was missed stays stored until a later import overwrites nothing for it. Repository renames keep the repository id (and so its evidence); the archived flag is stored from each metadata refresh, and archived repositories stay readable and are still refreshed. Limitations: live webhook delivery and live GitHub imports have not been exercised in this repository's automated checks; pull request review comments are signals only because inline review comments are not stored.

## Issue write operations

The `write_operations` table is the audit log for user-initiated GitHub issue writes. Each row holds the actor (`user_id`), repository, idempotency key, kind, request hash, content hash, the reviewed draft, the state, and the outcome as GitHub confirmed it. It never holds tokens, and the API logs no tokens or GitHub error bodies.

- **Lifecycle.**
  - A row is inserted as `pending` after the access, permission and assignee checks pass.
  - It is set to `unknown` immediately before the request is sent.
  - After GitHub answers, it becomes `succeeded` or `failed`.
  - If the process dies or the answer is lost after the row became `unknown`, the row stays `unknown`. Nothing retries it: these writes are not jobs and never enter the job queue.
- **Unknown operations.** Never resend an unknown create by hand: GitHub may already have the issue. Inspect it with `SELECT id, user_id, repository_id, kind, issue_number, state, outcome, sent_at, created_at FROM write_operations WHERE state = 'unknown' ORDER BY id;`. The actor resolves it with the Recheck action by giving the issue number they found on GitHub. The issue is linked only after its repository, author, title and body (by content hash) are verified and its GitHub `created_at` is no earlier than 5 seconds before `sent_at` (allowance for clock skew). An unknown update resolves by recheck once GitHub shows every requested change; it is failed only when none of its requests can still land.
- **One unresolved create per content.** The partial unique index `write_operations_unresolved_create` allows at most one `pending` or `unknown` create per user, repository and content hash, so overlapping submissions under different keys cannot both reach GitHub.
- **A stuck `pending` row** means the process stopped or the database failed between the insert and the mark-sent update. Nothing was sent: the conditional mark-sent update always precedes the request. The idempotency key stays bound to that row, so the client must submit again with a new key. A new create of the same title and body fails a `pending` create older than one minute (`FailStalePendingCreate`, outcome "abandoned before sending") and proceeds; should the original process resume, its mark-sent update finds no `pending` row and it sends nothing (it answers with the `failed` row). Only that recovery lets a colliding create retry: a blocker that resolved by itself may have been sent, so the new create answers `409` naming it. A newer `pending` create still answers `409`.
- **Draft removal on access loss.** Wiping sets `draft = NULL` and redacts `outcome` (`redacted_write_outcome`): GitHub's issue number, link and state, the error summary and the count of unapplied changes stay; titles, labels and logins go. Hashes, kind, state and timestamps stay. An outcome recorded after its draft was wiped is stored redacted. Drafts are wiped:
  - user scope (`accessfence.InvalidateAccess`, in the revoking transaction): that user's drafts on private repositories;
  - repository scope (same): every draft on that repository;
  - installation scope: by the private-repository cleanup job (`activity_cleanup`) that the invalidation queues for every private repository. After two matching app inventories it wipes the drafts of actors who hold none of the installations that still cover the repository (everyone's when none does). Nothing local maps installations to repositories, so the invalidation itself cannot tell whose access ended. For an event that can end access (anything but installation `created`/`unsuspend` and `installation_repositories` `added` with nothing removed) it records the installation's local holders (`activity_cleanup.wipe_users`); whenever the job defers (no app key, an inventory error such as a suspended installation, unstable inventories, a fence race) it first wipes those holders' drafts on the repository, through the job's claim fence (a worker that lost its claim wipes nothing and leaves `wipe_users`), failing closed even for a holder another installation still covers. The shared projection still waits for a complete inventory.

  A read or command that gets a definite denial on a private repository wipes that actor's drafts there, which covers access lost without any webhook (for example, a removed collaborator).
- **Retention.** `succeeded` and `failed` operations are deleted 30 days after `created_at` (`ingestion.WriteOperationRetention`) by the worker scheduler's hourly "write operation retention" duty. Unresolved (`pending` or `unknown`) operations of that age are kept but redacted (draft wiped, outcome redacted): an unknown create's content hash, actor, repository and idempotency key are what stop a retry from creating a second issue. An unknown create goes once a recheck resolves it, and a stale `pending` create once a retry of its content fails it. An unknown change cannot be rechecked once its draft is wiped, and other `pending` rows never resolve, so those stay indefinitely in redacted form (they carry no duplicate risk).
- **Same-second projections.** The readback of a confirmed change (and of a recheck) and its projection run in one transaction holding a per-issue advisory lock (`pg_advisory_xact_lock`, 10 s lock timeout), so the later readback is projected last even when GitHub's one-second `updated_at` ties. A create's own response is projected without the lock and never replaces a same-second copy. A waiter that times out leaves its operation `unknown` for a recheck.
- **Browser draft recovery and reconciliation (U6b).** The workspace issue editor (`apps/web/src/components/issue-editor.tsx`) runs Edit → Review → Submit → Confirmed for issue creation and label/assignee/state updates, and the Issues tab pairs imported issues with Write history (`apps/web/src/components/write-history.tsx`).
  - Drafts are stored in browser `localStorage` scoped by viewer, mode, repository, and target (`repoorbit:issue-draft:<mode>:<login>:<repoId>:create` or `...:update:<number>`). Demo mode never persists drafts and disables all writes with a Demo banner.
  - A failed submission (validation/permission/unassignable 4xx or rejected write) keeps the draft and failure reason across close and page reload; editing and resubmitting generates a fresh `Idempotency-Key`. If a failed create already linked a created GitHub issue with unapplied labels or assignees, the create draft is cleared and the UI offers **Edit issue #N** so follow-up is an update rather than a duplicate create.
  - An `unknown` outcome (202 from the API, transport timeout, or mid-write access-change 503) keeps the draft and never auto-resends. When an operation record exists, the editor and Write history show its GitHub search link (`searchUrl`) and a **Recheck** form that verifies a candidate issue number (for creates) or re-reads the issue (for updates); a `409` mismatch keeps the operation `unknown` and preserves the draft. When no response reached the browser (transport error), the original `Idempotency-Key` is preserved so checking status replays the same key safely without creating a second issue.
  - Browser drafts are cleared on confirmed completion, explicit **Discard draft**, sign-out (`logout`), or when repository access is rejected.
- **Limitation.** The draft of a collaborator whose private access ended silently survives until their next read, a user revocation, the cleanup after an installation change, or a repository purge. GitHub sends no event this app handles for that case. Live GitHub write calls have not been exercised by the automated checks, which use the fake GitHub.

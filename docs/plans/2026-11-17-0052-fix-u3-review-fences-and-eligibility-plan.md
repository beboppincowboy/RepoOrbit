---
title: U3 Review Fixes - Fences and Eligibility - Plan
type: fix
date: 2026-11-17
artifact_contract: ce-unified-plan/v1
product_contract_source: ce-plan-bootstrap
execution: code
---

# U3 Review Fixes - Fences and Eligibility - Plan

**Parent plan:** `docs/plans/2026-11-16-1846-feat-repoorbit-intelligence-plan.md` (U3a to U3d). **Contracts:** `docs/plans/2026-11-16-u3-contracts.md`. **Branch:** `feat/repository-access-and-ingestion`. Findings 1 and 5 of the Codex review (fresh listings recheck the fence; interactive star refresh updates follow eligibility) are already fixed in `aae4ff7` and are out of scope here.

---

## Goal Capsule

- **Objective:** After a user's GitHub access is revoked or changed, nothing fetched under the old access is committed or shown, and a user's follow eligibility and paused work reflect their current stars, selections and credentials without waiting for a poll.
- **Means:** close each race with the smallest mechanism that serializes it in PostgreSQL: one global version row, row locks at commit time, one transaction per write (KTD1 to KTD9).
- **Authority:** the settled decisions below, then the parent plan and contracts. No new tuning knobs, no new abstractions beyond the hooks named here.
- **Stop conditions:** evidence that a settled decision cannot work stops the run and is reported; a deadlock the lock order does not remove is a blocker, not a retry loop.
- **Execution profile:** code; proof-first integration tests against PostgreSQL on port 54329.

---

## Product Contract

### Summary

Eight fixes from the two-model Codex review of the merged U3 diff. Four tighten authorization (a global permissions version, a revocation webhook that revokes credentials, a commit-time selection lock, a fenced star write). Four keep work moving (resume on reselect, sign-in and installation change; a pause that keeps pending intent; a shared credential rate limit; docs).

### Problem Frame

Review found that several commits and snapshot writes could land after the authorization or eligibility they depended on had changed: a private metadata commit missing an installation removal, a lapsed-lease worker overwriting newer stars, a refresh committing after the last follower left, and a revoked user's token staying in use. Separately, paused work waited for the 15-minute poll after the user fixed the cause, a pause erased a refresh request that arrived during the run, and two job kinds paced one GitHub credential independently.

### Requirements

**Authorization**
- R1. A private metadata commit is withheld when any authorization change committed after its snapshot was taken, including removal of an installation the system had not yet saved.
- R2. A signed `github_app_authorization` event bumps the user's fence and, in the same transaction, deletes that user's sessions and clears their stored tokens.
- R3. A refresh commit holds a share lock on the credential owner's selection row and revalidates eligibility under it; a deselection or suspension cannot commit between validation and the commit.
- R4. A star snapshot written by the reconcile job commits only through the job's claim fence, together with follow eligibility changes and resumed work.

**Keeping work moving**
- R5. Selecting, signing in, and connecting an installation resume that user's paused work in the same transaction, never reviving dead jobs.
- R6. A job that returns Pause while a request is pending is re-queued without spending an attempt instead of being parked.
- R7. The metadata refresh and the star reconcile share one durable per-credential retry-after, checked before any GitHub call and set on a rate limit.

**Docs**
- R8. `docs/operations.md` and `docs/api.md` describe the changed behavior where they currently describe the old behavior.

### Scope Boundaries

- Not changed: the cached-listing fence, the interactive `/starred` write path, the hourly and 15-minute schedulers, webhook signature, size and delivery-id handling.
- Not added: a second live access check after the metadata fetch (KTD5).

#### Deferred to Follow-Up Work

- The cached-listing snapshot (`TakeSnapshotForUser`) keeps its coarse epoch sum; moving it to the global version is a possible later simplification.
- Live GitHub acceptance and Playwright specs; requested separately.

### Success Criteria

- Each of R1 to R7 has an integration test that fails on `aae4ff7` for the stated reason and passes after the unit.
- `go test -tags=integration ./...` and `-race` on jobs, ingestion, accessfence and repositories pass.

---

## Planning Contract

### Key Technical Decisions

- KTD1. **A monotonic global permissions version** (`access_fences` scope `global`, subject `0`) is bumped by every `InvalidateAccess` and tracked by every private `TakeSnapshot`, so `StillCurrent`'s `FOR SHARE` serializes any concurrent bump with the commit. The coarse snapshot is taken before `CheckOne`, as `refresh.go` already does. (session-settled: user-approved — chosen over re-snapshotting after the access check, which misses a removal landing between check and snapshot, and over a plain epoch sum, which detects every bump but cannot be locked against rows that do not exist yet: serializing on one real row is the only atomic option.) Governs R1.
- KTD2. **Webhook revocation reuses the existing cleanup queries** (`DeleteUserSessions`, `ClearUserTokens`) inside the webhook transaction, after the bump, instead of calling `auth.Service.Revoke`, which opens its own transaction. (session-settled: user-approved — chosen over leaving tokens until the next 401 and over epoch-compare logic in the credential picker: jobs then see no credential and pause without spending attempts.) Governs R2.
- KTD3. **Commit-time eligibility takes `FOR SHARE` on the credential owner's selection row** and revalidates eligibility before writing metadata and success. (session-settled: user-approved — chosen over locking every eligible selection, which blocks unrelated users, and over accepting the race.) Governs R3.
- KTD4. **The share lock is `NOWAIT`, mapped to the existing `errRace` retry.** Lock order is job row, then selection row. The star handler's commit and the interactive refresh update selection rows and then resume repo jobs, which is the reverse order, so a refresh that waited could deadlock; `NOWAIT` removes the refresh from any wait cycle. Governs R3, R4, R5.
- KTD5. **No second live access check after the metadata fetch.** (session-settled: user-approved — chosen over an extra `CheckOne` before commit: KTD1 covers every removal GitHub reports, and a removal GitHub has not reported is indistinguishable from one a second after the commit; the extra call doubles GitHub traffic on private refreshes.) Governs R1.
- KTD6. **Split fetch from write for stars.** `StarRefresher` gets a fetch-only method and a transactional store method; the handler fetches, checks the shared throttle, then does snapshot replacement, suspend/restore and `RecheckEligibility` inside one `fence.Commit`. The interactive path calls the same store method in its own transaction. (session-settled: user-approved — chosen over passing the claim fence into `replaceStars`, which leaks job concepts into `repositories`.) Governs R4.
- KTD7. **One hook, `OnEligibilityChange(ctx, tx, userID)`**, generalizes `OnStarsReplaced` from `aae4ff7` and is called by the star store, `storeSelection` and the installation-connect handler. `auth.Service` gets `OnSignIn` with the same shape, because `ingestion` imports `auth`. Both are wired to `ingestion.RecheckEligibility` in `cmd/api` and `cmd/worker`. (session-settled: user-approved — chosen over resuming only the selected repository, and over leaving it to the poll.) Governs R5.
- KTD8. **`PauseJob` re-queues when `pending` is set**: state `queued`, `run_at` now, `pending` cleared, no attempt spent; otherwise it pauses as before. (session-settled: user-approved — chosen over parking with `pending` preserved, which is the reselect stall again.) Governs R6.
- KTD9. **The shared throttle is a table, `credential_throttle(user_id, retry_after)`**, so the API and worker processes agree and a restart keeps it. Writes are best effort outside the fence transaction. (session-settled: user-approved for sharing the throttle across both handlers; the storage choice is a planning assumption, see Assumptions.) Governs R7.

### Assumptions

- A completed-listing-only `syncInstallations` is acceptable because KTD1 no longer depends on installations being saved.
- The credential throttle row is keyed by user id because one user has one stored credential; a user whose tokens are cleared keeps a harmless stale row.
- `ListEligibleSelectors` ordering (earliest selection first) stays, so the locked row is the one whose credential the loop used.

### High-Level Technical Design

Lock order each transaction follows (directional; the units are authoritative):

| Transaction | Locks in order | Waits? |
|---|---|---|
| Refresh commit (U3) | job row (claim check) → owner's selection row `FOR SHARE NOWAIT` → global fence row `FOR SHARE` → remaining fence rows `FOR SHARE` | selection lock never waits; contention becomes `errRace` retry |
| Star job commit (U4) | star job row → selection rows (suspend/restore) → repo job rows (resume) | may wait on a refresh's job row, which holds nothing it needs |
| Interactive star write / selection / sign-in / install (U4, U5) | advisory or none → selection rows → repo job rows | same |
| Webhook (U1, U2) | `webhook_deliveries` insert → global fence row → subject fence row → user rows | serializes with commits on the global row by design |

Fence-row order is fixed everywhere: the global row first, then the subject rows. A bump and a private commit therefore always queue on the global row before either can hold a subject row, so they cannot wait on each other.

---

## Implementation Units

Unit order is execution order: security first (U1 to U3), then eligibility and pause (U4 to U6), then the throttle and docs (U7, U8).

### U1. Global permissions version

**Goal:** Any authorization bump, including for an installation never saved locally, blocks a private commit whose snapshot predates it, atomically with the commit.

**Requirements:** R1 (KTD1, KTD5)

**Dependencies:** none

**Files:**
- `backend/migrations/00009_global_fence.sql` (widen the `access_fences.scope` check to allow `global`; Down restores it)
- `backend/internal/accessfence/fence.go`
- `backend/internal/accessfence/fence_integration_test.go`
- `backend/internal/ingestion/refresh_integration_test.go`

**Approach:**
- Add `ScopeGlobal`; `InvalidateAccess` upserts and increments the `global`/`0` row first, then the subject row, in the same transaction.
- `TakeSnapshot` tracks the global key alongside user, repository and installation keys. `StillCurrent` share-locks the global row in its own first statement, then the remaining tracked rows, so its order matches `InvalidateAccess` (global first, then subject).
- `TakeSnapshotForUser` is unchanged.

**Execution note:** Start with a failing integration test that bumps an unsaved installation between the access check and the commit.

**Patterns to follow:** the existing `TakeSnapshot` and `StillCurrent` pairing in `backend/internal/accessfence/fence.go`; fence tests in `fence_integration_test.go`.

**Test scenarios:**
- Bump an installation the user has no row for after `TakeSnapshot` and before `StillCurrent`; `StillCurrent` returns false.
- A bump of any scope after the snapshot returns false; no bump returns true.
- A transaction holding an uncommitted `InvalidateAccess` makes a concurrent `StillCurrent` wait, then return false once it commits.
- A concurrent `InvalidateAccess` on an installation and a private `StillCurrent` over that installation, repeated 25 times, never returns a deadlock error.
- Private refresh integration: removal webhook for an unsaved installation lands during the fake GitHub fetch; metadata and last-success are not written and the job defers.
- Down migration restores the three-scope constraint.

**Verification:** accessfence and ingestion integration tests pass; `-race` on both.

### U2. Revocation webhook revokes credentials

**Goal:** A signed `github_app_authorization` event ends that user's access, not only their cached reads.

**Requirements:** R2 (KTD2)

**Dependencies:** none

**Files:**
- `backend/internal/ingestion/webhook.go`
- `backend/internal/ingestion/webhook_integration_test.go`

**Approach:**
- In the `github_app_authorization` branch, after the user bump, call `DeleteUserSessions` and `ClearUserTokens` through the webhook transaction's queries, only when the sender maps to a known user.
- Unknown sender stays a no-op acknowledgement.

**Execution note:** Proof-first: extend the existing webhook integration test to assert tokens and sessions after delivery.

**Patterns to follow:** `auth.Service.Revoke` in `backend/internal/auth/service.go` for query order.

**Test scenarios:**
- Delivery for a known user: epoch bumped, sessions deleted, token columns cleared, all in one commit.
- The same delivery redelivered: acknowledged, no second effect.
- Unknown sender: acknowledged, nothing written.
- A failing cleanup query rolls the whole delivery back (no bump without cleanup, no delivery row), so GitHub can redeliver.
- After delivery the refresh handler finds no credential for that user and pauses without spending an attempt; another selector's credential still works.

**Verification:** webhook integration tests pass.

### U3. Commit-time selection lock

**Goal:** A refresh cannot write metadata after its credential owner's selection stopped being eligible.

**Requirements:** R3 (KTD3, KTD4)

**Dependencies:** U1 (same commit transaction; keep fence check order)

**Files:**
- `backend/queries/ingestion.sql`
- `backend/internal/ingestion/refresh.go`
- `backend/internal/ingestion/refresh_integration_test.go`

**Approach:**
- Add a query that selects the owner's selection row `FOR SHARE NOWAIT` with the eligibility predicate; run it first in the commit transaction, after the claim check.
- No row, or lock-not-available (55P03), returns `errRace`; the existing branch defers 5 s.
- Keep the transaction short: lock, fence check, two writes.

**Execution note:** Start with a failing concurrency test.

**Patterns to follow:** `ListEligibleSelectors` and the `errRace`/`errNoSelection` handling in `backend/internal/ingestion/refresh.go`.

**Test scenarios:**
- Deselection in a second transaction opened before the commit and committed after its lock: the refresh gets `errRace`, writes nothing.
- Deselection committed before the lock: `errNoSelection`, nothing written.
- Suspension (unstar) behaves like deselection.
- A deselect already holding the row lock makes the refresh return `errRace` immediately rather than waiting.
- Star-job commit and a refresh commit on overlapping repositories complete without a deadlock error across 25 repeated runs.
- Owner still eligible: metadata and last success are written.

**Verification:** concurrency test stable under `-race -count=25`.

### U4. Fenced star write

**Goal:** A worker that lost its claim cannot overwrite a newer star snapshot, and eligibility and resumed work commit with the snapshot.

**Requirements:** R4 (KTD6, KTD7)

**Dependencies:** U1

**Files:**
- `backend/internal/repositories/access.go`
- `backend/internal/repositories/starred.go`
- `backend/cmd/api/main.go`
- `backend/internal/ingestion/reconcile.go`
- `backend/cmd/worker/main.go`
- `backend/internal/ingestion/reconcile_integration_test.go`
- `backend/internal/repositories/starred_integration_test.go`

**Approach:**
- `repositories.Service` gets a fetch-only method (all pages or an error or a rate-limit retry time; revokes on 401 as today) and a store method taking a transaction (advisory lock, delete, insert, snapshot row, suspend, restore, `OnEligibilityChange`).
- Rename `OnStarsReplaced` (from `aae4ff7`) to `OnEligibilityChange` here, in `access.go`, `starred.go` and the `cmd/api` and `cmd/worker` wiring; the store method calls it. U5 adds the remaining call sites.
- `refreshStars` becomes fetch then store in its own transaction.
- `ingestion.StarRefresher` becomes fetch plus store; `StarReconcileHandler` fetches (no write), then one `fence.Commit` calls store and `RecheckEligibility`. The worker adapter maps to the new methods and stops calling `Starred(refresh=true)`, which removes the extra private-entry checks.

**Execution note:** Start with a failing test where the claim is lost between fetch and store.

**Patterns to follow:** the current handler failure mapping in `StarReconcileHandler`; the fence stand-in in `reconcile_integration_test.go`.

**Test scenarios:**
- Lost claim after fetch: store is not run, the snapshot is unchanged, the handler returns without spending an attempt.
- Completed refresh: snapshot replaced, unstarred follow suspended, restarred restored, paused repo jobs resumed, all in one commit; a failure in any step rolls all back.
- Partial fetch (page error) and rate limit change nothing; rate limit defers to the retry time.
- No credential pauses with no attempt spent.
- Interactive `/starred?refresh=1` still replaces the snapshot and updates eligibility (the `aae4ff7` test keeps passing).
- Two concurrent stores for one user serialize on the advisory lock.

**Verification:** repositories and ingestion integration tests pass.

### U5. Resume on reselect, sign-in and installation change

**Goal:** A user who fixes the cause of a pause sees paused work resume immediately.

**Requirements:** R5 (KTD7)

**Dependencies:** U4 (the hook and its rename land there)

**Files:**
- `backend/internal/repositories/selections.go`
- `backend/internal/repositories/http.go`
- `backend/internal/auth/service.go`
- `backend/cmd/api/main.go`
- `backend/cmd/worker/main.go`
- `backend/internal/repositories/fence_integration_test.go`
- `backend/internal/auth/session_test.go`

**Approach:**
- Call `OnEligibilityChange` (introduced in U4) from `storeSelection` (after `OnSelect`) and from `Installed` (the installation upsert and the hook share one transaction).
- `auth.Service.CompleteLogin` wraps user upsert, session creation and `OnSignIn` in one transaction.
- Wire both hooks to `ingestion.RecheckEligibility(ctx, tx, userID, now)` in `cmd/api` and `cmd/worker`.

**Execution note:** Start with a failing test: pause a repo job for no credential, select it again, assert the job is queued.

**Patterns to follow:** `OnSelect` and its rollback test `TestConnectRollsBackWhenSchedulingFails`.

**Test scenarios:**
- Paused refresh for a repo, user reselects: job queued, `run_at` now.
- Dead job and reselect: stays dead.
- Sign-in with a paused star job: queued.
- Installation connect: the user's paused refreshes resume in the same transaction as the installation row; a hook error rolls back both.
- Hook failure in `storeSelection` leaves no selection.
- Nil hooks change nothing.

**Verification:** repositories and auth tests pass.

### U6. Pause keeps pending intent

**Goal:** A refresh requested while a job runs is not lost when that job pauses.

**Requirements:** R6 (KTD8)

**Dependencies:** none

**Files:**
- `backend/queries/jobs.sql`
- `backend/internal/jobs/jobs_integration_test.go`

**Approach:**
- `PauseJob` sets state to `queued` with `run_at` = now when `pending` is true, otherwise `paused`; `pending` cleared either way; no attempt change.
- `worker.go` needs no change; the execrows contract still reports one affected row.

**Execution note:** Start with a failing test that enqueues during a running job that returns Pause.

**Patterns to follow:** `DeferJob`, `ResumeJob` and the pending tests in `backend/internal/jobs/jobs_integration_test.go`.

**Test scenarios:**
- Running job, `Enqueue` during run, handler returns Pause: job is queued, attempts unchanged, `pending` false.
- Same without the `Enqueue`: job is paused.
- Pause with a lost claim: no row changes.
- A re-queued job that pauses again with nothing pending ends paused.

**Verification:** jobs integration tests pass; `-race`.

### U7. Shared durable credential throttle

**Goal:** A rate limit seen by either job kind paces the other for the same credential.

**Requirements:** R7 (KTD9)

**Dependencies:** U4 (star handler seam)

**Files:**
- `backend/migrations/00010_credential_throttle.sql`
- `backend/queries/ingestion.sql`
- `backend/internal/ingestion/refresh.go`
- `backend/internal/ingestion/reconcile.go`
- `backend/internal/ingestion/refresh_integration_test.go`
- `backend/internal/ingestion/reconcile_integration_test.go`

**Approach:**
- Table `credential_throttle(user_id primary key, retry_after timestamptz)` with an upsert that keeps the later time.
- Replace the in-memory map and mutex in `Deps` with queries; both handlers read before any GitHub call and write on a rate limit.
- Defer to the stored time, never rotate to another credential (parent plan rule).

**Execution note:** Start with a failing test: metadata rate limit, then a star job for the same user.

**Patterns to follow:** current `throttledUntil`/`throttle` call sites in `refresh.go`.

**Test scenarios:**
- Metadata rate limit for user A: A's star job defers to that time with no GitHub call.
- Star rate limit for A: A's metadata refresh defers; another user's jobs run.
- Expired throttle is ignored.
- A later time overwrites an earlier one; an earlier one does not overwrite a later one.
- A new `Deps` (restart) still sees the stored throttle.

**Verification:** ingestion integration tests pass.

### U8. Docs

**Goal:** Operators and API readers see the current behavior.

**Requirements:** R8

**Dependencies:** U1 to U7

**Files:**
- `docs/operations.md`
- `docs/api.md`

**Approach:**
- Operations: the global version and why every bump serializes with private commits; webhook revocation effects; lock order and the `NOWAIT` retry; where resume is triggered; the shared throttle table.
- API: no wire change expected; update only if a response or status code changed.

**Test expectation:** none -- documentation only.

**Verification:** the sections named in the units exist and match the code.

---

## Verification Contract

| Gate | Command | Applies |
|---|---|---|
| Format and vet | `gofmt -l backend` (empty), `go vet ./...` and `go vet -tags=integration ./...` from `backend` | every unit |
| Unit tests | `go test ./...` from `backend` | every unit |
| Integration | `DATABASE_URL=postgres://repoorbit:repoorbit@localhost:54329/repoorbit?sslmode=disable go test -tags=integration ./... -count=1` | every unit |
| Race | `go test -tags=integration -race ./internal/jobs ./internal/ingestion ./internal/accessfence ./internal/repositories` | U1 to U7 |
| Concurrency stability | the U3 deadlock test with `-count=25` | U3 |
| Generated code | `make generate` after query or migration changes; commit regenerated `backend/internal/db` | U1 to U7 |

## Definition of Done

- Each of R1 to R8 is met and each unit's test scenarios exist and pass.
- All gates above pass on the final tree; no skipped tests.
- Abandoned-attempt code is removed from the diff.
- Files already dirty in the working tree before this work (`GLOSSARY.md`, the parent plan, `docs/plans/2026-11-17-u4-behavior.md`) are not committed by this run.

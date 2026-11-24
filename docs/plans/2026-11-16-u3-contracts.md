# U3a–U3d parallel contracts

Companion to `2026-11-16-1846-feat-repoorbit-intelligence-plan.md` (U3). Frozen before parallel work starts; the plan stays the authority for behavior. A worker that needs a contract change **stops and reports** instead of editing another unit's surface.

Base commit for all worktrees: the commit that adds this file.

## Ownership

| Unit | Migration | Owns (exclusive writes) |
|------|-----------|-------------------------|
| U3a | `00006_jobs.sql` | `internal/jobs/**`, `queries/jobs.sql`, `docs/operations.md` section "Job queue" |
| U3b | `00007_ingestion.sql` | `internal/accessfence/**`, `internal/ingestion/` (except `webhook*`, `reconcile*`, `status*`; deletes `contract.go`), `queries/ingestion.sql`, `internal/repositories/**` and `internal/auth/**` edits needed for atomic scheduling and fence use |
| U3c | `00008_webhooks.sql` | `internal/ingestion/webhook*.go`, `queries/webhooks.sql`, `docs/{security,github-app-setup}.md`, `.env.example`, `compose.yaml`, `docs/operations.md` section "Webhooks" |
| U3d | `00009_status.sql` | `internal/ingestion/reconcile*.go`, `internal/ingestion/status*.go`, `queries/status.sql`, `backend/graphql/**`, `apps/web/**`, `docs/api.md`, `docs/operations.md` section "Reconciliation and status" |

**Orchestrator-only files** (workers must not edit; the orchestrator wires them at integration): `backend/cmd/{api,worker}/main.go`, `backend/internal/platform/config.go`, and everything under `backend/internal/db/` (generated; regenerated with `make generate` after every merge, never hand-merged). Workers may regenerate sqlc inside their own worktree to compile and test; the orchestrator discards that output on merge. Each worker reports the wiring it needs (constructor calls, config fields, env vars) in its final message.

Each worker's test code lives beside its code. Integration tests use the existing `internal/testutil` DB helper and the `integration` build tag.

## C1. Jobs API (U3a provides; U3b/U3c/U3d consume)

Frozen signatures live in `backend/internal/jobs/api.go` (compiling stub; U3a replaces bodies). Summary:

- One row per `(kind, key)`. Kinds: `repo_metadata_refresh` key `repo:<repositories.id>`; `star_reconcile` key `user:<users.id>`.
- States: `queued | running | paused | dead`.
- `Enqueue(ctx, tx, kind, key, runAt)` runs inside the caller's transaction. Coalesces when queued, sets `pending` when running/paused/dead, **never revives dead**.
- `Resume` unparks paused; `RequeueDead` is the operator-only revive of dead with a fresh attempt budget.
- Handler return: `nil`; `*Defer` (field `RetryAt`) (rate limit, no attempt spent); `*Pause` (no credential, no remote calls, no attempt spent); `Permanent(err)` (dead); other error (backoff + jitter, max 5 attempts).
- `ClaimFence.Commit(ctx, fn)` verifies claim generation and lease inside the transaction; no network calls in `fn`.
- Timing: 2 concurrent jobs, 60 s lease, 20 s heartbeat, 5 min max per attempt. `Clock` is injected; production uses wall time with jitter.
- Other units test against a **fake** `jobs` only through the exported API; until U3a merges, their integration tests that need a real queue use `t.Skip("needs U3a")` guarded by `errors.Is(err, jobs.ErrNotImplemented)`, and are un-skipped by the orchestrator after merge. Unit-level logic (handlers, HMAC, eligibility) is tested directly without the queue.

## C2. Access fence (U3b provides; U3c consumes `InvalidateAccess`)

Frozen in `backend/internal/accessfence/fence.go`.

- Table `access_fences(scope text, subject_id bigint, epoch bigint, PRIMARY KEY (scope, subject_id))`, scopes `user | installation | repository`.
- `InvalidateAccess` increments the epoch in the caller's transaction **before** cleanup. Webhooks never grant access.
- `TakeSnapshot` records epochs before a remote fetch; `StillCurrent` re-checks inside the commit transaction. Private commits and cached private reads withhold on `false`. `repositories.Service` consults epochs so a process-local cache cannot outlive a bump.

## C3. Ingestion surface

Frozen in `backend/internal/ingestion/contract.go` (U3b replaces it).

- `RequestMetadataRefresh(ctx, tx, repoID) (scheduled bool, err)`: enqueues `repo_metadata_refresh` only if the repository has an active, follow-eligible selection. Selection changes, webhooks, and polls all go through it.
- Selection eligibility column, owned by U3b in `00007`: `selections.follow_suspended_at timestamptz NULL`. NULL means eligible. **U3d writes it** (unstar suspends, completed refresh with the repo restores). U3b's credential picker and `RequestMetadataRefresh` already honor it.
- Credential picker (U3b): an eligible active selecting user with a usable token; fall back to another; none means `*jobs.Pause`. Private additionally requires a live access check plus fence `StillCurrent`.
- Repository sync status columns (job state, last success, next retry, paused flag, safe error code) are **read by U3d from the `jobs` table plus `repository_sync` (U3b, `00007`)**: `repository_sync(repository_id PK, last_success_at timestamptz, last_error_code text)`. No raw errors, no credential identities.

## C4. Webhooks (U3c)

- Table `webhook_deliveries(delivery_id text PRIMARY KEY, event text NOT NULL, received_at timestamptz NOT NULL)`. Insert and `RequestMetadataRefresh`/`InvalidateAccess` commit in one transaction; acknowledge only after commit. Duplicate delivery id: acknowledge, no second work.
- Route: `POST /webhooks/github`, mounted by the orchestrator; U3c exposes `ingestion.WebhookHandler(WebhookDeps) http.Handler`, `WebhookDeps{Pool, Secret []byte, Clock}`.

## C5. Reconciliation and status (U3d)

- Exposes `ingestion.RecheckEligibility(ctx, userID)` (resume paused work after sign-in/reselect/star refresh/installation change) and a periodic scheduler started by the worker via `ingestion.StartScheduler(ctx, Deps)`: metadata poll 15 min, star reconcile hourly, jitter, injected clock.
- GraphQL exposes metadata status separately from activity coverage; the "activity not yet synchronized" message stays.

## Integration order and checks

Merge **a, b, c, d** in that order. After each merge the orchestrator runs `make generate`, then `gofmt`, `go vet`, `go build ./...`, `make test`, and `make test-integration` (PostgreSQL on 54329); wiring in `cmd/*` and `config.go` is added by the orchestrator in the merge commit. Un-skip the contract-guarded tests once their dependency has merged.

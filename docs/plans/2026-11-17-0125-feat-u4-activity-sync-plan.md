---
title: U4 Repository Activity and Coverage
type: feat
date: 2026-11-17
artifact_contract: ce-unified-plan/v1
artifact_readiness: implementation-ready
product_contract_source: ce-plan-bootstrap
execution: code
deepened: 2026-11-17
---

# U4 Repository Activity and Coverage

## Goal Capsule

- **Objective:** Give authorized viewers usable repository activity evidence with honest coverage and freshness, and keep that evidence current.
- **Means:** Extend the existing PostgreSQL queue, GitHub client, access fence, and GraphQL read path in four sequential runnable slices.
- **Authority:** This plan carries the U4 decisions in the updated repository intelligence plan and its U4 interview companion. U3 through `4180951` is the baseline. Preserve the original documents in the other worktree; this plan contains the U4 contract needed for execution.
- **Stop conditions:** If GitHub cannot provide a confirmed guarantee, report the affected guarantee and evidence rather than marking the resource complete or silently weakening it. Never expose or erase private data on ambiguous access evidence.

## Product Contract

### Problem Frame

A repository's metadata does not show whether the project is active or provide the evidence needed for later response, review, and timeline views. Users need readable imported activity and an explicit indication of what is missing or stale.

### Requirements

- R1. Selection starts shared activity synchronization; eligible public repositories require no installation, and private repositories require current user and installation access. Existing eligible selections are reconciled at startup.
- R2. Import issues, labels, assignees, PRs, reviews, issue-thread comments, published releases, and supported issue events. Preserve source IDs, actors, bot identity, source times and links needed for later metrics; distinguish observed time where source time is unavailable.
- R3. Fix an initial boundary at least 90 days before import starts. Include older open issues and PRs, PRs merged inside the window, and older comments/reviews necessary for metrics. Finish a catch-up pass before declaring coverage complete.
- R4. Persist independent resource checkpoints, coverage, success, failure and retry information. Partial evidence remains readable with current authorization; an incomplete resource is unknown, not zero. A resource failure does not stop independent streams.
- R5. Relevant signed webhooks accelerate reconciliation; 15-minute polling repairs missed signals. A selecting user can request activity refresh without clearing evidence, bypassing pacing or reviving dead work. Requests coalesce and intent arriving during work survives.
- R6. Page effects and checkpoints commit atomically under current job claim and access/eligibility fences. Recheck current visibility for reads, withhold inaccessible content, and purge shared private projections when no authorized installation still needs them.
- R7. Openings use creation time. Import available close, reopen, assign, unassign, label and unlabel events without inferring prior transitions from current state. Preserve a visible gap for unsupported history.
- R8. Archived repositories remain readable and synchronized while eligible; rename preserves stable ID. Confirmed deletion removes current issue/comment/review/release content and retains minimal identity to prevent resurrection; ambiguous failures and incomplete listings never prove deletion.
- R9. Activity is synchronized only when every required resource has completed backfill and catch-up. Freshness, subsequent failure, unsupported history, metadata status and demo data remain distinct.

### Key Decisions

- **Evidence before analytics** (session-settled: user-approved — chosen over combining ingestion with finished analytics: evidence can be checked before derived views). Governs R2–R4, R7, R9.
- **Fixed boundary and resource coverage** (session-settled: user-approved — chosen over a rolling cutoff and all-or-nothing status: long imports and individual failures must not conceal gaps). Governs R2–R4, R9.
- **Polling plus coalesced refresh** (session-settled: user-approved — chosen over webhook-only and destructive resets: polling repairs missed signals without discarding progress). Governs R1, R5.
- **Source evidence and lifecycle** (session-settled: user-approved — chosen over inferred history, stopping at archive, or deleting on lookup failure: source evidence and access must remain distinct). Governs R6–R8.

### Scope Boundaries

U4 delivers evidence, authorized reads, synchronization and coverage/status. U5 owns analytics calculations, charts and timeline rendering. Recommendations, issue writes, source-code ingestion, new dependencies and generic ingestion frameworks are outside this work. The GitHub App private-key configuration required for safe cleanup is included; it is not a user-facing tuning control. Live GitHub acceptance is separate from automated acceptance; report missing credentials or consent as a live-verification gap.

### Success Criteria

All four units and their scenarios pass; partial streams and permanent gaps are visible without suggesting inactivity. A public repository without installation and an authorized private repository are imported separately when credentials permit, recording coverage, throttling and private release permission behavior.

## Planning Contract

### Assumptions

- The updated U4 behavior interview and updated master plan in the source checkout are authoritative inputs; neither is copied over or edited here. The stable master requirements R3–R7 and KTD3–KTD6, KTD9–KTD12 are represented by this plan's R1–R9.
- Allocate the next unused migration number at implementation time. Existing demo rows and IDs must remain distinct from real GitHub identities.
- Mutable pagination converges when the relevant list is stable for successive complete scans; continuous mutation may delay completion. A GitHub endpoint's absence, 404, incomplete page or lost permission does not independently establish deletion. Where positive deletion evidence is unavailable, retain the content as unconfirmed subject to access checks and expose the gap.

### Key Technical Decisions

- KTD1. Use independently scheduled resource work and checkpoints so a failing or throttled stream cannot block others. Persist fixed boundary, traversal phase/cursor, last success and retry/failure per resource; retain a separate catch-up watermark. Idempotent upserts use stable source identity and source-version ordering so an older replay cannot overwrite newer content; handle equal or absent source times conservatively without regressing state. Preserve U3 pending/dead semantics and include activity kinds in eligibility recovery.
- KTD2. Commit each fetched page and its checkpoint in the same short claim-fenced transaction, revalidate the credential owner's eligible selection and current private-access snapshot before writing, and never keep a DB transaction across GitHub I/O. Activity pages do not report repository visibility: freshly verify visibility and access before fetching and recheck after fetching before commit; on a public-to-private change or unverifiable access, withhold the page, tighten stored visibility and retry through the private fence path. The fence handles observed concurrent invalidation; no live check can detect a change GitHub has not reported. Reuse the U3 shared throttle and fallback; do not expose credentials or raw errors, and never log tokens or imported private content.
- KTD3. Treat paginated endpoints as mutable, not snapshots. Use deterministic ordering where supported, a fixed backfill boundary and an overlapping catch-up scan; advance durable coverage after a complete backfill traversal and an overlapping catch-up through a captured high-water time. For old open issues and PRs, catch-up repeats full open-object traversal, not just an updated-since scan; repeat full scans until two consecutive completed scans agree on stable identities, then complete catch-up through a captured high-water time. Replay and overlap updates around checkpoints. This is eventual reconciliation under an interval stable enough to converge, not a claim of snapshot isolation; retain an incomplete status while scans differ or fail. Restart or overlap a traversal when a cursor is unsafe; never infer completeness from timestamps alone. Explicitly test insertions and updates that shift pages during interruption.
- KTD4. U3 has invalidation and authorization fences but no shared activity-projection purge. Atomically enqueue durable cleanup re-evaluation for user, installation and repository security invalidations, independently of eligible activity jobs. Conservatively enumerate locally stored private repository IDs for user- or installation-wide events, persist progress and retry after restarts. A user token checks only that viewer's access; its revocation or a missing local selector cannot prove that the app installation is gone. For the app-wide decision, authenticate as the GitHub App using its existing client ID and a configured private-key file; list all app installations with an app JWT, obtain installation tokens, and list each installation's accessible repositories through all pages. A confirmed repository membership in any installation retains the projection even if no user credential remains. For a negative decision require two consecutive complete, successfully authenticated app inventories and per-installation repository listings with the same installation/repository membership, and retain pending cleanup while they differ. This converges under stable intervals and does not assert snapshot isolation. Purge only when those inventories establish no installation can access the stable repository ID. An incomplete response, missing key, suspended/indeterminate installation, rate limit, rename ambiguity or error is unknown and must retry, never count as absence. While cleanup is pending, each read independently requires a fresh authorized viewer-and-installation check; withhold when that check is unverified, not merely because inventory failed. Signed removal or uninstall events trigger re-evaluation; delayed events alone never prove current absence or grant access. Capture the global access fence before the inventory and recheck/lock it in the purge transaction; observed concurrent authorization or membership changes abort and retry. A missed upstream event is not observable by this fence, so periodic inventory repairs missed signals; it cannot promise instant detection of unreported changes. In the same fenced purge transaction invalidate resource coverage, cursor and watermark while preserving the fixed boundary; advance a rebuild generation to fence stale work. Verified restoration schedules a full backfill and catch-up, while dead work still requires operator requeue. App credentials are only for cleanup inventory, never a substitute for viewer authorization or U3's user-scoped ingestion. Apply to every resource as introduced.
- KTD5. Resource status reports backfill boundary/phase, catch-up, last success, current retry/failure and permanent gaps separately. Overall synchronized derives from all required streams, never metadata success. Freshness remains separate and earlier success does not mask a later error.
- KTD6. Use confirmed source deletion events or another positively verified source-specific signal; store a minimal tombstone and apply it under the same fences as writes. Never infer deletion from a missing page or an ambiguous GitHub 404. Reconciliation must not resurrect a tombstone from older observations; distinguish a genuinely newer source record if restoration is supported.

### High-Level Technical Design

Directional data flow: selection / webhook / poll / manual request → durable per-resource jobs → credential and access check → GitHub page fetch → claim and access fenced projection plus checkpoint → authorized GraphQL read and status UI. A security invalidation independently withholds reads and triggers cleanup/re-evaluation.

Each stream moves through `not started → backfill → catch-up → current`; a failure, throttle or pause retains its checkpoint and overlays state without erasing prior success. Pending requests trigger another pass; dead work requires operator requeue. The repository's synchronized indicator is the conjunction of required stream coverage, while freshness and gaps remain separate.

### API Constraints and Research

- [App-wide installation listing](https://docs.github.com/en/rest/apps/apps#list-installations-for-the-authenticated-app) requires an app JWT; [installation repository listing](https://docs.github.com/en/rest/apps/installations#list-repositories-accessible-to-the-app-installation) uses an installation token. [User-token listings](https://docs.github.com/en/rest/apps/installations#list-repositories-accessible-to-the-user-access-token) are scoped to that user and cannot prove app-wide absence. The private-key file is an operator-provided secret; validate its readability and key format at real-mode startup, do not log it or store it in the database, and keep demo mode credential-free. Existing real-mode deployments must supply it before upgrading.

- GitHub's [repository issues endpoint](https://docs.github.com/en/rest/issues/issues#list-repository-issues) includes PR-shaped entries; its `since` filters updates, so separately include old open issues and exclude PR-shaped records. [Pull request listing](https://docs.github.com/en/rest/pulls/pulls#list-pull-requests) has no `since` parameter; traversal must include old PRs merged within the window. [Reviews](https://docs.github.com/en/rest/pulls/reviews#list-reviews-for-a-pull-request) are paginated per PR, and GitHub [keeps pending review comments visible only to their author](https://docs.github.com/en/pull-requests/get-started/reviewing-pull-requests-quickstart); older reviews/comments required for qualifying objects cannot be cut off at the initial boundary.
- Follow GitHub's [pagination links](https://docs.github.com/en/rest/using-the-rest-api/using-pagination-in-the-rest-api) and do not assume snapshot isolation. [Issue events](https://docs.github.com/en/rest/issues/events#list-issue-events-for-a-repository) do not supply a documented time filter. [Releases](https://docs.github.com/en/rest/releases/releases#list-releases) require appropriate contents permission and drafts must be filtered. GitHub may return [404 for insufficient permissions](https://docs.github.com/en/rest/authentication/authenticating-to-the-rest-api), so it is not proof of deletion.

## Implementation Units

### U4a. Resumable issues, coverage and private cleanup

**Goal:** Selecting a repository imports usable issues and reports partial coverage safely. **Requirements:** R1–R4, R6, R9; KTD1–KTD5. **Dependencies:** U3d at `4180951`.

**Files:** `backend/internal/ingestion/{sync.go,schedule.go,reconcile.go,status.go,refresh.go,webhook.go}`, `backend/internal/jobs/api.go`, `backend/internal/githubapp/{client.go,fake/fake.go}`, `backend/internal/accessfence/fence.go`, `backend/internal/repositories/{selections.go,access.go}`, `backend/internal/auth/service.go`, `backend/internal/platform/config.go`, `backend/cmd/{api,worker}/main.go`, `.env.example`, `compose.yaml`, `docs/{github-app-setup,operations,security}.md`, `backend/migrations/<next>_activity.sql`, `backend/queries/{activity,repositories}.sql`, generated `backend/internal/db/`, `backend/graphql/{schema.graphqls,status.go,schema.resolvers.go,generated.go,model/}`, `apps/web/src/{lib/api.ts,components/metadata-status.tsx}`, `docs/api.md`; tests `backend/internal/ingestion/sync_integration_test.go`, `backend/internal/ingestion/webhook_integration_test.go`, `backend/internal/repositories/cleanup_access_test.go`, `backend/internal/repositories/fence_integration_test.go`, `backend/internal/platform/config_test.go`, `backend/graphql/status_integration_test.go`, `apps/web/tests/status.spec.ts`.

**Approach:** Add resource jobs and persistent issue, normalized label/assignee and coverage state, preserving demo and existing GraphQL issue behavior. Attach enqueue atomically to selection and startup reconciliation. Implement KTD4 for every private issue projection and apply the existing live-read boundary. Configure a GitHub App private-key file for real mode, validate it at startup, and document provisioning and upgrade requirements; demo mode stays credential-free. Extend the existing GitHub client/fake with app-JWT inventory and installation-token listing. Preserve the in-progress U4 files while correcting their access assumptions: separate owned/public visibility from installation-visible membership and require the user-and-installation intersection for private reads and ingestion; a user-owned listing alone proves neither installation membership nor global absence. Render issues already imported and explicit not-yet-imported status for all future resources. Follow `backend/internal/ingestion/refresh.go` for credential fallback, share lock and access snapshot; `backend/internal/jobs/worker.go` for claim fencing; `backend/graphql/schema.resolvers.go` for authorized reads. Prove the stale-worker and revoked-access paths early.

**Scenarios:** (1) selection and job commit together; existing selections schedule once on startup; (2) multi-page interruption/replay imports exactly one copy, excludes PR-shaped issues, includes old open and relevant closed issues, and preserves the original boundary through catch-up; (3) inserted/updated records shift pages during resume and are recovered or leave explicit incomplete coverage; (4) claim loss, deselection, revocation and a detected public-to-private transition during fetch block both page and checkpoint even with a missing webhook; (5) last authorized installation triggers purge and prevents stale resurrection, another authorized installation retains data even after every local user revokes; two converged complete inventories confirming no installation purge, while shifted pages, incomplete inventory, rejected credentials and pagination errors keep cleanup pending; an independently verified viewer can still read and an unverified viewer is withheld; installation-only and user-wide invalidations discover affected repositories and resume cleanup after a restart, including no remaining eligible selector, stale signed removal followed by restoration, a concurrent fence bump, missing app key, and transient verification failure; reauthorization after purge rebuilds old unchanged evidence from the preserved boundary, remains incomplete through interrupted recovery, and rejects old-generation commits; a private owned repository without confirmed installation is withheld, including incomplete installation listing; an authorized private intersection works; guessed IDs and other users do not bypass access; (6) missing credential pauses, rate limit defers, and partial issues remain readable while future streams are incomplete; demo remains labeled.

### U4b. Pull requests, reviews and comments

**Goal:** Store evidence needed for response, review, contributor and merge metrics. **Requirements:** R2–R4, R6, R9; KTD1–KTD5. **Dependencies:** U4a.

**Files:** `backend/internal/ingestion/{sync.go,status.go}`, `backend/internal/githubapp/{client.go,fake/fake.go}`, `backend/migrations/<next>_review_evidence.sql`, `backend/queries/activity.sql`, generated `backend/internal/db/` and GraphQL files as needed, `backend/internal/accessfence/fence.go`, `backend/internal/ingestion/sync_integration_test.go`, `backend/graphql/status_integration_test.go`, `docs/api.md`.

**Approach:** Extend fenced page commits and cleanup to PR, review and issue-thread comment resources. Persist author/reviewer, bot type, submitted review state, timestamps and links. Distinguish pending reviews by excluding their unsubmitted content and identity from shared projections; GitHub keeps pending review comments private to their author. Traverse older qualifying objects and their complete required review/comment history; atomically register required child work with the parent page and checkpoint. Gate resource completion on all qualifying discovered children, including parents found during catch-up; track child progress independently and expose authorized evidence and sufficiency. Do not compute metrics.

**Scenarios:** (1) old PR merged yesterday retains creation and earlier reviews; old open issue retains earlier replies; (2) pending reviews never enter shared projections or count as submitted, while bot and self-response remain distinguishable; (3) crash after parent-page discovery cannot lose child work, catch-up-discovered parents delay completion, and interrupted/repeated child pages do not duplicate; review failure or throttling does not prevent issue/comment progress; (4) completed empty differs from incomplete; (5) changed pages do not silently lose records, and newer observations survive older replay and equal or absent timestamps; (6) access revocation blocks every new projection's commit/read and cleanup preserves data required by another authorized installation.

### U4c. Releases and issue-event history

**Goal:** Make published releases and source-backed issue events available with explicit historical gaps. **Requirements:** R2–R4, R6–R7, R9; KTD1–KTD5. **Dependencies:** U4b.

**Files:** `backend/internal/ingestion/{sync.go,status.go}`, `backend/internal/githubapp/{client.go,fake/fake.go}`, `backend/migrations/<next>_activity_events.sql`, `backend/queries/activity.sql`, generated DB/GraphQL artifacts, `backend/internal/accessfence/fence.go`, `backend/internal/ingestion/sync_integration_test.go`, `backend/graphql/status_integration_test.go`, `docs/api.md`.

**Approach:** Import published releases and available closed/reopened/assigned/unassigned/labeled/unlabeled issue events with stable source identity, actor, source and observation times and links. Source creation timestamps provide openings. Keep webhook delivery IDs distinct from event IDs; represent unsupported history as a permanent gap. Extend authorization and cleanup for both streams.

**Scenarios:** (1) repeated multi-page fetches yield one source record; late events keep source chronology; (2) current state does not synthesize history; unavailable events preserve a gap after traversal; (3) drafts are excluded, private-release permission failure leaves other streams usable; (4) revocation blocks writes and reads and cleanup reaches both new resource types.

### U4d. Continuous reconciliation, lifecycle and final acceptance

**Goal:** Keep activity current and make refresh, freshness and lifecycle truthful end to end. **Requirements:** R1, R4–R9; KTD1–KTD6. **Dependencies:** U4c.

**Files:** `backend/internal/ingestion/{webhook.go,reconcile.go,schedule.go,sync.go,status.go}`, `backend/internal/repositories/{http.go,selections.go}`, `backend/internal/jobs/api.go`, `backend/internal/githubapp/{client.go,fake/fake.go}`, `backend/cmd/{api,worker}/main.go`, `backend/queries/activity.sql`, optional `backend/migrations/<next>_activity_reconciliation.sql`, generated DB/GraphQL artifacts, `backend/graphql/status.go`, `apps/web/src/{components/metadata-status.tsx,app/repositories/page.tsx,app/starred/page.tsx}`, tests `backend/internal/ingestion/{sync_integration_test.go,webhook_integration_test.go,reconcile_test.go}`, `backend/internal/repositories/fence_integration_test.go`, `backend/graphql/status_integration_test.go`, `apps/web/tests/status.spec.ts`, `docs/{api,operations,security,github-app-setup}.md`.

**Approach:** Route relevant signed issue, comment, PR, review and release events to durable work as reconciliation signals; signed, deduplicated deletion actions are the narrow exception and may create fenced tombstones as positive evidence, even when a follow-up lookup is unavailable. Keep security invalidation independent. Extend jittered polling and `RecheckEligibility` to activity jobs. Add an authenticated, CSRF-protected per-repository “Refresh activity” action for the viewer's eligible selection and reuse the queue's pending/dead behavior. Show accessible in-progress, accepted/coalesced, failure and dead-work feedback distinct from listing refresh. Persist archived state, preserve identity across renames, and implement KTD6 per supported deletion signal. Surface synchronized, stale, failed, gap and demo states separately; document subscriptions, permissions, cleanup, requeue and limitations.

**Scenarios:** (1) signed deliveries deduplicate, only schedule selected repositories and repeated triggers yield one source event; polling repairs missed events for public and private access; (2) repeated refresh coalesces, mid-run intent survives, forged/CSRF requests fail, throttle holds, dead jobs stay dead, and the browser shows the action feedback states; (3) losing one selector preserves other eligible work, no selectors stop remote work, recovery resumes paused streams; (4) rename preserves ID, archived stays readable and refreshed; confirmed signed deletion tombstones content despite a later unavailable lookup, stale pages cannot resurrect it, ambiguous 404 and partial pages do not delete; (5) access loss withholds immediately and fences/purges according to KTD4; (6) complete coverage plus catch-up enables synchronized status while subsequent failure, stale freshness, permanent gaps and demo remain visible.

## Verification Contract

Run formatting, generation drift, vet, unit tests, PostgreSQL integration and targeted race/concurrency tests after affected slices; run the full backend and affected browser suites at the end. Use fake GitHub fixtures for pagination mutation, old evidence, independent failures, claim loss, authorization, cleanup and deletion cases. Verify app-key configuration, secret handling and real/demo startup, app-wide multi-page inventory and user-access separation. Verify incremental migrations preserve representative seeded/demo data and exercise supported rollback. Check API and operations documentation against final behavior. Separately perform public-no-installation and authorized-private live imports when credentials are available, recording coverage, retry behavior and permission gaps; inability to run live checks remains explicit.

## Definition of Done

- U4a–U4d scenarios pass and each leaves a runnable slice; every stored private resource has authorized reads, fenced writes and cleanup from its first introduction.
- All resource coverage, catch-up, failures, freshness and permanent gaps are independently honest; no missing data is represented as zero.
- Automated verification and live acceptance results are reported separately; no claim of live acceptance is made without the checks.
- U5 analytics and timeline rendering are not started.

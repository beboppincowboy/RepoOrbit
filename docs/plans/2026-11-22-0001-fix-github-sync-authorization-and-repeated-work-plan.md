---
title: Fix GitHub sync authorization and repeated work
type: fix
status: active
artifact_contract: ce-unified-plan/v1
artifact_readiness: requirements-only
execution: code
date: 2026-11-22
---

# Fix GitHub sync authorization and repeated work

## Goal

Complete activity catch-up without account-wide GitHub listings per page or repeatedly resetting comment and review scans. Preserve fresh authorization before fetching and before saving, and preserve the existing transactional selection, access epoch, generation, and cursor checks.

## Verified baseline

- The previous fake-GitHub integration run observed four installation listings for two issue pages. Both page checks call the account-wide visibility path through `repositories.CheckOne`.
- The signed-in local stack advanced from zero to 11 of 12 top-level streams current. That earlier snapshot had 34 incomplete comment scans and 23 incomplete review scans. On November 22 at 10:05 a.m. Vancouver time, all 12 top-level streams were current, all 57 child scans were complete, the queue was empty, and no user cooldown was active. Every child had reached generation 90. The snapshot confirms catch-up converged and captures the accumulated resets, but does not attribute each generation bump to a specific trigger.
- All three parent call sites in `sync.go` and `evidence_pull_requests.go` call `registerChild`. On conflict, it increments generation and clears the cursor and both reconciliation scans. Child completion requires consecutive matching passes. Queue uniqueness on kind and key prevents duplicate job rows while allowing repeated work on the same row.

## Implementation units

### U1. Verify the GitHub authorization contract

Use a disposable private repository, a GitHub App installed on selected repositories, and an authorized test identity whose user grant can be removed independently. Record status codes and repository IDs only. Compare the existing ID-based lookup with the documented name-based lookup for both grants present, user grant removed, and app repository grant removed. Repeat with grants restored and test the public-star case. Verify JWT installation resolution, including no installation, suspension, and repository rename. Use a disposable fixture and obtain authorization for grant changes before running this matrix. An inconclusive matrix blocks U2's private path. Use the documented endpoint if it passes and ID lookup fails. Stop and revisit the design if neither endpoint establishes the intended grant.

### U2. Add fresh repository-specific worker authorization

Add an ingestion-specific check in `repositories` using the existing GitHub client. Verify returned repository ID and retain existing eligibility and visibility handling. Resolve the installation separately through the app JWT repository endpoint before fetching. Treat missing or unverified installation access for a private repo as a reason to defer or pause, never as permission to fetch with a user token. An eligible public repository without an installation can use the user token. Preserve token revocation on 401, handle authoritative access loss separately from ambiguous 403/404 responses, and honor GitHub rate-limit responses, including rate-limited 403 and 429. Recheck user authorization and repository identity, visibility, and name after fetching. Keep the existing claim and database fences. Test installation-token routing and assert two direct user checks and zero account-wide listings on normal issue and shared evidence pages.

### U3. Isolate credential cooldowns

Try another eligible selector after a user credential throttles. Track blocking deadlines for the credentials actually needed by each candidate, and defer until the earliest viable route when every candidate is blocked. Respect a shared installation cooldown across selectors, including installation resolution or token refresh where applicable. Pace deferred jobs after the provider deadline so backfill cannot consume the entire newly available budget in one burst. Use the existing queue and throttle machinery. Verify alternate users, alternate installations, a shared throttled installation, and eventual progress after reset.

### U4. Preserve child reconciliation progress

Separate discovery from invalidation in `schedule.go`. Parent scans should atomically insert and enqueue missing children, and preserve the job, generation, cursor, completion flag, and scan state when they revisit an unchanged parent. Preserve the existing child key conventions for comments and reviews. Keep deliberate invalidation for relevant source changes and explicit refreshes. Audit periodic reconciliation and comment/review event coverage before choosing invalidation signals, since edits and deletions may occur without a reliable parent timestamp change. Preserve a periodic mechanism to detect those changes. Test unchanged parents across multiple scans, relevant changes, refresh, concurrent enqueue, missed events, and resumption of pre-existing incomplete rows.

## Verification contract

- Run the live grant matrix before switching private ingestion. Use fake GitHub integration cases for rate limits, false or mismatched metadata, credential routing, access loss during fetch, unchanged cursors on rejected commits, and public-star selection enforcement.
- Run the ingestion, repositories, GitHub client, and worker test suites, then the repository integration suite. Replace or incorporate the existing scratch call-count repro only after confirming ownership.
- On the signed-in local stack, track top-level stream status, child completion, generation resets, and cooldowns through catch-up and a subsequent reconciliation cycle. Verify a stable repository reaches current coverage, unchanged children are not reset, and genuine refreshes still collect changes.
- Compare user-token request counts against the baseline and update this plan with measured results and remaining gaps. Baseline catch-up was verified. The live private-grant matrix and post-fix reconciliation remain pending.

### U5. Keep repository dashboards available and show durable sync status

Use a labeled static indicator in the sidebar. Red means sync is blocked or failed, yellow means coverage is partial or awaiting its first successful sync, and green means synchronized. Treat background refresh as separate from coverage. Surface active work in the dashboard, where a spinner can represent a request in flight. Check metadata, top-level streams, and comment/review jobs for failures. Ensure a failed background refresh cannot be hidden by another running stream.

Keep the dashboard route and shell reachable during loading, sync errors, and access-check failures. Show an inline status or alert with the affected resource, available error reason, next retry time, and retry action when supported. Keep authorized content visible through ordinary sync failures. If authorization is unverified or revoked, retain the dashboard shell and withhold protected content and identity according to existing access rules.

Verify initial loading, partial coverage during background refresh, synchronized coverage, rate limiting, stopped and paused jobs, and a failed refetch with previously loaded data. Check both sidebar navigation and direct URLs, including a private repo losing access. Ensure indicators have accessible text labels.

## Implementation progress

The working tree contains static sidebar status indicators and inline dashboard errors. Access-unverified rows retain their dashboard links with a generic label. Worker changes try other eligible selectors on credential throttles and preserve child scan state on stabilization passes. The first pass of periodic reconciliation can still reopen completed children to recover missed events. Post-reset pacing and end-to-end verification remain pending.

The full plan is blocked on U1. Before U2 can switch private ingestion to direct lookup, verify the user and installation grants independently on an authorized disposable fixture. Record the lookup and installation resolution results in this plan before marking it implementation-ready.

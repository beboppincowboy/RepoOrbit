---
title: Sync worker and dashboard fixes
artifact_contract: ce-unified-plan/v1
artifact_readiness: implementation-ready
execution: code
---

## Scope

Ship the dashboard and worker fixes using existing fresh authorization. The live private grant test was skipped at the user's direction. Repository-specific authorization and post-reset pacing remain in the broader sync plan.

## Implementation

- Open repository dashboards from selected rows and direct URLs. Keep authorization failures in the dashboard and withhold protected data. Show red, yellow and green indicators for failed, partial and synchronized coverage.
- Preserve child checkpoints across repeated stabilization passes. Keep periodic reconciliation for missed source changes.
- Try other eligible selectors after a credential throttle and continue to honor shared installation cooldowns.
- Treat explicit GitHub pull-request disablement as disabled coverage. Preserve the post-fetch access and commit checks and verify the stopped DeepSeek job can finish after requeue.

## Verification and shipping

Review the existing diff, run backend and relevant database integration tests and frontend checks, and verify dashboard behavior with browser coverage. Base the PR on current main so the 13 separate issue-write commits in the working checkout stay out of the sync PR. Preserve local planning files and the scratch integration repro.

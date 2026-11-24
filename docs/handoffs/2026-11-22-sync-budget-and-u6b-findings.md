# Handoff: sync call budget and U6b review findings (2026-11-22)

**Worktree:** `feat-repository-access-and-ingestion` · **Branch:** `feat/front-end` at `7663b86` (U6b) · **Nothing committed this session.**

## Where things stand

| Area | State |
|---|---|
| Installation-token sync fix (from an earlier session) | Uncommitted in `backend/internal/githubapp/*` and `backend/internal/ingestion/*`. |
| One-page child shortcut (this session) | Done and uncommitted. Tests pass. |
| Repo-wide comment sync | Planned only: `docs/plans/2026-11-22-1516-perf-repo-wide-comment-sync-plan.md`. |
| U6b review findings (5) | Investigated and triaged. Not fixed. |
| Earlier issue-editor and API simplifications | Uncommitted in `apps/web/src/components/issue-editor.tsx`, `apps/web/src/lib/api.ts` and related files. They don't change behavior and fix none of the findings. |

Watch out:
- `docs/` is entirely untracked in this worktree, including all plans and this handoff. Commit it or it can be lost.
- `backend/internal/ingestion/zz_callcount_integration_test.go` (`TestScratchSyncPagesShareAccessListing`) is scratch-named. Rename it or fold it into `sync_integration_test.go` before committing.

---

## 1. Sync call budget

### Findings

- **Each page costs 5 calls:** 2 user-token repository checks (`checkActivityAccess` in `backend/internal/ingestion/refresh.go`), 2 app-JWT installation lookups and 1 data call. Sync therefore still spends the user's token budget on every page. The JWT lookups use a separate budget.
- **Every 15-minute poll re-runs every comment and review job.** `pollActivity` (`reconcile.go`) calls `SignalActivity` (`schedule.go`), which resets each stream with `previous_scan=NULL`. The issue and PR syncs then treat the scan as a first pass, and `registerChild` re-enqueues every finished child. The measured repo has about 205 issues and 126 PRs, so comments alone cost about 1,650 calls per poll, or about 6,600 an hour. That is over GitHub's 5,000-per-hour limit. This corrects an earlier claim that finished children are rarely re-polled.
- **Before the shortcut, every child ran at least twice.** A child counted as done only when two consecutive scans matched (`evidence_children.go`).
- **Comparing scans never deletes anything.** `previous_scan`/`current_scan` only decide when a scan is done. Comments are deleted only by signed webhooks (`activity_deletion.go`) and repository cleanup (`cleanup.go`).

### Verdicts on the original efficiency ideas

| Idea | Verdict |
|---|---|
| Repo-wide comments endpoint | Do it. Planned (below). |
| Cache the installation lookup | Skip. It uses the JWT budget, and the lookup after each fetch is what catches an uninstall mid-fetch (`zz_callcount` asserts this). |
| ETags / conditional requests | Not pursued. The fix is to stop re-queuing jobs, not to make repeated fetches cheaper. |
| One-page shortcut (new) | Done (below). |

### Work done: one-page shortcut

- **Change:** `backend/internal/ingestion/evidence_children.go`. A child scan that fits in one response (started with no cursor, no next page) is marked done on its first run. Multi-page scans still need two matching scans.
- **Test:** `backend/internal/ingestion/evidence_integration_test.go`. Comment and review children must complete in a single run.
- **Verified:** `go test -tags=integration ./internal/ingestion/` passes. With the shortcut disabled, the updated test fails.

### Plan: repo-wide comment sync

`docs/plans/2026-11-22-1516-perf-repo-wide-comment-sync-plan.md` (6 units). It replaces the per-issue and per-PR comment jobs with one repo-level stream on `GET /repos/{o}/{r}/issues/comments`, reusing `syncEventResource`.

Decisions settled with the user:
- Catch-ups list only comments changed since the last converged scan.
- Existing per-issue progress is dropped, and each repo rescans once.
- Reviews stay one job per PR.

Decisions made in the plan:
- A one-page catch-up converges on its first run (KTD3).
- Deletions bump the comment stream's generation (KTD4).
- The occasional full rescan is dropped (KTD6), because scan comparison never deletes rows. The user had approved it before this research.

Deferred follow-ups:
- Re-queue a review job only when its PR's `updated_at` changed. This is the next-largest per-poll cost.
- Set the watermark when a backfill completes, so the first catch-up doesn't rescan the whole window.

Review: done inline by the agent, not with reviewer subagents.

---

## 2. U6b review findings (commit `7663b86`)

Investigation: no PR (#1–#3) or commit addressed any of them. Verdicts below come from the current code.

Key fact: the backend refuses a second create with the same content only while the first is `pending` or `unknown` (index `write_operations_unresolved_create`, `backend/migrations/00015_write_operations.sql:32`). It does not refuse one after the first create succeeded.

| Priority | Finding | Investigated before? | Worth fixing | Fix |
|---|---|---|---|---|
| P1 | Reload during submit can create a duplicate issue | No | **Now** | Save the draft and key as `unknown` before sending (`issue-editor.tsx:289` skips saving while submitting). Clear on success. |
| P1 | Recovery errors discard the original key | Partly. Network failures and same-key responses keep the key (`issue-editor.tsx:584-585`, `641-645`) | **Now (cheap)** | When retrying with the kept key, keep it on `server` or `unverified` errors instead of moving to Failed (`issue-editor.tsx:646-649`). |
| P2 | Temporary access failures erase saved drafts | No | **Now** | Clear drafts only on confirmed access loss. Today any `RepositoryAccessUnverifiedError`, including rate limits, clears them (`workspace.$repositoryId.tsx:164`, `199-202`). Keep the private-draft wipe from `a772f83`. |
| P2 | Reloading an edit hides close/reopen for authors | No | Later | Load issue metadata when restoring (`workspace.$repositoryId.tsx:284-286`). Narrow impact, and the backend still allows the action. |
| P2 | Assignee suggestions include ineligible users (R12) | No | Not now | Needs a new assignees endpoint. Submission already checks each assignee (`backend/internal/issues/commands.go:642-650`). |

---

## Suggested next steps

1. Fix the three "now" U6b findings: editor and workspace route only, one test each.
2. Commit the installation-token fix and the one-page shortcut, with the scratch test renamed.
3. Run `ce-work` on the repo-wide comment sync plan.
4. Commit `docs/`.

## Verification run this session

- `go test -tags=integration ./internal/ingestion/` passed (Postgres at `localhost:54329`).
- Not run this session: the full backend suite, `make check`, web lint and typecheck, Playwright.

// Board staging (U6d): the five columns, staged cards in localStorage, and the pure derivation of each card's one
// audited update (KTD2) and its "changed on GitHub" check (KTD3). Free of React so specs can import it directly.
import type { BoardIssue, BoardSnapshot, BoardStatus, IssueUpdateDraft } from "./api";
import { boardDraftPrefix, issueDraftStorageKey, readStorageByPrefix, removeStorage, writeStorage } from "./drafts";

// The Board's columns, in order.
export const BOARD_STATUSES: readonly BoardStatus[] = ["backlog", "todo", "in_progress", "in_review", "done"];

// The status label that places an open issue in each labelled column. Backlog and Done have none.
export const STATUS_LABELS: Partial<Record<BoardStatus, string>> = { todo: "status:todo", in_progress: "status:in-progress", in_review: "status:review" };

// Whether label is one of the three status labels, in any spelling.
export function isStatusLabel(label: string): boolean {
  return Object.values(STATUS_LABELS).includes(label.toLowerCase());
}

// What the user staged on a card: a status and their own assignee edits (both lists empty when never touched), applied
// over whatever snapshot the card holds. assignedFromBacklog is set by stageAssignees when assigning moved the target
// from Backlog to Todo, so the review names that side effect.
export type BoardTarget = { status: BoardStatus; assigneesAdd: string[]; assigneesRemove: string[]; assignedFromBacklog?: boolean };

// pending exists only in the sending tab's memory: saveBoardCard stores it as unknown, so a reload mid-send offers
// Check again instead of a stuck pending card.
export type BoardCardPhase = "staged" | "changed" | "pending" | "unknown" | "failed" | "confirmed";

export type BoardCard = {
  version: 1;
  number: number;
  snapshot: BoardSnapshot;
  target: BoardTarget;
  phase: BoardCardPhase;
  // Stored before the send: the exact body and Idempotency-Key, which Check again resends unchanged. Null while staged.
  request: { draft: IssueUpdateDraft; idempotencyKey: string } | null;
  // The recorded operation, once a response named it.
  operationId: number | null;
};

// One card's update draft (with expect built from its snapshot) and the review's lines for what it does beyond the move.
export type BoardChange = { draft: IssueUpdateDraft; sideEffects: string[] };

// The saver is the signed-in user; assignable means they are in the repository's /assignees list.
export type BoardSaver = { login: string; assignable: boolean };

// Every staged card of one viewer and repository, in issue-number order.
export function loadBoardCards(viewerKey: string, repositoryId: string): BoardCard[] {
  return readStorageByPrefix<BoardCard>(boardDraftPrefix(viewerKey, repositoryId))
    .filter(card => card.version === 1)
    .sort((a, b) => a.number - b.number);
}

// Stores one card under its own key, writing a pending card as unknown (KTD8).
export function saveBoardCard(viewerKey: string, repositoryId: string, card: BoardCard): void {
  writeStorage(issueDraftStorageKey(viewerKey, repositoryId, { board: card.number }), card.phase === "pending" ? { ...card, phase: "unknown" } : card);
}

export function removeBoardCard(viewerKey: string, repositoryId: string, issueNumber: number): void {
  removeStorage(issueDraftStorageKey(viewerKey, repositoryId, { board: issueNumber }));
}

const same = (a: string, b: string) => a.toLowerCase() === b.toLowerCase();
const has = (list: string[], value: string) => list.some(item => same(item, value));
const sameSet = (a: string[], b: string[]) => a.every(v => has(b, v)) && b.every(v => has(a, v));

// Replaces the target's assignee edits. Newly adding someone while the target is Backlog moves the target to Todo
// (KTD2 rule 5); any other target status is left alone.
export function stageAssignees(target: BoardTarget, assigneesAdd: string[], assigneesRemove: string[]): BoardTarget {
  const assigning = target.status === "backlog" && assigneesAdd.some(login => !has(target.assigneesAdd, login));
  return assigning ? { status: "todo", assigneesAdd, assigneesRemove, assignedFromBacklog: true } : { ...target, assigneesAdd, assigneesRemove };
}

// Derives the one update that moves the issue from snapshot to target (KTD2), or null when nothing would change.
// Labels and logins compare case-insensitively, and removals use the issue's own spelling.
export function deriveChange(snapshot: BoardSnapshot, target: BoardTarget, saver: BoardSaver): BoardChange | null {
  const assigneesAdd = target.assigneesAdd.filter(login => !has(snapshot.assignees, login));
  const assigneesRemove = snapshot.assignees.filter(login => has(target.assigneesRemove, login));
  if (target.status === snapshot.status && !assigneesAdd.length && !assigneesRemove.length) return null;

  const sideEffects: string[] = [];
  const statusLabels = snapshot.labels.filter(isStatusLabel);
  const targetLabel = STATUS_LABELS[target.status];
  const closing = target.status === "done" && snapshot.state === "open";
  const reopening = target.status !== "done" && snapshot.state === "closed";
  if (closing) sideEffects.push("Closes the issue as completed");
  if (reopening) sideEffects.push("Reopens the issue");
  // Labels change only with a move, so an assignee-only edit never touches a flagged card's labels. A reopen always adds
  // the target label: the app's cleanup may strip it from the still-closed issue just before.
  const moving = target.status !== snapshot.status;
  const labelsAdd = moving && targetLabel && (reopening || !has(snapshot.labels, targetLabel)) ? [targetLabel] : [];
  const labelsRemove = moving ? statusLabels.filter(label => !targetLabel || !same(label, targetLabel)) : [];
  for (const label of labelsRemove) sideEffects.push(`Removes label ${label}`);
  if (target.status === "in_progress" && snapshot.status !== "in_progress" && !snapshot.assignees.length && !target.assigneesAdd.length && !target.assigneesRemove.length) {
    if (saver.assignable) {
      assigneesAdd.push(saver.login);
      sideEffects.push(`Assigns you (${saver.login})`);
    } else {
      sideEffects.push("Assignment skipped: GitHub won't accept you as an assignee");
    }
  }
  if (target.assignedFromBacklog && target.status === "todo" && snapshot.status !== "todo") sideEffects.push("Moves to Todo because an assignee was added");

  const delta = (add: string[], remove: string[]) => (add.length || remove.length ? { ...(add.length ? { add } : {}), ...(remove.length ? { remove } : {}) } : undefined);
  const labels = delta(labelsAdd, labelsRemove);
  const assignees = delta(assigneesAdd, assigneesRemove);
  const draft: IssueUpdateDraft = {
    ...(labels ? { labels } : {}),
    ...(assignees ? { assignees } : {}),
    ...(closing ? { state: "closed" as const } : reopening ? { state: "open" as const } : {}),
    expect: { status: snapshot.status, assignees: snapshot.assignees, statusLabels },
  };
  return { draft, sideEffects };
}

// Whether a staged card's issue changed on GitHub since its snapshot (KTD3): the row is newer and differs in Board
// status, assignee set or, for an open issue, status-label set. Only staged cards are checked; a closed issue's
// leftover-label cleanup is never a change.
export function isChangedOnGitHub(card: BoardCard, row: BoardIssue): boolean {
  const { snapshot } = card;
  if (card.phase !== "staged" || !(Date.parse(row.updatedAt) > Date.parse(snapshot.updatedAt))) return false;
  return row.status !== snapshot.status
    || !sameSet(row.assignees, snapshot.assignees)
    || (row.state === "open" && !sameSet(row.labels.filter(isStatusLabel), snapshot.labels.filter(isStatusLabel)));
}

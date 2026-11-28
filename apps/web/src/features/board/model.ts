import type { BoardIssue, RepositoryBoard } from "@/features/board/api";
import type { BoardSnapshot, IssueCapabilities, IssueUpdateDraft, WriteOperation } from "@/features/issues/api";
import { STATUS_LABELS, type BoardStatus } from "@/features/issues/status";
import { classifyWriteFailure } from "@/features/issues/write-errors";
import { hasIgnoreCase } from "@/features/issues/model";

// How many open and closed issues one Board read returns at most.
export const OPEN_CAP = 500;
export const CLOSED_CAP = 50;

export function isStatusLabel(label: string): boolean {
  return Object.values(STATUS_LABELS).includes(label.toLowerCase());
}

// What the user staged on a card: a status and their own assignee edits (both lists empty when never touched), applied
// over whatever snapshot the card holds. assignedFromBacklog is set by stageAssignees when assigning moved the target
// from Backlog to Todo, so the review names that side effect.
export type BoardTarget = { status: BoardStatus; assigneesAdd: string[]; assigneesRemove: string[]; assignedFromBacklog?: boolean };

export type BoardCardPhase = "staged" | "changed" | "pending" | "unknown" | "failed" | "confirmed";

export type BoardCard = {
  version: 1;
  number: number;
  snapshot: BoardSnapshot;
  target: BoardTarget;
  phase: BoardCardPhase;
  // Stored before the send: the exact body and Idempotency-Key, which Check again resends unchanged. Null while staged.
  request: { draft: IssueUpdateDraft; idempotencyKey: string } | null;
  operationId: number | null;
  // When the stored request was first sent (ISO 8601), which bounds the same-key replay window. Absent or null when no
  // request is held.
  attemptedAt?: string | null;
};

// What one send came back with: the operation GitHub's write path answered, or the thrown error. isReplay marks a resend
// of an already-used Idempotency-Key, where a refusal no longer proves the first send went unrecorded.
export type BoardSaveResult = { operation: WriteOperation } | { error: unknown; isReplay?: boolean };

// Where a card goes after a send or a recovery: "removed" deletes it from storage, "card" stores the card given.
export type BoardSaveOutcome = { kind: "removed" } | { kind: "card"; card: BoardCard };

export type BoardChange = { draft: IssueUpdateDraft; sideEffects: string[] };

// The saver is the signed-in user; assignable means they are in the repository's /assignees list.
export type BoardSaver = { login: string; assignable: boolean };

const same = (a: string, b: string) => a.toLowerCase() === b.toLowerCase();
const sameSet = (a: string[], b: string[]) => a.every(v => hasIgnoreCase(b, v)) && b.every(v => hasIgnoreCase(a, v));

export function stageAssignees(target: BoardTarget, assigneesAdd: string[], assigneesRemove: string[]): BoardTarget {
  const assigning = target.status === "backlog" && assigneesAdd.some(login => !hasIgnoreCase(target.assigneesAdd, login));
  return assigning ? { status: "todo", assigneesAdd, assigneesRemove, assignedFromBacklog: true } : { ...target, assigneesAdd, assigneesRemove };
}

export function deriveChange(snapshot: BoardSnapshot, target: BoardTarget, saver: BoardSaver): BoardChange | null {
  const assigneesAdd = target.assigneesAdd.filter(login => !hasIgnoreCase(snapshot.assignees, login));
  const assigneesRemove = snapshot.assignees.filter(login => hasIgnoreCase(target.assigneesRemove, login));
  if (target.status === snapshot.status && !assigneesAdd.length && !assigneesRemove.length) return null;

  const sideEffects: string[] = [];
  const statusLabels = snapshot.labels.filter(isStatusLabel);
  const targetLabel = STATUS_LABELS[target.status];
  const closing = target.status === "done" && snapshot.state === "open";
  const reopening = target.status !== "done" && snapshot.state === "closed";
  if (closing) sideEffects.push("Closes the issue as completed");
  if (reopening) sideEffects.push("Reopens the issue");
  const moving = target.status !== snapshot.status;
  const labelsAdd = moving && targetLabel && (reopening || !hasIgnoreCase(snapshot.labels, targetLabel)) ? [targetLabel] : [];
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

export function isChangedOnGitHub(card: BoardCard, row: BoardIssue): boolean {
  const { snapshot } = card;
  if (card.phase !== "staged" || !(Date.parse(row.updatedAt) > Date.parse(snapshot.updatedAt))) return false;
  return row.status !== snapshot.status
    || !sameSet(row.assignees, snapshot.assignees)
    || (row.state === "open" && !sameSet(row.labels.filter(isStatusLabel), snapshot.labels.filter(isStatusLabel)));
}

// Where a sent card goes next: by operation state when the send resolved, otherwise by how the error classifies.
// Contract: the save loop never sends or maps a card that is already unknown or pending.
// An unknown card keeps its request, key and attemptedAt for recovery; failed and changed cards drop them, so staging
// and saving again sends a new request under a new key.
export function applySaveResult(card: BoardCard, result: BoardSaveResult): BoardSaveOutcome {
  const unknown = (operationId: number | null): BoardSaveOutcome => ({ kind: "card", card: { ...card, phase: "unknown", operationId } });
  const released = (phase: "failed" | "changed", operationId: number | null, snapshot = card.snapshot, target = card.target): BoardSaveOutcome =>
    ({ kind: "card", card: { ...card, phase, snapshot, target, request: null, operationId, attemptedAt: null } });

  if ("operation" in result) {
    const { id, state } = result.operation;
    if (state === "succeeded") return { kind: "removed" };
    return state === "failed" ? released("failed", id) : unknown(id);
  }

  const failure = classifyWriteFailure(result.error, { isReplay: result.isReplay });
  // The card's earlier operation id belongs to this send only when the same key is being resent.
  const sameKeyId = result.isReplay ? card.operationId : null;
  switch (failure.outcome) {
    case "changed_on_github": {
      const target = card.target.status === card.snapshot.status ? { ...card.target, status: failure.current.status } : card.target;
      return released("changed", null, failure.current, target);
    }
    case "not_recorded":
      return released("failed", null);
    case "recheck_unresolved":
      return unknown(failure.operation?.id ?? sameKeyId);
    case "possibly_recorded":
    case "duplicate_create": // Only a create gets this; stay locked if it ever appears.
      return unknown(sameKeyId);
  }
}

export const locked = (card?: BoardCard) => card?.phase === "pending" || card?.phase === "unknown";

export const cardOf = (cards: readonly BoardCard[], issueNumber: number) => cards.find(card => card.number === issueNumber);

// Where the row's card is staged to go, or where the row is now when it holds no card.
export const targetOf = (row: BoardIssue, card: BoardCard | undefined): BoardTarget => card?.target ?? { status: row.status!, assigneesAdd: [], assigneesRemove: [] };

// The column that shows the row: a failed card stays where the row is.
export const columnOf = (row: BoardIssue, card: BoardCard | undefined) => !card || card.phase === "failed" ? row.status : card.target.status;

// The text filter as matchesFilters reads it: trimmed, lower-cased and without a leading #.
export const searchNeedle = (text: string) => text.trim().toLowerCase().replace(/^#/, "");

// Whether the row passes every filter; an empty filter passes all rows.
export function matchesFilters(row: BoardIssue, { assignee, label, needle }: { assignee: string; label: string; needle: string }): boolean {
  return (!assignee || row.assignees.includes(assignee))
    && (!label || row.labels.includes(label))
    && (!needle || row.title.toLowerCase().includes(needle) || String(row.number) === needle);
}

export const filterOptions = (values: string[]) => [...new Set(values)].sort((a, b) => a.localeCompare(b));

type BlockedReasonInput = {
  demo: boolean;
  board: RepositoryBoard | null;
  capabilities: IssueCapabilities | null;
  capabilitiesError: string | null;
};

// Why Board changes cannot be staged; null when they can.
export function getBlockedReason({ demo, board, capabilities, capabilitiesError }: BlockedReasonInput): string | null {
  if (demo) return "Demo mode — remote GitHub writes are disabled, so Board changes cannot be staged.";
  if (board && !board.statusLabels) return "No status source: turn status labels on to stage Board changes.";
  if (capabilitiesError !== null) return capabilitiesError;
  if (!capabilities) return "Checking your current GitHub permissions…";
  if (!capabilities.canManageIssues) return "Moving cards, changing assignees, and closing or reopening issues requires Triage or higher permission on GitHub.";
  return null;
}

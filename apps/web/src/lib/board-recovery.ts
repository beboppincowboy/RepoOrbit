import { IssueWriteError, canReplayWithSameKey, recheckWriteOperation, submitIssueUpdate } from "./api";
import { applySaveResult, type BoardCard, type BoardSaveOutcome } from "./board";

export type BoardRecoveryAction = "recheck" | "check-recorded-status" | "none";

export type BoardRecovery = {
  // Where the card goes now.
  outcome: BoardSaveOutcome;
  // What the server said went wrong; null when it reported no failure.
  error: string | null;
};

export type BoardRecoveryOptions = {
  repositoryId: string;
  now?: number;
  submit?: typeof submitIssueUpdate;
  recheck?: typeof recheckWriteOperation;
};

// How an unknown card can be resolved. With no operation id, the stored request can be resent under its stored key only
// inside the replay window. Otherwise the card offers nothing and stays locked.
export function recoveryAction(card: BoardCard, now: number = Date.now()): BoardRecoveryAction {
  if (card.phase !== "unknown") return "none";
  if (card.operationId !== null) return "recheck";
  return card.request && canReplayWithSameKey(card.attemptedAt, now) ? "check-recorded-status" : "none";
}

// Runs the action the card offers now. Resolves null, calling nothing, when it offers none. Storing the outcome is the
// caller's job.
export async function runRecovery(card: BoardCard, { repositoryId, now, submit = submitIssueUpdate, recheck = recheckWriteOperation }: BoardRecoveryOptions): Promise<BoardRecovery | null> {
  const action = recoveryAction(card, now);
  if (action === "none") return null;
  try {
    const operation = action === "recheck"
      ? await recheck(card.operationId!, card.number)
      : await submit(repositoryId, card.number, card.request!.idempotencyKey, card.request!.draft);
    const error = operation.state === "failed" ? operation.outcome?.error ?? "GitHub did not apply every requested change." : null;
    return { outcome: applySaveResult(card, { operation }), error };
  } catch (thrown) {
    const error = thrown instanceof Error ? thrown.message : "Could not reach GitHub.";
    if (action === "check-recorded-status") return { outcome: applySaveResult(card, { error: thrown, isReplay: true }), error };
    // A failed recheck says nothing about the original write, so the card stays as it is. An unresolved one may carry
    // the operation as the server now holds it.
    const operation = thrown instanceof IssueWriteError && thrown.kind === "recheck_unresolved" ? thrown.operation : null;
    return { outcome: operation ? applySaveResult(card, { operation }) : { kind: "card", card }, error };
  }
}

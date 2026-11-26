import { IssueWriteError, canReplayWithSameKey, recheckWriteOperation, submitIssueUpdate } from "./api";
import { applySaveResult, type BoardCard, type BoardSaveOutcome } from "./board";

// "recheck" asks GitHub's operation record about a send that has an operation id. "resend" sends the stored request again
// under its stored Idempotency-Key. "none" leaves the card locked.
export type BoardRecoveryAction = "recheck" | "resend" | "none";

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

// Which recovery an unknown card offers now. A card with an operation id is rechecked. A card without one can resend its
// stored request under its stored key, only inside the replay window (canReplayWithSameKey). Any other card offers none.
export function recoveryAction(card: BoardCard, now: number = Date.now()): BoardRecoveryAction {
  if (card.phase !== "unknown") return "none";
  if (card.operationId !== null) return "recheck";
  return card.request && canReplayWithSameKey(card.attemptedAt, now) ? "resend" : "none";
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
    if (action === "resend") return { outcome: applySaveResult(card, { error: thrown, isReplay: true }), error };
    // A failed recheck says nothing about the original write, so the card stays as it is. An unresolved one may carry
    // the operation as the server now holds it.
    const operation = thrown instanceof IssueWriteError && thrown.kind === "recheck_unresolved" ? thrown.operation : null;
    return { outcome: operation ? applySaveResult(card, { operation }) : { kind: "card", card }, error };
  }
}

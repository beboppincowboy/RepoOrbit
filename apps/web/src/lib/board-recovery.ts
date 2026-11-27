import { recheckWriteOperation, submitIssueUpdate } from "@/features/issues/api";
import { canReplayWithSameKey, WRITE_RECOVERY_WINDOW_MS } from "@/features/issues/write-errors";
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

// Whether the user may delete this card. A pending card may not. An unknown card may only when it has no operation id and
// its attemptedAt is older than the replay window, so nothing can check it any more.
export function canDiscardBoardCard(card: BoardCard, now: number = Date.now()): boolean {
  if (card.phase === "pending") return false;
  if (card.phase !== "unknown") return true;
  if (card.operationId !== null || typeof card.attemptedAt !== "string") return false;
  return now - Date.parse(card.attemptedAt) > WRITE_RECOVERY_WINDOW_MS;
}

// Runs the action the card offers now. Resolves null, calling nothing, when it offers none. Storing the outcome is the
// caller's job.
export async function runRecovery(card: BoardCard, { repositoryId, now, submit = submitIssueUpdate, recheck = recheckWriteOperation }: BoardRecoveryOptions): Promise<BoardRecovery | null> {
  const action = recoveryAction(card, now);
  if (action === "none") return null;
  try {
    const signal = AbortSignal.timeout(120_000);
    const operation = action === "recheck"
      ? await recheck(card.operationId!, card.number, signal)
      : await submit(repositoryId, card.number, card.request!.idempotencyKey, card.request!.draft, signal);
    const error = operation.state === "failed" ? operation.outcome?.error ?? "GitHub did not apply every requested change." : null;
    return { outcome: applySaveResult(card, { operation }), error };
  } catch (thrown) {
    const error = thrown instanceof Error ? thrown.message : "Could not reach GitHub.";
    if (action === "resend") return { outcome: applySaveResult(card, { error: thrown, isReplay: true }), error };
    // Leave the original write locked until a recheck returns an operation successfully.
    return { outcome: { kind: "card", card }, error };
  }
}

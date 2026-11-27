import { IssueWriteError, generateIdempotencyKey, submitIssueUpdate } from "./api";
import { applySaveResult, deriveChange, guardBoardCardWrites, loadBoardCard, removeBoardCard, type BoardCard, type BoardSaver, type BoardSaveResult } from "./board";
import { boardDraftPrefix } from "./drafts";

export type BoardSaveOptions = {
  viewerKey: string;
  repositoryId: string;
  // The cards as the review showed them.
  reviewed: readonly BoardCard[];
  saver: BoardSaver;
  // Called after each change to the stored cards.
  onChange: () => void;
  submit?: typeof submitIssueUpdate;
};

let saving = false;

const unchangedSinceReview = (card: BoardCard, reviewed: BoardCard) =>
  JSON.stringify([card.snapshot, card.target]) === JSON.stringify([reviewed.snapshot, reviewed.target]);

// Waits for the viewer and repository's Board write lock, then runs work. Runs unlocked if navigator.locks is absent.
export async function withBoardLock<T>(viewerKey: string, repositoryId: string, work: () => Promise<T>): Promise<T> {
  const locks = globalThis.navigator?.locks;
  return locks ? await locks.request(boardDraftPrefix(viewerKey, repositoryId), work) : work();
}

// Sends the reviewed cards as issue updates, one at a time in issue-number order. Each card is reloaded just before its
// turn and sent only if it is still staged as reviewed. Transport and session errors stop the batch.
// Resolves true if the save ran, or false if another save is already running in this tab.
export async function saveBoardChanges({ viewerKey, repositoryId, reviewed, saver, onChange, submit = submitIssueUpdate }: BoardSaveOptions): Promise<boolean> {
  if (saving) return false;
  saving = true;
  const stored = (issueNumber: number) => loadBoardCard(viewerKey, repositoryId, issueNumber);
  try {
    const guard = guardBoardCardWrites(viewerKey, repositoryId);
    return await withBoardLock(viewerKey, repositoryId, async () => {
      for (const seen of [...reviewed].sort((a, b) => a.number - b.number)) {
        if (guard.wasCleared()) break;
        const card = stored(seen.number);
        if (card?.phase !== "staged" || !unchangedSinceReview(card, seen)) continue;
        const change = deriveChange(card.snapshot, card.target, saver);
        if (!change) continue;

        const request = { draft: change.draft, idempotencyKey: generateIdempotencyKey() };
        const sending: BoardCard = { ...card, request, attemptedAt: new Date().toISOString() };
        // Stored as pending, which reads back as unknown, so a send cut off by a reload stays locked. Nothing is sent
        // unless the request reads back from storage.
        if (!guard.write({ ...sending, phase: "pending" })) {
          onChange();
          break;
        }
        if (stored(card.number)?.request?.idempotencyKey !== request.idempotencyKey) continue;
        onChange();
        if (guard.wasCleared()) break;

        let result: BoardSaveResult;
        try {
          result = { operation: await submit(repositoryId, card.number, request.idempotencyKey, request.draft, AbortSignal.timeout(120_000)) };
        } catch (error) {
          result = { error };
        }
        // A late result must respect storage cleared during the send or a recovery that recorded an operation id.
        const current = stored(card.number);
        if (current?.request?.idempotencyKey === request.idempotencyKey && (!("error" in result) || current.operationId === null)) {
          const outcome = applySaveResult(sending, result);
          if (outcome.kind === "removed") removeBoardCard(viewerKey, repositoryId, card.number);
          const cleared = outcome.kind === "card" && !guard.write(outcome.card);
          onChange();
          if (cleared) break;
        }
        if ("error" in result) {
          const { error } = result;
          if (!(error instanceof IssueWriteError) || error.kind === "transport" || error.kind === "unauthenticated") break;
        }
      }
      return true;
    });
  } finally {
    saving = false;
  }
}

import { generateIdempotencyKey, submitIssueUpdate } from "./api";
import { applySaveResult, deriveChange, loadBoardCard, removeBoardCard, saveBoardCard, type BoardCard, type BoardSaver, type BoardSaveResult } from "./board";

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

// Sends the reviewed cards as issue updates, one at a time in issue-number order. Each card is reloaded just before its
// turn and sent only if it is still staged as reviewed. Resolves true once every reviewed card has been sent or skipped, and
// false, sending nothing, when another save is already running.
export async function saveBoardChanges({ viewerKey, repositoryId, reviewed, saver, onChange, submit = submitIssueUpdate }: BoardSaveOptions): Promise<boolean> {
  if (saving) return false;
  saving = true;
  const stored = (issueNumber: number) => loadBoardCard(viewerKey, repositoryId, issueNumber);
  try {
    for (const seen of [...reviewed].sort((a, b) => a.number - b.number)) {
      const card = stored(seen.number);
      if (card?.phase !== "staged" || !unchangedSinceReview(card, seen)) continue;
      const change = deriveChange(card.snapshot, card.target, saver);
      if (!change) continue;

      const request = { draft: change.draft, idempotencyKey: generateIdempotencyKey() };
      const sending: BoardCard = { ...card, request, attemptedAt: new Date().toISOString() };
      // Stored as pending, which reads back as unknown, so a send cut off by a reload stays locked. Nothing is sent
      // unless the request reads back from storage.
      const holdsRequest = () => stored(card.number)?.request?.idempotencyKey === request.idempotencyKey;
      saveBoardCard(viewerKey, repositoryId, { ...sending, phase: "pending" });
      if (!holdsRequest()) continue;
      onChange();

      let result: BoardSaveResult;
      try {
        result = { operation: await submit(repositoryId, card.number, request.idempotencyKey, request.draft) };
      } catch (error) {
        result = { error };
      }
      // Signing out or losing access clears the cards while a send is in flight; its result must not bring one back.
      if (!holdsRequest()) continue;
      const outcome = applySaveResult(sending, result);
      if (outcome.kind === "removed") removeBoardCard(viewerKey, repositoryId, card.number);
      else saveBoardCard(viewerKey, repositoryId, outcome.card);
      onChange();
    }
    return true;
  } finally {
    saving = false;
  }
}

import { cardOf, type BoardCard } from "@/features/board/model";
import { boardDraftPrefix, issueDraftStorageKey } from "@/features/issues/drafts";
import { readStorageByPrefix, removeStorage, writeStorage } from "@/shared/utils/storage";

// Every staged card of one viewer and repository, in issue-number order.
export function loadBoardCards(viewerKey: string, repositoryId: string): BoardCard[] {
  return readStorageByPrefix<BoardCard>(boardDraftPrefix(viewerKey, repositoryId))
    .filter(card => card.version === 1)
    .sort((a, b) => a.number - b.number);
}

// The stored card for one issue number, read from storage now (undefined when none is staged).
export function loadBoardCard(viewerKey: string, repositoryId: string, issueNumber: number): BoardCard | undefined {
  return cardOf(loadBoardCards(viewerKey, repositoryId), issueNumber);
}

// Callers hold the draft storage lock.
export function saveBoardCard(viewerKey: string, repositoryId: string, card: BoardCard): boolean {
  return writeStorage(issueDraftStorageKey(viewerKey, repositoryId, { board: card.number }), card.phase === "pending" ? { ...card, phase: "unknown" } : card);
}

// Callers hold the draft storage lock.
export function removeBoardCard(viewerKey: string, repositoryId: string, issueNumber: number): void {
  removeStorage(issueDraftStorageKey(viewerKey, repositoryId, { board: issueNumber }));
}

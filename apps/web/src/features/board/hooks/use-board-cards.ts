import { useEffect, useRef, useState, useSyncExternalStore } from "react";
import type { BoardIssue } from "@/features/board/api";
import { loadBoardCard, loadBoardCards, removeBoardCard, saveBoardCard } from "@/features/board/card-storage";
import { useBoardQuery, useStatusLabelRefetch } from "@/features/board/hooks/use-board-query";
import { cardOf, deriveChange, getBlockedReason, locked, type BoardCard, type BoardSaver, type BoardTarget } from "@/features/board/model";
import { canDiscardBoardCard, recoveryAction, runRecovery } from "@/features/board/recovery";
import { saveBoardChanges, withBoardLock } from "@/features/board/save";
import type { IssueCapabilities } from "@/features/issues/api";
import { boardDraftPrefix, withDraftStorageLock } from "@/features/issues/drafts";
import { viewerKeyOf } from "@/features/repositories/access-store";
import type { ViewerSession } from "@/shared/api/transport";

// The storage event only reaches other tabs. This one refreshes the Board in this tab, including after a remount.
const CARDS_SAVED = "repoorbit:board-cards-saved";

// Whether a save or a recovery started in this tab is still running. It outlives the Board, so one mounted again
// meanwhile stays busy.
let saveRunning = false;
const SAVE_RUNNING = "repoorbit:board-save-running";
function setSaveRunning(running: boolean) {
  saveRunning = running;
  window.dispatchEvent(new Event(SAVE_RUNNING));
}
function subscribeSaveRunning(onChange: () => void) {
  window.addEventListener(SAVE_RUNNING, onChange);
  return () => window.removeEventListener(SAVE_RUNNING, onChange);
}

type BoardCardsOptions = {
  session: ViewerSession;
  repositoryId: string;
  openIssue: number | undefined;
  saver: BoardSaver;
  capabilities: IssueCapabilities | null;
  capabilitiesError: string | null;
  // Called after a save or a recovery that may have written to GitHub.
  onGitHubWrite: () => void;
};

// The staged cards of one viewer and repository with the Board read they sit on, and what stages, discards, saves and
// recovers them.
export function useBoardCards({ session, repositoryId, openIssue, saver, capabilities, capabilitiesError, onGitHubWrite }: BoardCardsOptions) {
  const viewerKey = viewerKeyOf(session);
  const demo = session.mode === "demo";
  const [cards, setCards] = useState(() => loadBoardCards(viewerKey, repositoryId));
  const pendingStages = useRef(0);
  const [saveNotice, setSaveNotice] = useState<string | null>(null);
  const saving = useSyncExternalStore(subscribeSaveRunning, () => saveRunning, () => false);
  const { query, board, labelsSetting } = useBoardQuery({ session, repositoryId, openIssue, cards, setCards });
  const refetchBoard = query.refetch;
  useEffect(() => {
    const reload = () => {
      if (pendingStages.current === 0) setCards(loadBoardCards(viewerKey, repositoryId));
    };
    const onStorage = (event: StorageEvent) => {
      reload();
      if (event.newValue === null && event.key?.startsWith(boardDraftPrefix(viewerKey, repositoryId))) void refetchBoard();
    };
    window.addEventListener("storage", onStorage);
    window.addEventListener(CARDS_SAVED, reload);
    return () => {
      window.removeEventListener("storage", onStorage);
      window.removeEventListener(CARDS_SAVED, reload);
    };
  }, [viewerKey, repositoryId, refetchBoard]);
  useStatusLabelRefetch(labelsSetting, board?.statusLabels, refetchBoard);

  const blocked = getBlockedReason({ demo, board, capabilities, capabilitiesError });
  // Recheck only reads GitHub, so it needs no write permission. Resending writes.
  const recoveryBlocked = (card: BoardCard | undefined) => !demo && card && recoveryAction(card) === "recheck" ? null : blocked;

  // Sends the reviewed cards, then closes the review.
  async function confirmSave(reviewed: BoardCard[], closeReview: () => void) {
    if (saveRunning || blocked) return;
    setSaveRunning(true);
    try {
      let ran = true;
      let failed = false;
      try {
        ran = await saveBoardChanges({ viewerKey, repositoryId, reviewed, saver, onChange: () => window.dispatchEvent(new Event(CARDS_SAVED)) });
      } catch {
        failed = true;
      }
      if (ran) {
        onGitHubWrite();
        await refetchBoard();
        const current = loadBoardCards(viewerKey, repositoryId);
        if (pendingStages.current === 0) setCards(current);
        const unsent = current.filter(card => card.phase === "staged" && reviewed.some(seen => seen.number === card.number));
        if (unsent.length) {
          setSaveNotice(`${unsent.map(card => `#${card.number}`).join(", ")} ${unsent.length === 1 ? "was not sent and is" : "were not sent and are"} still staged.`);
        } else if (failed) {
          setSaveNotice("The save stopped unexpectedly. Check each card before saving again.");
        }
      }
    } finally {
      setSaveRunning(false);
      closeReview();
    }
  }

  // Resolves an unknown card, one send at a time with the save. Resolves the failure to show, or null.
  async function recover(issueNumber: number): Promise<string | null> {
    if (saveRunning || recoveryBlocked(cardOf(cards, issueNumber))) return null;
    setSaveRunning(true);
    try {
      const result = await withBoardLock(viewerKey, repositoryId, async () => {
        const stored = () => loadBoardCard(viewerKey, repositoryId, issueNumber);
        const card = await withDraftStorageLock(stored);
        if (!card || recoveryBlocked(card)) return null;
        const recovery = await runRecovery(card, { repositoryId });
        if (!recovery) return null;
        return withDraftStorageLock(() => {
          const current = stored();
          // Signing out, losing access or another tab may settle the card meanwhile.
          if (current?.phase !== "unknown" || current.operationId !== card.operationId
            || current.request?.idempotencyKey !== card.request?.idempotencyKey) return null;
          if (recovery.outcome.kind === "removed") removeBoardCard(viewerKey, repositoryId, issueNumber);
          else if (recovery.outcome.card !== card) saveBoardCard(viewerKey, repositoryId, recovery.outcome.card);
          return recovery;
        });
      });
      if (!result) return null;
      window.dispatchEvent(new Event(CARDS_SAVED));
      onGitHubWrite();
      await refetchBoard();
      return result.error;
    } catch {
      return "Could not check this change. Reload the page and try again.";
    } finally {
      setSaveRunning(false);
      // Also redraws a card whose action was no longer offered when it was pressed.
      window.dispatchEvent(new Event(CARDS_SAVED));
    }
  }

  async function stage(row: BoardIssue, target: BoardTarget) {
    if (blocked || !row.status) return;
    const card = cardOf(cards, row.number);
    if (locked(card)) return;
    setSaveNotice(null);
    const snapshot = card?.snapshot ?? { state: row.state, labels: row.labels, assignees: row.assignees, status: row.status, updatedAt: row.updatedAt };
    const next: BoardCard | null = deriveChange(snapshot, target, saver)
      ? { version: 1, number: row.number, snapshot, target, phase: "staged", request: null, operationId: null }
      : null;
    pendingStages.current++;
    setCards(current => {
      const remaining = current.filter(item => item.number !== row.number);
      if (next) remaining.push(next);
      return remaining.sort((a, b) => a.number - b.number);
    });
    try {
      await withDraftStorageLock(() => {
        const stored = loadBoardCard(viewerKey, repositoryId, row.number);
        if (locked(stored) || JSON.stringify(stored) !== JSON.stringify(card)) return;
        if (next) saveBoardCard(viewerKey, repositoryId, next);
        else removeBoardCard(viewerKey, repositoryId, row.number);
      });
    } finally {
      // Keep newer optimistic gestures visible until their writes have also been checked.
      if (--pendingStages.current === 0) window.dispatchEvent(new Event(CARDS_SAVED));
    }
  }

  async function discard(issueNumber: number) {
    const seen = JSON.stringify(cardOf(cards, issueNumber));
    const discarded = await withDraftStorageLock(() => {
      const card = loadBoardCard(viewerKey, repositoryId, issueNumber);
      // Another tab may have replaced the card since the user chose to discard it.
      if (!card || JSON.stringify(card) !== seen || !canDiscardBoardCard(card)) return false;
      removeBoardCard(viewerKey, repositoryId, issueNumber);
      return true;
    });
    if (discarded) setSaveNotice(null);
    window.dispatchEvent(new Event(CARDS_SAVED));
  }

  return { cards, query, board, blocked, recoveryBlocked, saving, saveNotice, setSaveNotice, stage, discard, confirmSave, recover };
}

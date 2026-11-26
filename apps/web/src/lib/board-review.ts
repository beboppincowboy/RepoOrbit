import type { BoardIssue, BoardStatus } from "./api";
import { deriveChange, type BoardCard, type BoardSaver } from "./board";

export type BoardReviewItem = { number: number; title: string; change: string; sideEffects: string[] };

type SaveGate = { blocked: string | null; saving: boolean };

// The cards a save would send, in issue-number order.
export function reviewableCards(cards: readonly BoardCard[]): BoardCard[] {
  return cards.filter(card => card.phase === "staged").sort((a, b) => a.number - b.number);
}

// What the review lists for each card. names holds each Board column's name.
export function reviewItems(reviewed: readonly BoardCard[], rows: readonly BoardIssue[], saver: BoardSaver, names: Record<BoardStatus, string>): BoardReviewItem[] {
  return reviewed.map(({ number, snapshot, target }) => ({
    number,
    title: rows.find(row => row.number === number)?.title ?? "Issue not on the Board",
    change: target.status === snapshot.status ? `${names[snapshot.status]} · assignee change` : `${names[snapshot.status]} → ${names[target.status]}`,
    sideEffects: deriveChange(snapshot, target, saver)?.sideEffects ?? [],
  }));
}

// The Save button's state. A blocked Board gives the same reason as the staging controls.
export function saveButtonState({ staged, blocked, saving }: SaveGate & { staged: number }): { disabled: boolean; title: string | undefined } {
  return {
    disabled: blocked !== null || saving || staged === 0,
    title: blocked ?? (saving ? "Saving staged changes…" : staged === 0 ? "No staged changes to save." : undefined),
  };
}

export function confirmDisabled({ blocked, saving }: SaveGate): boolean {
  return blocked !== null || saving;
}

import type { BoardIssue } from "@/features/board/api";
import { mini } from "@/features/board/components/board-styles";
import type { BoardCard, BoardCardPhase } from "@/features/board/model";
import { STATUS_NAMES } from "@/features/issues/status";

const phaseNames: Record<BoardCardPhase, string> = { staged: "Staged", changed: "Changed on GitHub", pending: "Pending", unknown: "Unknown", failed: "Failed", confirmed: "Confirmed" };

// The staged change's phase, shown on its row or card; null when the issue holds no card.
export function phaseBadge(row: BoardIssue, card: BoardCard | undefined) {
  if (!card) return null;
  const moved = row.status && card.target.status !== row.status;
  const where = !moved ? "" : card.phase === "failed" ? ` · keeps ${STATUS_NAMES[card.target.status]}` : ` · from ${STATUS_NAMES[row.status!]}`;
  return <span className={`${mini} ${card.phase === "failed" ? "border-destructive bg-destructive-soft text-destructive" : "border-primary bg-primary-soft text-primary"}`}>{phaseNames[card.phase]}{where}</span>;
}

// The issue title as the button that opens its task drawer.
export function openButton(row: BoardIssue, className: string, onOpenIssue: (issueNumber: number) => void) {
  return <button type="button" onClick={() => onOpenIssue(row.number)} className={`text-left font-medium hover:underline ${className}`}>{row.title}</button>;
}

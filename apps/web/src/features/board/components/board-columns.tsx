import { flushSync } from "react-dom";
import type { BoardIssue } from "@/features/board/api";
import { mini, muted, panel } from "@/features/board/components/board-styles";
import { openButton, phaseBadge } from "@/features/board/components/card-parts";
import { cardOf, columnOf, locked, targetOf, type BoardCard, type BoardTarget } from "@/features/board/model";
import { BOARD_STATUSES, STATUS_NAMES, type BoardStatus } from "@/features/issues/status";

// Runs a drop as a View Transition so the card glides to its new column. Only drops use it: the transition applies
// the update a frame later, which would make the drawer's controlled inputs flicker. Browsers without the API, and viewers
// who prefer reduced motion, get the update at once.
function withCardMotion(update: () => void) {
  if (!document.startViewTransition || matchMedia("(prefers-reduced-motion: reduce)").matches) return update();
  document.startViewTransition(() => flushSync(update));
}

type BoardColumnsProps = {
  // Every row of the Board read, where a dropped card is looked up.
  rows: BoardIssue[];
  // The filtered rows.
  shown: BoardIssue[];
  cards: BoardCard[];
  // Why cards cannot be dragged or dropped; null when they can.
  blocked: string | null;
  dragging: BoardIssue | undefined;
  setDragging: (row: BoardIssue | undefined) => void;
  dropTarget: BoardStatus | undefined;
  setDropTarget: (status: BoardStatus | undefined) => void;
  // Whether a cap left open or closed issues out, which marks the column counts.
  openCapped: boolean | undefined;
  closedCapped: boolean | undefined;
  // Stages a dropped card; nothing is sent to GitHub.
  onStage: (row: BoardIssue, target: BoardTarget) => void;
  onOpenIssue: (issueNumber: number) => void;
};

// One column per status, with native HTML5 drag and drop between them.
export function BoardColumns({ rows, shown, cards, blocked, dragging, setDragging, dropTarget, setDropTarget, openCapped, closedCapped, onStage, onOpenIssue }: BoardColumnsProps) {
  return (
    <div className="flex min-h-0 flex-1 gap-3 overflow-x-auto">
      {BOARD_STATUSES.map(status => {
        const items = shown.filter(row => columnOf(row, cardOf(cards, row.number)) === status);
        const isDropTarget = dropTarget === status && dragging !== undefined && columnOf(dragging, cardOf(cards, dragging.number)) !== status;
        return (
          <section
            key={status}
            className={`${panel} flex min-w-[210px] flex-1 flex-col transition-[border-color,background-color,box-shadow] duration-150 ${isDropTarget ? "border-primary bg-primary-soft shadow-[inset_0_0_0_1px_var(--primary)]" : ""}`}
            onDragOver={e => {
              if (blocked) return;
              e.preventDefault();
              if (dropTarget !== status) setDropTarget(status);
            }}
            onDragLeave={e => { if (!e.currentTarget.contains(e.relatedTarget as Node | null)) setDropTarget(undefined); }}
            onDrop={e => {
              e.preventDefault();
              setDropTarget(undefined);
              const row = rows.find(r => r.number === Number(e.dataTransfer.getData("text/plain")));
              if (row) withCardMotion(() => { onStage(row, { ...targetOf(row, cardOf(cards, row.number)), status }); });
            }}
          >
            <h3 className="flex flex-none items-center justify-between gap-2 px-3 pt-3 pb-2 text-[13px]">
              {STATUS_NAMES[status]}<span className={`${mini} border-border ${muted}`}>{items.length}{(status === "done" ? closedCapped : openCapped) && "+"}</span>
            </h3>
            <ul aria-label={STATUS_NAMES[status]} className="m-0 flex min-h-0 flex-1 list-none flex-col gap-2 overflow-y-auto px-2 pb-2">
              {items.map(row => (
                <li
                  key={row.number}
                  data-issue-card={row.number}
                  draggable={!blocked && !locked(cardOf(cards, row.number))}
                  onDragStart={e => {
                    e.dataTransfer.setData("text/plain", String(row.number));
                    e.dataTransfer.effectAllowed = "move";
                    setDragging(row);
                  }}
                  onDragEnd={() => { setDragging(undefined); setDropTarget(undefined); }}
                  style={{ viewTransitionName: `board-card-${row.number}` }}
                  className={`flex flex-col gap-1.5 rounded-lg border border-border bg-card p-2.5 transition-[border-color,box-shadow,opacity,transform] duration-150 hover:border-border-strong hover:shadow-sm motion-safe:hover:-translate-y-px [&[draggable=true]]:cursor-grab [&[draggable=true]]:active:cursor-grabbing ${dragging?.number === row.number ? "scale-[.98] opacity-40" : ""}`}
                >
                  {openButton(row, "leading-snug", onOpenIssue)}
                  <div className="flex flex-wrap items-center gap-1.5 text-xs">
                    <span className={`font-mono ${muted}`}>#{row.number}</span>
                    {row.assignees.length > 0 && <span className="text-soft">{row.assignees.map(a => `@${a}`).join(", ")}</span>}
                    {row.multipleStatus && <span className={`${mini} border-warning bg-warning-soft text-warning`}>Multiple status labels</span>}
                    {phaseBadge(row, cardOf(cards, row.number))}
                  </div>
                </li>
              ))}
            </ul>
          </section>
        );
      })}
    </div>
  );
}

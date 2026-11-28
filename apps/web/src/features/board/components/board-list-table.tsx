import { createColumnHelper, tableFeatures, useTable } from "@tanstack/react-table";
import type { BoardIssue } from "@/features/board/api";
import { mini, muted, panel } from "@/features/board/components/board-styles";
import { openButton, phaseBadge } from "@/features/board/components/card-parts";
import { cardOf, columnOf, type BoardCard } from "@/features/board/model";
import { STATUS_NAMES } from "@/features/issues/status";

const th = "sticky top-0 z-[1] whitespace-nowrap border-b border-border bg-card px-3 py-2 text-left text-xs font-medium text-muted-foreground";
const features = tableFeatures({});
const column = createColumnHelper<typeof features, BoardIssue>();

type BoardListTableProps = {
  // The filtered rows.
  shown: BoardIssue[];
  cards: BoardCard[];
  // Whether the Status column is shown.
  statusLabels: boolean;
  onOpenIssue: (issueNumber: number) => void;
};

export function BoardListTable({ shown, cards, statusLabels, onOpenIssue }: BoardListTableProps) {
  const columns = column.columns([
    column.accessor("number", { header: "Issue", cell: c => <span className={`font-mono text-xs ${muted}`}>#{c.getValue()}</span> }),
    column.accessor("title", { header: "Title", cell: c => openButton(c.row.original, "", onOpenIssue) }),
    ...(statusLabels ? [column.display({ id: "status", header: "Status", cell: c => {
      const card = cardOf(cards, c.row.original.number);
      const status = columnOf(c.row.original, card);
      return <span className="flex flex-wrap items-center gap-1 text-xs">{status && STATUS_NAMES[status]}{c.row.original.multipleStatus && <span className={`${mini} border-warning bg-warning-soft text-warning`}>Multiple status labels</span>}{phaseBadge(c.row.original, card)}</span>;
    } })] : []),
    column.accessor("labels", { header: "Labels", cell: c => <div className="flex flex-wrap gap-1">{c.getValue().map(name => <span key={name} className={`${mini} border-border bg-muted text-soft`}>{name}</span>)}</div> }),
    column.accessor("assignees", { header: "Assignees", cell: c => <span className="text-xs text-soft">{c.getValue().length ? c.getValue().map(a => `@${a}`).join(", ") : <span className={muted}>Unassigned</span>}</span> }),
  ]);
  const table = useTable({ features, columns, data: shown });

  return (
    <section aria-label="Board list" className={`${panel} min-h-0 flex-[1.35_1_480px] overflow-y-auto`}>
      <table className="w-full border-collapse">
        <thead>{table.getHeaderGroups().map(group => <tr key={group.id}>{group.headers.map(h => <th key={h.id} className={th}><table.FlexRender header={h} /></th>)}</tr>)}</thead>
        <tbody>
          {table.getRowModel().rows.map(row => (
            <tr key={row.id} data-issue-card={row.original.number} className="hover:bg-muted">
              {row.getAllCells().map(cell => <td key={cell.id} className="border-b border-border px-3 py-[9px] align-middle"><table.FlexRender cell={cell} /></td>)}
            </tr>
          ))}
          {!shown.length && <tr><td colSpan={columns.length} className={`p-6 text-center ${muted}`}>No issues match.</td></tr>}
        </tbody>
      </table>
    </section>
  );
}

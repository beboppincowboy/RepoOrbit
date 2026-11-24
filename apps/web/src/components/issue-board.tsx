import { useEffect, useMemo, useState, type ReactNode } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { createColumnHelper, tableFeatures, useTable } from "@tanstack/react-table";
import { Button } from "@/components/ui/button";
import { StatusSourceSetup } from "@/components/status-source-setup";
import { STATUS_NAMES, TaskDrawer } from "@/components/task-drawer";
import { statusLabelsQueryKey } from "@/components/write-history";
import { BOARD_STATUSES, deriveChange, loadBoardCards, removeBoardCard, saveBoardCard, type BoardCard, type BoardCardPhase, type BoardTarget } from "@/lib/board";
import { GraphQLUnavailableError, RepositoryAccessUnverifiedError, fetchRepositoryBoard, fetchStatusLabels, type BoardIssue, type IssueCapabilities, type ViewerSession } from "@/lib/api";
import { currentAccessSeq, recordAccessDenied, recordAccessUnverified, recordAccessVerified, selectRowVisibility, useSelectionVisibility } from "@/lib/selected-repositories";

// The workspace route's class strings.
const panel = "rounded-xl border border-border bg-card min-w-0 min-h-0";
const muted = "text-muted-foreground";
const mini = "inline-flex items-center gap-1 whitespace-nowrap rounded-full border px-[7px] text-[11px]";
const th = "sticky top-0 z-[1] whitespace-nowrap border-b border-border bg-card px-3 py-2 text-left text-xs font-medium text-muted-foreground";
const field = "rounded-lg border border-border bg-card px-2 py-1 text-xs";

const phaseNames: Record<BoardCardPhase, string> = { staged: "Staged", changed: "Changed on GitHub", pending: "Pending", unknown: "Unknown", failed: "Failed", confirmed: "Confirmed" };
const features = tableFeatures({});
const column = createColumnHelper<typeof features, BoardIssue>();
const noRows: BoardIssue[] = [];
// The Board read's caps (KTD7): the most recently updated open issues and the most recently closed ones.
const OPEN_CAP = 500;
const CLOSED_CAP = 50;
const locked = (card?: BoardCard) => card?.phase === "pending" || card?.phase === "unknown";

type IssueBoardProps = {
  session: ViewerSession;
  repositoryId: string;
  viewerKey: string;
  viewerLogin: string | null;
  demo: boolean;
  github: string;
  capabilities: IssueCapabilities | null;
  capabilitiesError: string | null;
  // Logins GitHub accepts as assignees; the saver self-assigns on a move to In Progress only when listed.
  assignable: string[];
  // The issue whose drawer is open (the route's issue= search parameter).
  openIssue: number | undefined;
  onOpenIssue: (issueNumber: number | undefined) => void;
};

// The repository's issues by status in Board and List views, with shared filters and the task drawer. Moves, assignee
// edits and close/reopen are only staged on the card, in this browser; nothing reaches GitHub until Save.
export function IssueBoard({ session, repositoryId, viewerKey, viewerLogin, demo, github, capabilities, capabilitiesError, assignable, openIssue, onOpenIssue }: IssueBoardProps) {
  const queryClient = useQueryClient();
  const visibility = useSelectionVisibility(session);
  const rowVisibility = selectRowVisibility([], visibility, repositoryId);
  const [cards, setCards] = useState(() => loadBoardCards(viewerKey, repositoryId));
  const [view, setView] = useState<"board" | "list">("board");
  const [assignee, setAssignee] = useState("");
  const [label, setLabel] = useState("");
  const [text, setText] = useState("");
  // Another tab staged or discarded a card.
  useEffect(() => {
    const reload = () => setCards(loadBoardCards(viewerKey, repositoryId));
    window.addEventListener("storage", reload);
    return () => window.removeEventListener("storage", reload);
  }, [viewerKey, repositoryId]);

  const query = useQuery({
    queryKey: ["repositoryBoard", viewerKey, repositoryId],
    // Staged cards and an opened issue may fall outside the caps, so the read includes them (the API takes at most 100).
    queryFn: async ({ signal }) => {
      const startedAtSeq = currentAccessSeq();
      try {
        const result = await fetchRepositoryBoard(
          repositoryId,
          [...new Set([...(openIssue ? [openIssue] : []), ...cards.map(card => card.number)])].slice(0, 100),
          signal,
        );
        signal.throwIfAborted();
        if (result === null) {
          recordAccessDenied(queryClient, session, repositoryId);
          setCards([]);
        } else {
          recordAccessVerified(session, repositoryId, startedAtSeq);
        }
        return result;
      } catch (error) {
        if (!signal.aborted && error instanceof RepositoryAccessUnverifiedError) {
          recordAccessUnverified(queryClient, session, repositoryId, error.retryAt, { preserveSnapshot: Boolean(error.retryAt) });
        }
        throw error;
      }
    },
    gcTime: 0,
    retry: (count, error) => !(error instanceof RepositoryAccessUnverifiedError || error instanceof GraphQLUnavailableError) && count < 1,
  });
  const accessBlocked = rowVisibility.availability === "denied" || rowVisibility.availability === "unverified";
  // Immediately hide stale Board rows, filter options, staging controls, and any open TaskDrawer on refetch error,
  // authoritative null, or a newer Selection access rejection.
  const board = query.isError || query.isPending || accessBlocked ? null : (query.data ?? null);
  const rows = board?.issues ?? noRows;
  // StatusSourceSetup shares this query; once it turns labels on or off, read the Board again.
  const labelsSetting = useQuery({ queryKey: statusLabelsQueryKey(viewerKey, repositoryId), queryFn: ({ signal }) => fetchStatusLabels(repositoryId, signal), enabled: false, gcTime: 0 }).data?.enabled;
  const boardLabels = board?.statusLabels;
  const refetchBoard = query.refetch;
  useEffect(() => {
    if (labelsSetting !== undefined && boardLabels !== undefined && labelsSetting !== boardLabels) void refetchBoard();
  }, [labelsSetting, boardLabels, refetchBoard]);

  const needle = text.trim().toLowerCase().replace(/^#/, "");
  const shown = useMemo(() => rows.filter(row => (!assignee || row.assignees.includes(assignee))
    && (!label || row.labels.includes(label))
    && (!needle || row.title.toLowerCase().includes(needle) || String(row.number) === needle)), [rows, assignee, label, needle]);
  const options = (values: string[]) => [...new Set(values)].sort((a, b) => a.localeCompare(b));

  const cardOf = (issueNumber: number) => cards.find(card => card.number === issueNumber);
  const targetOf = (row: BoardIssue): BoardTarget => cardOf(row.number)?.target ?? { status: row.status!, assigneesAdd: [], assigneesRemove: [] };
  // A staged card shows where it is going; a failed one stays where GitHub has it.
  const columnOf = (row: BoardIssue) => {
    const card = cardOf(row.number);
    return !card || card.phase === "failed" ? row.status : card.target.status;
  };
  const saver = { login: viewerLogin ?? "", assignable: assignable.some(login => login.toLowerCase() === viewerLogin?.toLowerCase()) };
  const blocked = demo ? "Demo mode — remote GitHub writes are disabled, so Board changes cannot be staged."
    : board && !board.statusLabels ? "No status source: turn status labels on to stage Board changes."
    : capabilitiesError ?? (!capabilities ? "Checking your current GitHub permissions…"
      : capabilities.canManageIssues ? null : "Moving cards, changing assignees, and closing or reopening issues requires Triage or higher permission on GitHub.");

  // Stages target on row's card, or drops the card when target changes nothing. The snapshot is taken at first staging.
  function stage(row: BoardIssue, target: BoardTarget) {
    const card = cardOf(row.number);
    if (blocked || !row.status || locked(card)) return;
    const snapshot = card?.snapshot ?? { state: row.state, labels: row.labels, assignees: row.assignees, status: row.status, updatedAt: row.updatedAt };
    if (deriveChange(snapshot, target, saver)) saveBoardCard(viewerKey, repositoryId, { version: 1, number: row.number, snapshot, target, phase: "staged", request: null, operationId: null });
    else removeBoardCard(viewerKey, repositoryId, row.number);
    setCards(loadBoardCards(viewerKey, repositoryId));
  }

  function closeDrawer() {
    onOpenIssue(undefined);
    document.querySelector<HTMLElement>(`[data-issue-card="${openIssue}"] button`)?.focus();
  }

  const phaseBadge = (row: BoardIssue) => {
    const card = cardOf(row.number);
    if (!card) return null;
    const moved = row.status && card.target.status !== row.status;
    const where = !moved ? "" : card.phase === "failed" ? ` · keeps ${STATUS_NAMES[card.target.status]}` : ` · from ${STATUS_NAMES[row.status!]}`;
    return <span className={`${mini} ${card.phase === "failed" ? "border-destructive bg-destructive-soft text-destructive" : "border-primary bg-primary-soft text-primary"}`}>{phaseNames[card.phase]}{where}</span>;
  };
  const openButton = (row: BoardIssue, className: string) => <button type="button" onClick={() => onOpenIssue(row.number)} className={`text-left font-medium hover:underline ${className}`}>{row.title}</button>;

  const columns = column.columns([
    column.accessor("number", { header: "Issue", cell: c => <span className={`font-mono text-xs ${muted}`}>#{c.getValue()}</span> }),
    column.accessor("title", { header: "Title", cell: c => openButton(c.row.original, "") }),
    ...(board?.statusLabels ? [column.display({ id: "status", header: "Status", cell: c => {
      const status = columnOf(c.row.original);
      return <span className="flex flex-wrap items-center gap-1 text-xs">{status && STATUS_NAMES[status]}{c.row.original.multipleStatus && <span className={`${mini} border-warning bg-warning-soft text-warning`}>Multiple status labels</span>}{phaseBadge(c.row.original)}</span>;
    } })] : []),
    column.accessor("labels", { header: "Labels", cell: c => <div className="flex flex-wrap gap-1">{c.getValue().map(name => <span key={name} className={`${mini} border-border bg-muted text-soft`}>{name}</span>)}</div> }),
    column.accessor("assignees", { header: "Assignees", cell: c => <span className="text-xs text-soft">{c.getValue().length ? c.getValue().map(a => `@${a}`).join(", ") : <span className={muted}>Unassigned</span>}</span> }),
  ]);
  const table = useTable({ features, columns, data: shown });

  const listView = (
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

  const openCapped = board?.truncated && rows.filter(row => row.state === "open").length >= OPEN_CAP;
  const closedCapped = board?.truncated && rows.filter(row => row.state === "closed").length >= CLOSED_CAP;
  const boardView = (
    <div className="flex min-h-0 flex-1 gap-3 overflow-x-auto">
      {BOARD_STATUSES.map(status => {
        const items = shown.filter(row => columnOf(row) === status);
        return (
          <section
            key={status}
            className={`${panel} flex min-w-[210px] flex-1 flex-col`}
            onDragOver={e => { if (!blocked) e.preventDefault(); }}
            onDrop={e => {
              e.preventDefault();
              const row = rows.find(r => r.number === Number(e.dataTransfer.getData("text/plain")));
              if (row) stage(row, { ...targetOf(row), status });
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
                  draggable={!blocked && !locked(cardOf(row.number))}
                  onDragStart={e => e.dataTransfer.setData("text/plain", String(row.number))}
                  className="flex flex-col gap-1.5 rounded-lg border border-border bg-card p-2.5 hover:border-border-strong"
                >
                  {openButton(row, "leading-snug")}
                  <div className="flex flex-wrap items-center gap-1.5 text-xs">
                    <span className={`font-mono ${muted}`}>#{row.number}</span>
                    {row.assignees.length > 0 && <span className="text-soft">{row.assignees.map(a => `@${a}`).join(", ")}</span>}
                    {row.multipleStatus && <span className={`${mini} border-warning bg-warning-soft text-warning`}>Multiple status labels</span>}
                    {phaseBadge(row)}
                  </div>
                </li>
              ))}
            </ul>
          </section>
        );
      })}
    </div>
  );

  const staged = cards.filter(card => card.phase === "staged").length;
  const select = (labelText: string, value: string, onChange: (value: string) => void, values: string[], all: string): ReactNode => (
    <select aria-label={labelText} value={value} onChange={e => onChange(e.target.value)} className={field}>
      <option value="">{all}</option>
      {values.map(v => <option key={v} value={v}>{v}</option>)}
    </select>
  );
  const openRow = openIssue === undefined ? undefined : rows.find(row => row.number === openIssue);
  const openCard = openIssue === undefined ? undefined : cardOf(openIssue);

  return (
    <div className="flex min-h-0 flex-1 flex-col gap-3">
      <section aria-label="Board controls" className={`${panel} flex flex-none flex-wrap items-center gap-2 px-4 py-3`}>
        {board?.statusLabels && (
          <div role="group" aria-label="View" className="inline-flex gap-0.5 rounded-full border border-border bg-card p-0.5">
            {(["board", "list"] as const).map(v => <button key={v} type="button" aria-pressed={view === v} onClick={() => setView(v)}
              className="rounded-full px-[11px] py-[3px] text-xs text-muted-foreground aria-pressed:bg-primary-soft aria-pressed:font-semibold aria-pressed:text-primary">{v === "board" ? "Board" : "List"}</button>)}
          </div>
        )}
        <input type="search" aria-label="Filter by text" placeholder="Filter by title or #" value={text} onChange={e => setText(e.target.value)} className={`${field} w-48`} />
        {select("Filter by assignee", assignee, setAssignee, options(rows.flatMap(row => row.assignees)), "All assignees")}
        {select("Filter by label", label, setLabel, options(rows.flatMap(row => row.labels)), "All labels")}
        {board?.statusLabels && (
          <div className="ml-auto flex items-center gap-2 text-xs">
            <span className={muted}>{staged === 1 ? "1 staged change" : `${staged} staged changes`} · nothing is sent to GitHub until you save</span>
            <Button size="sm" disabled title="Reviewing and saving staged changes is not available yet.">Save…</Button>
          </div>
        )}
      </section>
      {demo && (
        <div role="status" className="flex-none rounded-[10px] border border-demo bg-demo-soft px-3 py-2 text-[13px] text-demo">
          <strong>Demo mode</strong> — remote GitHub writes are disabled, so Board changes cannot be staged.
        </div>
      )}
      {board?.truncated && (
        <p role="note" className={`m-0 flex-none text-xs ${muted}`}>
          The Board shows only {[openCapped && `the ${OPEN_CAP} most recently updated open issues`, closedCapped && `the ${CLOSED_CAP} most recently closed issues`].filter(Boolean).join(" and ") || "part of this repository's issues"}.
        </p>
      )}
      {query.isPending ? <p role="status" className={`${panel} p-4`}>Loading the Board…</p>
        : query.isError ? <p role="alert" className={`${panel} p-4`}>{query.error.message} <Button variant="outline" size="sm" onClick={() => void query.refetch()}>Retry</Button></p>
        : !board ? <p role="alert" className={`${panel} p-4`}>The Board is unavailable for this repository.</p>
        : board.statusLabels ? (view === "board" ? boardView : listView)
        : (
          <div className="flex min-h-0 flex-1 gap-3 max-[900px]:flex-col">
            {listView}
            <div className="flex min-h-0 flex-[1_1_320px] flex-col gap-3">
              <section className={`${panel} px-4 py-3`}>
                <h2 className="m-0 text-sm">No status source</h2>
                <p className={`m-0 text-xs ${muted}`}>This repository has no status labels, so its issues are listed without columns and cannot be moved.</p>
              </section>
              <StatusSourceSetup repositoryId={repositoryId} viewerKey={viewerKey} demo={demo} frozen={false} />
            </div>
          </div>
        )}
      {openIssue !== undefined && board && (
        <TaskDrawer
          key={openIssue}
          issueNumber={openIssue}
          issue={openRow}
          card={openCard}
          github={github}
          assignable={assignable}
          blocked={blocked ?? (locked(openCard) ? "This card's last save is not confirmed yet." : null)}
          sideEffects={openCard ? deriveChange(openCard.snapshot, openCard.target, saver)?.sideEffects ?? [] : []}
          onStage={target => openRow && stage(openRow, target)}
          onDiscard={() => { removeBoardCard(viewerKey, repositoryId, openIssue); setCards(loadBoardCards(viewerKey, repositoryId)); }}
          onClose={closeDrawer}
        />
      )}
    </div>
  );
}

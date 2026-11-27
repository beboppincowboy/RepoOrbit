import { useMemo, useState, type ReactNode } from "react";
import { BoardColumns } from "@/features/board/components/board-columns";
import { BoardControls } from "@/features/board/components/board-controls";
import { BoardListTable } from "@/features/board/components/board-list-table";
import { BoardNotices } from "@/features/board/components/board-notices";
import { BoardSaveReview } from "@/features/board/components/board-save-review";
import { panel } from "@/features/board/components/board-styles";
import { NoStatusSource } from "@/features/board/components/no-status-source";
import { TaskDrawer } from "@/features/board/components/task-drawer";
import type { BoardIssue } from "@/features/board/api";
import { useBoardCards } from "@/features/board/hooks/use-board-cards";
import { CLOSED_CAP, OPEN_CAP, cardOf, deriveChange, locked, matchesFilters, searchNeedle, type BoardCard } from "@/features/board/model";
import { confirmDisabled, reviewItems, reviewableCards } from "@/features/board/review";
import type { IssueCapabilities } from "@/features/issues/api";
import { STATUS_NAMES, type BoardStatus } from "@/features/issues/status";
import { viewerKeyOf } from "@/features/repositories/access-store";
import type { ViewerSession } from "@/shared/api/transport";
import { Button } from "@/shared/ui/button";

const noRows: BoardIssue[] = [];

type IssueBoardProps = {
  session: ViewerSession;
  repositoryId: string;
  github: string;
  capabilities: IssueCapabilities | null;
  capabilitiesError: string | null;
  assignable: string[];
  openIssue: number | undefined;
  onOpenIssue: (issueNumber: number | undefined) => void;
  onOpenRun?: (runId: string) => void;
  // Called after a save or a recovery that may have written to GitHub.
  onGitHubWrite: () => void;
};

export function IssueBoard({ session, repositoryId, github, capabilities, capabilitiesError, assignable, openIssue, onOpenIssue, onOpenRun, onGitHubWrite }: IssueBoardProps) {
  const viewerKey = viewerKeyOf(session);
  const viewerLogin = session.user?.login ?? null;
  const demo = session.mode === "demo";
  const [view, setView] = useState<"board" | "list">("board");
  const [dragging, setDragging] = useState<BoardIssue>();
  const [dropTarget, setDropTarget] = useState<BoardStatus>();
  const [assignee, setAssignee] = useState("");
  const [label, setLabel] = useState("");
  const [text, setText] = useState("");
  // The staged cards as they were when the review opened; null while it is closed.
  const [reviewed, setReviewed] = useState<BoardCard[] | null>(null);
  const saver = { login: viewerLogin ?? "", assignable: assignable.some(login => login.toLowerCase() === viewerLogin?.toLowerCase()) };
  const { cards, query, board, blocked, recoveryBlocked, saving, saveNotice, setSaveNotice, stage, discard, confirmSave, recover } =
    useBoardCards({ session, repositoryId, openIssue, saver, capabilities, capabilitiesError, onGitHubWrite });
  const rows = board?.issues ?? noRows;

  const needle = searchNeedle(text);
  const shown = useMemo(() => rows.filter(row => matchesFilters(row, { assignee, label, needle })), [rows, assignee, label, needle]);

  // A Board that stops showing its columns takes the review with it.
  if (reviewed && !board?.statusLabels) setReviewed(null);

  function closeDrawer() {
    onOpenIssue(undefined);
    document.querySelector<HTMLElement>(`[data-issue-card="${openIssue}"] button`)?.focus();
  }

  const openCapped = board?.truncated && rows.filter(row => row.state === "open").length >= OPEN_CAP;
  const closedCapped = board?.truncated && rows.filter(row => row.state === "closed").length >= CLOSED_CAP;
  const stagedCards = reviewableCards(cards);
  const drawerOpen = openIssue !== undefined && board !== null;
  const openRow = openIssue === undefined ? undefined : rows.find(row => row.number === openIssue);
  const openCard = openIssue === undefined ? undefined : cardOf(cards, openIssue);
  const listView = <BoardListTable shown={shown} cards={cards} statusLabels={Boolean(board?.statusLabels)} onOpenIssue={onOpenIssue} />;

  function renderContent(): ReactNode {
    if (query.isPending) {
      return <p role="status" className={`${panel} p-4`}>Loading the Board…</p>;
    }
    if (query.isError) {
      return <p role="alert" className={`${panel} p-4`}>{query.error.message} <Button variant="outline" size="sm" onClick={() => void query.refetch()}>Retry</Button></p>;
    }
    if (!board) {
      return <p role="alert" className={`${panel} p-4`}>The Board is unavailable for this repository.</p>;
    }
    if (board.statusLabels) {
      return view === "board" ? (
        <BoardColumns
          rows={rows}
          shown={shown}
          cards={cards}
          blocked={blocked}
          dragging={dragging}
          setDragging={setDragging}
          dropTarget={dropTarget}
          setDropTarget={setDropTarget}
          openCapped={openCapped}
          closedCapped={closedCapped}
          onStage={(row, target) => void stage(row, target)}
          onOpenIssue={onOpenIssue}
        />
      ) : listView;
    }
    return <NoStatusSource list={listView} repositoryId={repositoryId} viewerKey={viewerKey} demo={demo} />;
  }

  return (
    <div className={`flex min-h-0 flex-1 flex-col gap-3 ${drawerOpen ? "min-[900px]:mr-[400px]" : ""}`}>
      <BoardControls
        rows={rows}
        statusLabels={Boolean(board?.statusLabels)}
        view={view}
        setView={setView}
        text={text}
        setText={setText}
        assignee={assignee}
        setAssignee={setAssignee}
        label={label}
        setLabel={setLabel}
        drawerOpen={drawerOpen}
        staged={stagedCards.length}
        blocked={blocked}
        saving={saving}
        onSave={() => { setSaveNotice(null); setReviewed(stagedCards); }}
      />
      <BoardNotices saveNotice={saveNotice} demo={demo} truncated={board?.truncated} openCapped={openCapped} closedCapped={closedCapped} />
      {renderContent()}
      {drawerOpen && (
        <TaskDrawer
          key={openIssue}
          issueNumber={openIssue}
          repositoryId={repositoryId}
          viewerKey={viewerKey}
          onOpenRun={onOpenRun}
          issue={openRow}
          card={openCard}
          github={github}
          assignable={assignable}
          blocked={blocked ?? (locked(openCard) ? "This card's last save is not confirmed yet." : null)}
          sideEffects={openCard ? deriveChange(openCard.snapshot, openCard.target, saver)?.sideEffects ?? [] : []}
          recoveryBlocked={recoveryBlocked(openCard)}
          busy={saving}
          onRecover={() => recover(openIssue)}
          onStage={target => { if (openRow) void stage(openRow, target); }}
          onDiscard={() => void discard(openIssue)}
          onClose={closeDrawer}
        />
      )}
      {reviewed && board?.statusLabels && (
        <BoardSaveReview
          items={reviewItems(reviewed, rows, saver, STATUS_NAMES)}
          blocked={blocked}
          saving={saving}
          confirmDisabled={confirmDisabled({ blocked, saving })}
          onConfirm={() => void confirmSave(reviewed, () => setReviewed(null))}
          onClose={() => setReviewed(null)}
        />
      )}
    </div>
  );
}

import { createFileRoute, Link, useNavigate } from "@tanstack/react-router";
import { CicdTab } from "@/components/cicd-tab";
import { IssueBoard } from "@/components/issue-board";
import { RunDetailDrawer } from "@/components/run-detail-drawer";
import { Banner } from "@/features/analytics/components/banner";
import { Contributors } from "@/features/analytics/components/contributors";
import { OverviewTab } from "@/features/analytics/components/overview-tab";
import { PullRequests } from "@/features/analytics/components/pull-requests";
import { RepositoryUnavailable } from "@/features/analytics/components/repository-unavailable";
import { SnapshotBanner } from "@/features/analytics/components/snapshot-banner";
import { SyncDetails } from "@/features/analytics/components/sync-details";
import { Timeline } from "@/features/analytics/components/timeline";
import { card, Ico, icons, mini, muted, smallBtn } from "@/features/analytics/components/workspace-parts";
import { useRepositoryOverview } from "@/features/analytics/hooks/use-repository-overview";
import { useWriteRefresh } from "@/features/analytics/hooks/use-write-refresh";
import { IssueEditor } from "@/features/issues/components/issue-editor";
import { IssuesTab } from "@/features/issues/components/issues-tab";
import { useWorkspaceEditor } from "@/features/issues/hooks/use-workspace-editor";
import { viewerKeyOf } from "@/features/repositories/access-store";
import { SyncStatus } from "@/features/repositories/sync-status";
import type { MetricWindow } from "@/shared/api/transport";
import { Button } from "@/shared/ui/button";

const tabs = { overview: "Overview", issues: "Issues", board: "Board", pulls: "Pull Requests", cicd: "CI/CD", contributors: "Contributors", activity: "Activity" } as const;
type Tab = keyof typeof tabs;
const windows = [7, 30, 90] as const;
type EditorSearch = "new" | number | undefined;

function parseEditorSearch(raw: unknown): EditorSearch {
  if (raw === "new") return "new";
  if (typeof raw === "number" && Number.isInteger(raw) && raw > 0) return raw;
  if (typeof raw === "string" && /^\d+$/.test(raw)) {
    const n = Number.parseInt(raw, 10);
    if (n > 0) return n;
  }
  return undefined;
}

// A workflow run id is a positive integer, kept as a string because GitHub ids can exceed Number.MAX_SAFE_INTEGER.
function parseRunSearch(raw: unknown): string | undefined {
  const text = typeof raw === "number" && Number.isSafeInteger(raw) ? String(raw) : raw;
  return typeof text === "string" && /^[1-9]\d{0,18}$/.test(text) ? text : undefined;
}

export const Route = createFileRoute("/_shell/workspace/$repositoryId")({
  validateSearch: (search: Record<string, unknown>): { days: MetricWindow; tab: Tab; editor?: "new" | number; issue?: number; run?: string } => {
    const editor = parseEditorSearch(search.editor);
    const issue = parseEditorSearch(search.issue);
    const tab: Tab = typeof search.tab === "string" && Object.hasOwn(tabs, search.tab) ? search.tab as Tab : "overview";
    const run = tab === "cicd" ? parseRunSearch(search.run) : undefined;
    return {
      days: search.days === 7 || search.days === 90 ? search.days : 30,
      tab,
      ...(editor !== undefined ? { editor } : {}),
      ...(typeof issue === "number" ? { issue } : {}),
      ...(run !== undefined ? { run } : {}),
    };
  },
  component: RepositoryWorkspace,
});

function RepositoryWorkspace() {
  const { repositoryId } = Route.useParams();
  const search = Route.useSearch();
  const navigate = useNavigate({ from: "/workspace/$repositoryId" });
  const { session } = Route.useRouteContext();
  const viewerKey = viewerKeyOf(session);
  const demo = session.mode === "demo";
  const { query, overview, snapshot, snapshotReason, selection, row, rowVisibility, retryAt, rateLimited, sync, display, unavailable } =
    useRepositoryOverview(session, repositoryId, search.days);
  const {
    editorOpen, editingIssue, savedCreateDraft, capabilitiesQuery, assigneeSuggestions,
    openNewIssue, openEditIssue, switchToEditIssue, closeEditor, refreshSavedDraft,
  } = useWorkspaceEditor({
    viewerKey,
    repositoryId,
    demo,
    frozen: Boolean(snapshot),
    overview,
    tab: search.tab,
    editor: search.editor,
    onShowEditor: editor => void navigate({ search: prev => ({ ...prev, editor }), replace: true }),
    onHideEditor: () => void navigate({ search: prev => ({ days: prev.days, tab: prev.tab, ...(parseRunSearch(prev.run) ? { run: prev.run } : {}) }), replace: true }),
  });
  const { handleWriteRecorded, handleBoardWrite } = useWriteRefresh(session, repositoryId, refreshSavedDraft);
  const path = { to: Route.to, params: { repositoryId } } as const;

  function selectRun(runId: string) {
    void navigate({ search: prev => ({ days: prev.days, tab: "cicd", run: runId }), replace: true });
  }

  function closeRun() {
    void navigate({ search: prev => ({ ...prev, run: undefined }), replace: true });
  }

  if (unavailable || !overview || !sync) return <RepositoryUnavailable
    unavailable={unavailable}
    repositoryName={rowVisibility.canRevealName ? row?.repo.fullName : null}
    retryAt={retryAt}
    rateLimited={rateLimited}
    loading={query.isPending}
    retrying={query.isFetching || selection.isFetching}
    onRetry={() => void Promise.all([query.refetch(), selection.refetch()])}
  />;

  const [owner, name] = overview.fullName.split("/");
  const github = `https://github.com/${encodeURIComponent(owner)}/${encodeURIComponent(name)}`;
  const tabSearch = { days: search.days, tab: search.tab };
  // TanStack Start SSR can pass raw query strings through useSearch before validateSearch runs.
  const runId = search.tab === "cicd" ? parseRunSearch(search.run) : undefined;

  return <div className="flex h-full min-h-0 flex-col gap-3 max-[900px]:overflow-auto">
    <section className={`${card} flex flex-none flex-col gap-2.5 px-4 pt-4`}>
      <div className="flex flex-wrap items-center gap-x-3 gap-y-2">
        <Ico>{icons.repo}</Ico>
        <div className="min-w-0 break-words"><div className={`text-xs ${muted}`}>{owner} /</div><h1 className="text-[19px] leading-[1.2]">{name}</h1></div>
        {!demo && <a className={smallBtn} href={github}>View on GitHub {icons.ext}</a>}
        {snapshot ? <span className={smallBtn}>Frozen snapshot</span> : demo ? <SyncStatus compact {...sync} /> : <SyncDetails sync={sync} repositoryId={repositoryId} onRefresh={() => void query.refetch()} />}
        {overview.activity.archived && <span className={`${mini} border-border text-soft`}>Archived</span>}
        <div className="ml-auto flex flex-wrap items-center gap-2">
          <div role="group" aria-label="Time window" className="inline-flex gap-0.5 rounded-full border border-border bg-card p-0.5">
            {windows.map(days => <Link key={days} {...path} search={{ ...tabSearch, days }} aria-current={search.days === days ? "page" : undefined}
              className="rounded-full px-[11px] py-[3px] text-xs text-muted-foreground aria-[current=page]:bg-primary-soft aria-[current=page]:font-semibold aria-[current=page]:text-primary">{days}d</Link>)}
          </div>
          <Button
            size="sm"
            onClick={openNewIssue}
            disabled={Boolean(snapshot)}
            title={snapshot ? "Creating issues needs a live connection." : undefined}
          >
            + New issue{savedCreateDraft ? " (draft)" : ""}
          </Button>
        </div>
      </div>
      <nav aria-label="Workspace sections" className="flex flex-wrap gap-x-5 gap-y-0.5 border-b border-border">
        {(Object.keys(tabs) as Tab[]).map(tab => <Link key={tab} {...path} search={{ ...tabSearch, tab }} aria-current={search.tab === tab ? "page" : undefined}
          className="-mb-px border-b-2 border-transparent py-2 text-muted-foreground aria-[current=page]:border-foreground aria-[current=page]:font-semibold aria-[current=page]:text-foreground">{tabs[tab]}</Link>)}
      </nav>
    </section>
    {snapshot ? <SnapshotBanner
      savedAt={snapshot.dataUpdatedAt}
      reason={snapshotReason}
      retryAt={retryAt}
      rateLimited={rateLimited}
      retrying={query.isFetching}
      onRetry={() => void query.refetch()}
    /> : display && <Banner overview={overview} display={display} sync={sync} />}
    {query.isError && !snapshot && <div role="alert" className={`${card} flex items-center gap-3 p-3`}>
      Could not refresh repository analytics. <Button variant="outline" size="sm" onClick={() => void query.refetch()} disabled={query.isFetching}>Retry</Button>
    </div>}
    {search.tab === "overview" && <OverviewTab overview={overview} days={search.days} recent={<Link {...path} search={{ ...tabSearch, tab: "activity" }} className={smallBtn}>All</Link>} github={github} />}
    {search.tab === "issues" && (
      <IssuesTab
        id={repositoryId}
        viewerKey={viewerKey}
        demo={demo}
        frozen={Boolean(snapshot)}
        github={github}
        onNewIssue={openNewIssue}
        onEditIssue={openEditIssue}
        onResolved={handleWriteRecorded}
      />
    )}
    {search.tab === "board" && (snapshot ? <p className={`${card} p-4`}>The Board needs a live connection. Saved issue counts are available on Overview.</p> : (
      <IssueBoard
        key={repositoryId}
        session={session}
        repositoryId={repositoryId}
        github={github}
        capabilities={capabilitiesQuery.data ?? null}
        capabilitiesError={capabilitiesQuery.isError ? capabilitiesQuery.error.message : null}
        assignable={assigneeSuggestions}
        openIssue={search.issue}
        onOpenIssue={issue => void navigate({ search: prev => ({ ...prev, issue }), replace: true })}
        onOpenRun={selectRun}
        onGitHubWrite={handleBoardWrite}
      />
    ))}
    {search.tab === "pulls" && (snapshot ? <p className={`${card} p-4`}>The pull request list needs a live connection. Saved PR counts are available on Overview.</p> : <PullRequests key={repositoryId} id={repositoryId} viewer={viewerKey} github={github} />)}
    {search.tab === "cicd" && (snapshot ? <p className={`${card} p-4`}>CI/CD metrics need a live connection. Saved issue counts are available on Overview.</p> : (
      <CicdTab key={repositoryId} repositoryId={repositoryId} viewerKey={viewerKey} days={search.days} github={github} onSelectRun={selectRun} />
    ))}
    {search.tab === "contributors" && <Contributors overview={overview} days={search.days} />}
    {search.tab === "activity" && <section className={`${card} flex-1 overflow-auto`}><Timeline overview={overview} github={github} /></section>}
    {runId && !snapshot && (
      <RunDetailDrawer key={runId} repositoryId={repositoryId} viewerKey={viewerKey} runId={runId} github={github} onClose={closeRun} />
    )}
    {editorOpen && !snapshot && (
      <IssueEditor
        key={`${repositoryId}:${search.editor}`}
        mode={search.editor === "new" ? "create" : "update"}
        issue={editingIssue}
        viewerKey={viewerKey}
        viewerLogin={session.user?.login ?? null}
        repositoryId={repositoryId}
        fullName={overview.fullName}
        demo={demo}
        archived={overview.activity.archived}
        capabilities={capabilitiesQuery.data ?? null}
        capabilitiesLoading={capabilitiesQuery.isPending && !demo}
        capabilitiesError={capabilitiesQuery.isError ? capabilitiesQuery.error.message : null}
        onRetryCapabilities={() => void capabilitiesQuery.refetch()}
        assigneeSuggestions={assigneeSuggestions}
        onClose={closeEditor}
        onOperationRecorded={handleWriteRecorded}
        onSwitchToEditIssue={switchToEditIssue}
      />
    )}
  </div>;
}

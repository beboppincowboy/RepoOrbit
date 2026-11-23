import { repositorySnapshotKey, repositorySnapshotPrefix } from "@/lib/repository-snapshots";
import { useEffect, useMemo, useState, type FormEvent, type ReactNode } from "react";
import { useInfiniteQuery, useQuery, useQueryClient, type InfiniteData } from "@tanstack/react-query";
import { createFileRoute, Link, useNavigate } from "@tanstack/react-router";
import { createColumnHelper, tableFeatures, useTable } from "@tanstack/react-table";
import { clearRepositoryIssueDrafts, hasSavedCreateDraft, IssueEditor, type EditableIssue } from "@/components/issue-editor";
import { IssueIcon, PullRequestIcon } from "@/components/needs-attention";
import { LineChart, type ChartSeries } from "@/components/line-chart";
import { describeActivity, describeMetadata, LoadingCircle, RefreshActivityButton, syncDisplay, SyncStatus, type SyncInput } from "@/components/sync-status";
import { Button, buttonVariants } from "@/components/ui/button";
import { WriteHistory, writeOperationsQueryKey } from "@/components/write-history";
import { selectedRepositoriesKey, flattenSelectedRepositoryRows, useSelectedRepositories } from "@/lib/selected-repositories";
import {
  RepositoryAccessUnverifiedError,
  GraphQLUnavailableError,
  fetchIssueCapabilities,
  fetchWorkspaceIssue,
  fetchSelectedRepositoryDetailsPage,
  fetchRepositoryOverview,
  fetchWorkspaceIssues,
  fetchWorkspacePullRequests,
  type ActivityResourceStatus,
  type MetricWindow,
  type ReviewState,
  type WorkspaceIssue,
  type WorkspacePullRequest,
  type WriteOperation,
  type WriteOperationIssue,
} from "@/lib/api";

const tabs = { overview: "Overview", issues: "Issues", pulls: "Pull Requests", contributors: "Contributors", activity: "Activity" } as const;
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

export const Route = createFileRoute("/_shell/workspace/$repositoryId")({
  validateSearch: (search: Record<string, unknown>): { days: MetricWindow; tab: Tab; editor?: "new" | number } => {
    const editor = parseEditorSearch(search.editor);
    return {
      days: search.days === 7 || search.days === 90 ? search.days : 30,
      tab: typeof search.tab === "string" && Object.hasOwn(tabs, search.tab) ? search.tab as Tab : "overview",
      ...(editor !== undefined ? { editor } : {}),
    };
  },
  component: RepositoryWorkspace,
});

type Overview = NonNullable<Awaited<ReturnType<typeof fetchRepositoryOverview>>>;
type Coverage = Overview["openIssues"]["coverage"];
type Entry = Overview["timeline"][number];

// Prototype D2 classes, as Tailwind strings.
const card = "rounded-xl border border-border bg-card min-w-0 min-h-0";
const muted = "text-muted-foreground";
const lrow = "flex items-center gap-3 border-b border-border px-3 py-2.5 last:border-b-0";
const mini = "inline-flex items-center gap-1 whitespace-nowrap rounded-full border px-[7px] text-[11px]";
const smallBtn = buttonVariants({ variant: "outline", size: "sm" });

const svg = (size: number, d: ReactNode) => <svg width={size} height={size} viewBox="0 0 16 16" aria-hidden="true" className="shrink-0">{d}</svg>;
const icons = {
  repo: svg(16, <path d="M3 2.5h8.5v10H4.5A1.5 1.5 0 0 1 3 11zM3 11a1.5 1.5 0 0 1 1.5-1.5h7" fill="none" stroke="currentColor" strokeWidth="1.4" />),
  pr: <PullRequestIcon />,
  issue: <IssueIcon />,
  tag: svg(14, <><path d="M2.5 2.5h5.3l5.7 5.7-5.3 5.3-5.7-5.7z" fill="none" stroke="currentColor" strokeWidth="1.4" strokeLinejoin="round" /><circle cx="5.5" cy="5.5" r="1" fill="currentColor" /></>),
  ext: svg(12, <path d="M9 3h4v4M13 3L7 9M11 9.5V13H3V5h3.5" fill="none" stroke="currentColor" strokeWidth="1.5" />),
  info: svg(14, <><circle cx="8" cy="8" r="6" fill="none" stroke="currentColor" strokeWidth="1.4" /><path d="M8 7v4M8 5v.5" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" /></>),
};
const Ico = ({ children, tone = "bg-muted text-muted-foreground" }: { children: ReactNode; tone?: string }) =>
  <span className={`grid size-7 shrink-0 place-items-center rounded-lg ${tone}`}>{children}</span>;

const day = new Intl.DateTimeFormat("en-US", { month: "short", day: "numeric", timeZone: "UTC" });
const stamp = new Intl.DateTimeFormat("en-US", { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit", hourCycle: "h23", timeZone: "UTC" });
const clock = new Intl.DateTimeFormat("en-GB", { hour: "2-digit", minute: "2-digit" });
function ago(iso: string) {
  const minutes = Math.max(0, Math.round((Date.now() - Date.parse(iso)) / 60000));
  return minutes < 60 ? `${minutes}m ago` : minutes < 1440 ? `${Math.round(minutes / 60)}h ago` : `${Math.round(minutes / 1440)}d ago`;
}
// Prototype style: 45m, 4.1h, 11h, 2.3d.
function duration(seconds: number | null) {
  if (seconds === null) return null;
  const hours = seconds / 3600;
  if (hours < 1) return `${Math.round(seconds / 60)}m`;
  if (hours < 48) return hours < 10 ? `${hours.toFixed(1)}h` : `${Math.round(hours)}h`;
  return `${(hours / 24).toFixed(1)}d`;
}
// Links come from the API; only GitHub URLs are rendered as links.
const githubHref = (url: string, fallback: string) => url.startsWith("https://github.com/") ? url : fallback;

// KPI sub-line for an uncovered metric: [text, tone]. Partial reasons are amber like the prototype.
const coverageReasons: Record<string, [string, string]> = {
  not_synchronized: ["Awaiting data", muted],
  window_before_boundary: ["Window predates imported history", "text-warning"],
  historical_gap: ["Partial: issue event history incomplete", "text-warning"],
  children_incomplete: ["Partial: reviews and comments importing", "text-warning"],
};
const coverageReason = (coverage: Coverage) => coverageReasons[coverage.reason ?? ""] ?? ["Coverage incomplete", "text-warning"];

function RepositoryWorkspace() {
  const { repositoryId } = Route.useParams();
  const search = Route.useSearch();
  const navigate = useNavigate({ from: "/workspace/$repositoryId" });
  const { session } = Route.useRouteContext();
  const queryClient = useQueryClient();
  const viewerKey = `${session.mode}:${session.user?.login ?? ""}`;
  const [selectedEditIssue, setSelectedEditIssue] = useState<EditableIssue | null>(null);
  const [draftRevision, setDraftRevision] = useState(0);
  const overviewKey = ["repositoryOverview", session.mode, session.user?.login, repositoryId, search.days] as const;
  const snapshotPrefix = repositorySnapshotPrefix(session, repositoryId);
  const snapshotKey = repositorySnapshotKey(session, repositoryId, search.days);
  const query = useQuery({
    queryKey: overviewKey,
    queryFn: async ({ signal }) => {
      try {
        const data = await fetchRepositoryOverview(repositoryId, search.days, signal);
        signal.throwIfAborted();
        if (data) queryClient.setQueryData(snapshotKey, data);
        else queryClient.removeQueries({ queryKey: snapshotPrefix });
        return data;
      } catch (error) {
        if (!signal.aborted && !(error instanceof GraphQLUnavailableError) && !(error instanceof RepositoryAccessUnverifiedError && error.retryAt)) {
          queryClient.removeQueries({ queryKey: snapshotPrefix });
        }
        throw error;
      }
    },
    placeholderData: (previous, query) => query?.queryKey[1] === session.mode && query.queryKey[2] === session.user?.login && query.queryKey[3] === repositoryId ? previous : undefined,
    gcTime: 0,
    refetchOnWindowFocus: ({ state }) => !(state.error instanceof RepositoryAccessUnverifiedError && state.error.retryAt),
    retry: (count, error) => !(error instanceof RepositoryAccessUnverifiedError || error instanceof GraphQLUnavailableError) && count < 3,
    // Follow running or not-yet-started sync work; stop once it settles (reconciliation leaves settled streams "queued").
    refetchInterval: ({ state }) => {
      if (state.error instanceof RepositoryAccessUnverifiedError && state.error.retryAt) return false;
      if (!state.data) return false;
      const { syncState, activity } = state.data;
      const display = syncDisplay({ syncState, activity });
      if (display === "syncing") return 3000;
      if (display === "awaiting") return 10000;
      if (display !== "partial") return false;
      const streams = [activity.issues, activity.pullRequests, activity.releases, activity.issueEvents?.traversal];
      const children = [activity.reviews, activity.comments];
      const queued = streams.some(stream => stream?.jobState === "queued" && stream.phase !== "current")
        || children.some(child => child && !child.complete && child.nextAttemptAt);
      return queued ? 10000 : false;
    },
  });
  const path = { to: Route.to, params: { repositoryId } } as const;
  const snapshotReason = query.error instanceof GraphQLUnavailableError ? query.error.message
    : query.error instanceof RepositoryAccessUnverifiedError && query.error.retryAt ? "GitHub rate-limited access checks." : null;
  const snapshot = snapshotReason ? queryClient.getQueryState<Overview>(snapshotKey) : undefined;
  const overview = query.isError ? snapshot?.data : query.data;
  const demo = session.mode === "demo";
  // Metadata refresh state and the access check's retry time come from the selection list the shell already loads.
  const selection = useSelectedRepositories(session);
  const row = flattenSelectedRepositoryRows(selection).find(r => r.repo.id === repositoryId);
  const accessLost = query.isSuccess && overview === null;
  const accessRejected = query.error instanceof RepositoryAccessUnverifiedError || accessLost;
  useEffect(() => {
    if (!accessRejected || query.isFetching || row?.detail?.state !== "available") return;
    queryClient.setQueryData<InfiniteData<Awaited<ReturnType<typeof fetchSelectedRepositoryDetailsPage>>>>(
      selectedRepositoriesKey(session),
      previous => previous && {
        ...previous,
        pages: previous.pages.map(page => ({
          ...page,
          batches: page.batches.map(batch => {
            if (!batch.details || !batch.repositories.some(repo => repo.id === repositoryId)) return batch;
            return {
              ...batch,
              details: batch.details.map((detail, i) => batch.repositories[i].id === repositoryId && detail.state === "available"
                ? { state: "access-unverified", retryAt: null } : detail),
            };
          }),
        })),
      },
    );
  }, [accessRejected, query.isFetching, queryClient, repositoryId, row?.detail?.state, session]);
  const accessRetryAt = query.error instanceof RepositoryAccessUnverifiedError ? query.error.retryAt : null;
  const retryAt = accessRetryAt ?? (row?.detail?.state === "access-unverified" ? row.detail.retryAt : null);
  const retryTime = retryAt ? Date.parse(retryAt) : NaN;
  const rateLimited = Number.isFinite(retryTime);
  const refetchOverview = query.refetch;
  useEffect(() => {
    const delay = retryTime - Date.now();
    if (!accessRejected || !Number.isFinite(delay)) return;
    const timer = window.setTimeout(() => {
      void refetchOverview();
      void queryClient.invalidateQueries({ queryKey: selectedRepositoriesKey(session) });
    }, Math.max(1000, delay + 1000));
    return () => window.clearTimeout(timer);
  }, [accessRejected, queryClient, refetchOverview, retryTime, session]);
  useEffect(() => {
    if (accessLost && !query.isFetching) {
      clearRepositoryIssueDrafts(viewerKey, repositoryId);
    }
  }, [accessLost, query.isFetching, viewerKey, repositoryId]);
  const editorOpen = search.editor !== undefined;
  const capabilitiesQuery = useQuery({
    queryKey: ["issueCapabilities", viewerKey, repositoryId],
    queryFn: ({ signal }) => fetchIssueCapabilities(repositoryId, signal),
    enabled: !demo && !snapshot && Boolean(overview) && (editorOpen || search.tab === "issues"),
    gcTime: 0,
    retry: false,
  });
  // An editor restored from the URL (after a reload) knows only the issue number: load the imported issue so author-only
  // actions such as close/reopen are offered again.
  const restoredNumber = typeof search.editor === "number" && selectedEditIssue?.number !== search.editor ? search.editor : null;
  const restoredIssueQuery = useQuery({
    queryKey: ["workspaceIssue", viewerKey, repositoryId, restoredNumber],
    queryFn: ({ signal }) => fetchWorkspaceIssue(repositoryId, restoredNumber!, signal),
    enabled: !snapshot && restoredNumber !== null,
    gcTime: 0,
    retry: false,
  });
  const savedCreateDraft = useMemo(() => {
    void draftRevision;
    return !demo && hasSavedCreateDraft(viewerKey, repositoryId);
  }, [demo, viewerKey, repositoryId, draftRevision]);

  function openNewIssue() {
    setSelectedEditIssue(null);
    void navigate({ search: prev => ({ ...prev, editor: "new" }), replace: true });
  }

  function openEditIssue(issue: EditableIssue) {
    setSelectedEditIssue(issue);
    void navigate({ search: prev => ({ ...prev, editor: issue.number }), replace: true });
  }

  function closeEditor() {
    setSelectedEditIssue(null);
    setDraftRevision(v => v + 1);
    void navigate({ search: prev => ({ days: prev.days, tab: prev.tab }), replace: true });
  }

  function handleWriteRecorded(op: WriteOperation) {
    setDraftRevision(v => v + 1);
    void queryClient.invalidateQueries({ queryKey: writeOperationsQueryKey(viewerKey, repositoryId) });
    if (op.state === "succeeded" || op.issueNumber) {
      void queryClient.invalidateQueries({ queryKey: ["workspaceIssues", repositoryId, viewerKey] });
      void queryClient.invalidateQueries({ queryKey: ["repositoryOverview", session.mode, session.user?.login, repositoryId] });
      void queryClient.invalidateQueries({ queryKey: ["crossRepositoryOverview", session.mode, session.user?.login] });
    }
  }

  function switchToEditIssue(issueNumber: number, confirmed?: WriteOperationIssue) {
    openEditIssue({
      number: issueNumber,
      title: confirmed?.title,
      state: confirmed?.state,
      url: confirmed?.url,
      labels: confirmed?.labels,
      assignees: confirmed?.assignees,
    });
  }

  const sync: SyncInput | null = overview ? {
    syncState: overview.syncState,
    activity: demo ? null : overview.activity,
    metadataStatus: row?.repo.metadataStatus ?? null,
    retryAt: row?.detail?.state === "available" ? row.detail.issues.retryAt : retryAt,
  } : null;
  const display = sync ? syncDisplay(sync) : null;

  const unavailable = !snapshot && (accessRejected || row?.detail?.state === "access-unverified");
  if (unavailable || !overview || !sync) return <div className="flex h-full min-h-0 flex-col gap-3">
    <section className={`${card} flex flex-none flex-wrap items-center gap-3 p-4`}>
      <Ico>{icons.repo}</Ico>
      <h1 className="text-[19px] leading-[1.2]">{row?.repo.fullName || "Repository"}</h1>
      {unavailable && <SyncStatus compact syncState="awaiting-sync" withheld={1} retryAt={retryAt} />}
    </section>
    <section aria-label="Repository overview" className={`${card} p-4`}>
      {query.isPending && !unavailable ? <p role="status" className="flex items-center gap-2"><LoadingCircle />Loading repository data…</p> : <>
        <p role="alert" className="m-0">{unavailable
          ? rateLimited && retryAt
            ? `GitHub limited access checks. RepoOrbit will retry after ${clock.format(new Date(retryAt))}.`
            : "Repository data is unavailable because GitHub access could not be verified."
          : "Repository analytics are unavailable."}</p>
        {!rateLimited && <Button className="mt-3" onClick={() => void Promise.all([query.refetch(), selection.refetch()])} disabled={query.isFetching || selection.isFetching}>Retry</Button>}
      </>}
    </section>
  </div>;

  const [owner, name] = overview.fullName.split("/");
  const github = `https://github.com/${encodeURIComponent(owner)}/${encodeURIComponent(name)}`;
  const contributorSuggestions = overview.contributors.filter(c => !c.isBot).map(c => c.login);
  const editingIssue: EditableIssue | undefined = typeof search.editor === "number"
    ? (selectedEditIssue?.number === search.editor ? selectedEditIssue : restoredIssueQuery.data ?? { number: search.editor })
    : undefined;
  const tabSearch = { days: search.days, tab: search.tab };

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
    {snapshot ? <div role="status" className="flex flex-none flex-wrap items-center gap-2 rounded-[10px] border border-warning bg-warning-soft px-3 py-2.5 text-[13px]">
      <strong>Showing saved data from {new Date(snapshot.dataUpdatedAt).toLocaleString()}.</strong>
      <span>{snapshotReason} {retryAt && `Retry after ${clock.format(new Date(retryAt))}.`}</span>
      {!rateLimited && <Button variant="outline" size="sm" onClick={() => void query.refetch()} disabled={query.isFetching}>Retry</Button>}
    </div> : display && <Banner overview={overview} display={display} sync={sync} />}
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
    {search.tab === "pulls" && (snapshot ? <p className={`${card} p-4`}>The pull request list needs a live connection. Saved PR counts are available on Overview.</p> : <PullRequests key={repositoryId} id={repositoryId} viewer={viewerKey} github={github} />)}
    {search.tab === "contributors" && <Contributors overview={overview} days={search.days} />}
    {search.tab === "activity" && <section className={`${card} flex-1 overflow-auto`}><Timeline overview={overview} github={github} /></section>}
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
        contributorSuggestions={contributorSuggestions}
        onClose={closeEditor}
        onOperationRecorded={handleWriteRecorded}
        onSwitchToEditIssue={switchToEditIssue}
      />
    )}
  </div>;
}

// The header pill opens the full sync status (same copy as SyncStatus: metadata and activity lines) and Refresh activity.
function SyncDetails({ sync, repositoryId, onRefresh }: { sync: SyncInput; repositoryId: string; onRefresh: () => void }) {
  const [open, setOpen] = useState(false);
  const metadata = sync.metadataStatus ? describeMetadata(sync.metadataStatus) : [];
  const activity = sync.activity ? [...(sync.activity.synchronized ? [] : [sync.activity.message]), ...describeActivity(sync.activity)] : [];
  const lines = (label: string, items: string[]) => items.length > 0 && <ul aria-label={label} className="m-0 list-none space-y-1 p-0 text-[13px] text-muted-foreground">{items.map(line => <li key={line}>{line}</li>)}</ul>;
  return <div className="relative" onKeyDown={e => { if (e.key === "Escape") setOpen(false); }}>
    <button type="button" aria-expanded={open} aria-controls="sync-details" aria-label="Sync details" onClick={() => setOpen(!open)} className="flex items-center gap-1 rounded-full">
      <SyncStatus compact {...sync} /><span aria-hidden="true" className={`text-muted-foreground transition-transform ${open ? "rotate-90" : ""}`}>›</span>
    </button>
    {open && <div id="sync-details" role="region" aria-label="Sync status" className={`${card} absolute top-full left-0 z-20 mt-2 flex max-h-[60vh] w-[min(420px,80vw)] flex-col gap-2 overflow-auto p-3 shadow-[0_18px_44px_rgba(15,23,42,.16)]`}>
      {lines("Metadata status", metadata)}
      {lines("Activity status", activity)}
      {sync.retryAt && <p className="m-0 text-[13px] text-warning">GitHub rate limited the access check; next check after {clock.format(new Date(sync.retryAt))}.</p>}
      {sync.activity && <RefreshActivityButton repositoryId={repositoryId} onAccepted={onRefresh} />}
    </div>}
  </div>;
}

const metricsOf = (o: Overview) => [o.firstResponseTime.coverage, o.mergeTime.coverage, o.openIssues.coverage, o.pullRequestsMerged.coverage, o.contributorsCoverage];
const streamNames: [string, (a: Overview["activity"]) => ActivityResourceStatus | null | undefined][] = [
  ["issues", a => a.issues], ["pull requests", a => a.pullRequests], ["releases", a => a.releases], ["issue events", a => a.issueEvents?.traversal],
];
const list = (items: string[]) => items.length < 2 ? items.join("") : `${items.slice(0, -1).join(", ")} and ${items.at(-1)}`;

// One state banner under the header, as in the prototype: rate limited, syncing/awaiting, stopped, or partial coverage.
function Banner({ overview, display, sync }: { overview: Overview; display: string; sync: SyncInput }) {
  const a = overview.activity;
  const note = (text: ReactNode, warn = true) => <div role="status" className={`flex flex-none items-start gap-2.5 rounded-[10px] border px-3 py-2.5 text-[13px] ${warn ? "border-warning bg-warning-soft" : "border-border bg-card"}`}>
    <span className="mt-[3px]">{icons.info}</span><span>{text}</span>
  </div>;
  const streams = streamNames.map(([label, get]) => [label, get(a)] as const).filter((s): s is readonly [string, ActivityResourceStatus] => !!s[1]);
  const uncovered = metricsOf(overview).filter(c => !c.covered);
  if (display === "rate-limited") {
    const last = streams.map(([, s]) => s.lastSuccessAt).filter((t): t is string => !!t).sort().at(-1);
    const retry = streams.filter(([, s]) => s.failureCode === "rate_limited").map(([, s]) => s.nextAttemptAt).filter((t): t is string => !!t).sort()[0];
    return note(`Rate limited. Showing the last good data from ${last ? ago(last) : "an earlier sync"}${retry ? `; next retry ${clock.format(new Date(retry))}` : ""}.`);
  }
  if (display === "syncing") {
    const running = streams.filter(([, s]) => s.jobState === "running").map(([label]) => label);
    return note(`Syncing ${running.length ? list(running) : "activity"}. Figures update as data arrives.`, false);
  }
  if (display === "stopped" || display === "access-unverified") {
    const failures = [
      ...streams.map(([label, status]) => [label, status.failureCode ?? (status.jobState === "dead" || status.jobState === "paused" ? status.jobState : null)] as const),
      ["comments", a.comments?.failureCode],
      ["reviews", a.reviews?.failureCode],
      ["metadata", sync.metadataStatus?.failureCode ?? (sync.metadataStatus?.state === "paused" || sync.metadataStatus?.state === "dead" ? sync.metadataStatus.state : null)],
    ].filter(([, code]) => Boolean(code));
    return note(failures.length
      ? `Sync needs attention. ${failures.map(([label, code]) => `${label} (${code?.replace(/_/g, " ")})`).join(", ")}.`
      : "GitHub access could not be verified for sync.");
  }
  if (display === "awaiting" || display === "not-synchronized" || (uncovered.length && uncovered.every(c => c.reason === "not_synchronized"))) {
    return note("First sync has not started. Figures show — until data exists.", false);
  }
  if (uncovered.length || a.issueEvents?.historicalGap) {
    const reasons = new Set(uncovered.map(c => c.reason));
    const parts = [
      reasons.has("historical_gap") || a.issueEvents?.historicalGap ? (overview.eventsCoveredFrom ? `Issue events before ${day.format(new Date(overview.eventsCoveredFrom))} were never recorded` : "Issue event history is not recorded yet") : "",
      reasons.has("children_incomplete") ? `Reviews and comments are still importing${a.reviews?.pendingChildren ? ` (${a.reviews.pendingChildren} pull requests pending)` : ""}` : "",
      reasons.has("not_synchronized") ? "Some activity has not finished its first import" : "",
    ].filter(Boolean);
    return note(<><b className="font-semibold">Partial coverage.</b> {parts.length ? `${parts.join("; ")}.` : "Some figures are not covered for this window."}</>);
  }
  return null;
}

function Kpi({ label, value, coverage, detail, tone = muted }: { label: string; value: ReactNode; coverage: Coverage; detail: ReactNode; tone?: string }) {
  const [reason, reasonTone] = coverageReason(coverage);
  return <div role="group" aria-label={label} className={`${card} px-4 py-3`}>
    <small className={`flex justify-between text-xs ${muted}`}>{label}</small>
    <b className="num block text-[26px] font-semibold leading-[1.25] tracking-[-.02em]">{coverage.covered && value !== null ? value : "—"}</b>
    <span className={`text-xs ${coverage.covered ? tone : reasonTone}`}>{coverage.covered ? detail : reason}</span>
  </div>;
}

function OverviewTab({ overview: o, days, recent, github }: { overview: Overview; days: MetricWindow; recent: ReactNode; github: string }) {
  const chart = useMemo(() => ({
    days: o.daily.map(d => d.day),
    series: [
      { name: "Issues opened", unit: "opened", color: "--primary", values: o.daily.map(d => d.issuesOpened) },
      { name: "PRs merged", unit: "merged", color: "--chart-2", values: o.daily.map(d => d.pullRequestsMerged) },
    ] satisfies ChartSeries[],
  }), [o.daily]);
  return <>
    <section aria-label="Repository metrics" className="grid flex-none grid-cols-[repeat(auto-fit,minmax(160px,1fr))] gap-3">
      <Kpi label="Median first response" value={duration(o.firstResponseTime.seconds)} coverage={o.firstResponseTime.coverage} detail="Issue responsiveness" tone="text-success-foreground" />
      <Kpi label="Median merge time" value={duration(o.mergeTime.seconds)} coverage={o.mergeTime.coverage} detail="Merged PRs" />
      <Kpi label="Open issues" value={o.openIssues.value} coverage={o.openIssues.coverage} detail={o.staleOpenIssues.value === null ? "" : `${o.staleOpenIssues.value} stale`} />
      <Kpi label="Merged PRs" value={o.pullRequestsMerged.value} coverage={o.pullRequestsMerged.coverage} detail={o.openPullRequests?.value != null ? `${o.openPullRequests.value} open now · last ${days} days` : `last ${days} days`} />
    </section>
    <div className="flex min-h-0 flex-1 gap-3 max-[900px]:flex-col">
      <section className={`${card} flex flex-[999_1_520px] flex-col gap-2 p-4 max-[900px]:min-h-[280px]`}>
        <div className="flex flex-wrap justify-between gap-2"><h2 className="text-sm">Opened vs merged</h2>
          <div className={`flex flex-wrap gap-3.5 text-xs ${muted}`}>{chart.series.map(s => <span key={s.name}>
            <i className="mr-[5px] inline-block size-2 rounded-full" style={{ background: `var(${s.color})` }} />{s.name}{s.values.every(v => v === null) && " (not imported yet)"}
          </span>)}</div>
        </div>
        <LineChart days={chart.days} series={chart.series} />
      </section>
      <section className={`${card} flex flex-[1_1_300px] flex-col`}>
        <div className="flex justify-between p-4 pb-1.5"><h2 className="text-sm">Recent activity</h2>{recent}</div>
        <div className="min-h-0 flex-1 overflow-auto">
          {o.timeline.length ? o.timeline.slice(0, 5).map(e => <div key={`${e.kind}-${e.sourceId}`} className={lrow}>
            <Ico>{entryIcon(e)}</Ico>
            <span className="min-w-0 flex-1"><a href={entryHref(e, github)} className="block truncate font-medium hover:underline">{entryLabel(e)}</a><span className={`text-xs ${muted}`}>{actor(e)}</span></span>
            <span className={`whitespace-nowrap text-xs ${muted}`}>{ago(e.occurredAt)}</span>
          </div>) : <p className={`p-4 ${muted}`}>No activity imported yet.</p>}
        </div>
      </section>
    </div>
  </>;
}

const entryIcon = (e: Entry) => e.kind === "pull_request_merged" ? icons.pr : e.kind === "release_published" ? icons.tag : icons.issue;
const entryLabel = (e: Entry) => e.kind === "pull_request_merged" ? `PR #${e.number} merged`
  : e.kind === "release_published" ? `${e.title || "Release"} released`
  : `Issue #${e.number} ${e.kind.replace(/^issue_/, "")}`;
const entryHref = (e: Entry, github: string) => githubHref(e.url, e.number === null ? `${github}/releases` : `${github}/${e.kind === "pull_request_merged" ? "pull" : "issues"}/${e.number}`);
const actor = (e: Entry) => e.actor ? `${e.actor.login}${e.actor.isBot ? " · bot" : ""}` : "ghost";

// Full timeline with GitHub links. A hatched "History gap" row sits where issue-event coverage starts.
function Timeline({ overview: o, github }: { overview: Overview; github: string }) {
  const gapAt = o.eventsCoveredFrom === null ? o.timeline.length
    : Date.parse(o.eventsCoveredFrom) > Date.parse(o.windowStart) ? o.timeline.findIndex(e => Date.parse(e.occurredAt) < Date.parse(o.eventsCoveredFrom!)) : null;
  const gap = <li key="gap" className="hatch border-b border-border p-2.5 text-center text-xs text-muted-foreground">
    {o.eventsCoveredFrom ? `History gap · issue events before ${day.format(new Date(o.eventsCoveredFrom))} were never recorded` : "History gap · issue events are not recorded yet"}
  </li>;
  const rows = o.timeline.map(e => <li key={`${e.kind}-${e.sourceId}`} className="border-b border-border">
    <a href={entryHref(e, github)} className={`${lrow} border-b-0 hover:bg-muted`}>
      <Ico>{entryIcon(e)}</Ico>
      <span className="min-w-0 flex-1"><span className="block font-medium">{entryLabel(e)}{e.kind !== "release_published" && e.title && ` · ${e.title}`}</span><span className={`text-xs ${muted}`}>{actor(e)}</span></span>
      <span className={`font-mono text-xs ${muted}`}>{stamp.format(new Date(e.occurredAt)).replace(",", "")}</span>{icons.ext}
    </a>
  </li>);
  if (gapAt !== null) rows.splice(gapAt === -1 ? rows.length : gapAt, 0, gap);
  if (!o.timeline.length && gapAt === null) return <p className={`p-4 ${muted}`}>No activity in this window.</p>;
  return <ul aria-label="Activity" className="m-0 list-none p-0">{rows}</ul>;
}

// Participation, alphabetical, stacked PRs / reviews / comments. Bots are left out of the bars and listed apart.
function Contributors({ overview: o, days }: { overview: Overview; days: MetricWindow }) {
  const people = o.contributors.filter(p => !p.isBot).sort((x, y) => x.login.localeCompare(y.login));
  const bots = o.contributors.filter(p => p.isBot);
  const max = Math.max(1, ...o.contributors.map(p => p.pullRequestsOpened + p.reviewsSubmitted + p.commentsCreated)) * 1.1;
  const bar = (value: number, color: string) => <span style={{ width: `${value / max * 100}%`, background: `var(${color})` }} />;
  const botText = bots.length ? bots.map(b => [`${b.login} ${b.pullRequestsOpened} PRs`, b.reviewsSubmitted ? `${b.reviewsSubmitted} rev` : "", b.commentsCreated ? `${b.commentsCreated} cmt` : ""].filter(Boolean).join(" · ")).join(", ")
    : o.activeBotContributors.value ? `${o.activeBotContributors.value} active` : "none in this window";
  return <section className={`${card} flex flex-1 flex-col gap-3 p-4`}>
    <div><h2 className="text-sm">Participation · last {days} days</h2><p className={`mt-0.5 mb-0 text-xs ${muted}`}>Alphabetical. Participation, not a productivity ranking.</p></div>
    {!o.contributorsCoverage.covered ? <p className={muted}>— · {coverageReason(o.contributorsCoverage)[0]}</p> : <div className="flex min-h-0 flex-1 flex-col gap-2.5 overflow-auto">
      {people.length ? <ul aria-label="Contributors" className="m-0 flex list-none flex-col gap-2.5 p-0">
        {people.map(p => <li key={p.login} className="grid grid-cols-[28px_110px_1fr_170px] items-center gap-2.5 max-[600px]:grid-cols-[28px_1fr]">
          <span className="inline-grid size-6 place-items-center rounded-full bg-primary-soft text-[10px] font-semibold text-primary" aria-hidden="true">{p.login.slice(0, 2).toUpperCase()}</span>
          <span className="truncate">{p.login}</span>
          <span className="flex h-2.5 overflow-hidden rounded-[5px] bg-muted max-[600px]:col-span-2" aria-hidden="true">{bar(p.pullRequestsOpened, "--primary")}{bar(p.reviewsSubmitted, "--chart-2")}{bar(p.commentsCreated, "--border-strong")}</span>
          <span className={`font-mono text-xs ${muted} max-[600px]:col-span-2`}>{p.pullRequestsOpened} PRs · {p.reviewsSubmitted} rev · {p.commentsCreated} cmt</span>
        </li>)}
      </ul> : <p className={muted}>No contributor activity in this window.</p>}
      <div className={`border-t border-dashed border-border pt-2.5 text-xs ${muted}`}>Bots counted separately: {botText}</div>
    </div>}
  </section>;
}

const reviewLabels: Record<ReviewState, [string, string]> = {
  awaiting_review: ["Awaiting review", "border-border text-soft"],
  commented: ["Commented", "border-border text-soft"],
  approved: ["Approved", "border-success-soft bg-success-soft text-success-foreground"],
  changes_requested: ["Changes requested", "border-warning-soft bg-warning-soft text-warning"],
  merged: ["Merged", "border-primary-soft bg-primary-soft text-primary"],
  closed: ["Closed", "border-border bg-muted text-muted-foreground"],
};
const features = tableFeatures({});
const column = createColumnHelper<typeof features, WorkspacePullRequest>();
const noPulls: WorkspacePullRequest[] = [];

function PullRequests({ id, viewer, github }: { id: string; viewer: string; github: string }) {
  const query = useInfiniteQuery({
    queryKey: ["workspacePullRequests", id, viewer],
    queryFn: ({ pageParam, signal }) => fetchWorkspacePullRequests(id, pageParam, signal),
    initialPageParam: null as string | null,
    getNextPageParam: last => last.hasNextPage ? last.endCursor : undefined,
    gcTime: 0,
  });
  const columns = useMemo(() => column.columns([
    column.accessor("number", { header: "PR", cell: c => <a href={githubHref(c.row.original.url, `${github}/pull/${c.getValue()}`)} className="font-mono text-xs text-muted-foreground hover:underline">#{c.getValue()}</a> }),
    column.accessor("title", { header: "Title", cell: c => <><span className="font-medium">{c.getValue()}</span>{c.row.original.author && <span className={`ml-2 text-xs ${muted}`}>{c.row.original.author.login}</span>}</> }),
    column.accessor("reviewState", { header: "Review", cell: c => {
      const state = c.getValue();
      return state === null ? <span className={`${mini} border-dashed border-border-strong text-muted-foreground`}>Review history importing</span>
        : <span className={`${mini} ${reviewLabels[state][1]}`}>{reviewLabels[state][0]}</span>;
    } }),
    column.accessor("createdAt", { header: "Opened", cell: c => <span className={`font-mono text-xs ${muted}`}>{day.format(new Date(c.getValue()))}</span> }),
  ]), [github]);
  const pulls = useMemo(() => query.data?.pages.flatMap(page => page.nodes) ?? noPulls, [query.data]);
  const table = useTable({ features, columns, data: pulls });
  const th = "sticky top-0 z-[1] whitespace-nowrap border-b border-border bg-card px-3 py-2 text-left text-xs font-medium text-muted-foreground";
  return <div className={`${card} flex-1 overflow-auto`}>
    {query.isPending ? <p role="status" className="p-4">Loading pull requests…</p> : query.isError ? <p role="alert" className="p-4">{query.error.message} <Button variant="outline" onClick={() => void query.refetch()}>Retry</Button></p> : <>
      <table className="w-full border-collapse">
        <thead>{table.getHeaderGroups().map(group => <tr key={group.id}>{group.headers.map(h => <th key={h.id} className={th}><table.FlexRender header={h} /></th>)}</tr>)}</thead>
        <tbody>
          {table.getRowModel().rows.map(row => <tr key={row.id} className="hover:bg-muted">{row.getAllCells().map(cell => <td key={cell.id} className="border-b border-border px-3 py-[9px] align-middle"><table.FlexRender cell={cell} /></td>)}</tr>)}
          {!pulls.length && <tr><td colSpan={columns.length} className={`p-6 text-center ${muted}`}>No imported pull requests.</td></tr>}
        </tbody>
      </table>
      {query.hasNextPage && <div className="p-3"><Button variant="outline" onClick={() => void query.fetchNextPage()} disabled={query.isFetching}>Load more pull requests</Button></div>}
    </>}
  </div>;
}

const issueColumn = createColumnHelper<typeof features, WorkspaceIssue>();
const noIssues: WorkspaceIssue[] = [];

function IssuesTab({
  id,
  viewerKey,
  demo,
  frozen,
  github,
  onNewIssue,
  onEditIssue,
  onResolved,
}: {
  id: string;
  viewerKey: string;
  demo: boolean;
  frozen: boolean;
  github: string;
  onNewIssue: () => void;
  onEditIssue: (issue: EditableIssue) => void;
  onResolved: (op: WriteOperation) => void;
}) {
  const [jumpNumber, setJumpNumber] = useState("");
  const query = useInfiniteQuery({
    queryKey: ["workspaceIssues", id, viewerKey],
    queryFn: ({ pageParam, signal }) => fetchWorkspaceIssues(id, pageParam, signal),
    initialPageParam: null as string | null,
    getNextPageParam: last => last.hasNextPage ? last.endCursor : undefined,
    enabled: !frozen,
    gcTime: 0,
  });

  const issues = useMemo(() => query.data?.pages.flatMap(page => page.nodes) ?? noIssues, [query.data]);

  const columns = useMemo(() => issueColumn.columns([
    issueColumn.accessor("number", {
      header: "Issue",
      cell: c => (
        <a href={githubHref(c.row.original.url, `${github}/issues/${c.getValue()}`)} className="font-mono text-xs text-muted-foreground hover:underline">
          #{c.getValue()}
        </a>
      ),
    }),
    issueColumn.accessor("title", {
      header: "Title",
      cell: c => (
        <>
          <span className="font-medium">{c.getValue()}</span>
          {c.row.original.authorLogin && <span className={`ml-2 text-xs ${muted}`}>@{c.row.original.authorLogin}</span>}
        </>
      ),
    }),
    issueColumn.accessor("state", {
      header: "State",
      cell: c => (
        <span className={`${mini} ${c.getValue() === "open" ? "border-success-soft bg-success-soft text-success-foreground" : "border-border bg-muted text-muted-foreground"}`}>
          {c.getValue()}
        </span>
      ),
    }),
    issueColumn.accessor("labels", {
      header: "Labels",
      cell: c => (
        <div className="flex flex-wrap gap-1">
          {c.getValue().length
            ? c.getValue().map(name => <span key={name} className={`${mini} border-border bg-muted text-soft`}>{name}</span>)
            : <span className={`text-xs ${muted}`}>—</span>}
        </div>
      ),
    }),
    issueColumn.accessor("assignees", {
      header: "Assignees",
      cell: c => (
        <span className="text-xs text-soft">
          {c.getValue().length ? c.getValue().map(a => `@${a}`).join(", ") : <span className={muted}>Unassigned</span>}
        </span>
      ),
    }),
    issueColumn.display({
      id: "actions",
      header: () => <span className="sr-only">Actions</span>,
      cell: c => (
        <Button
          variant="outline"
          size="sm"
          disabled={frozen}
          onClick={() => onEditIssue({
            number: c.row.original.number,
            title: c.row.original.title,
            state: c.row.original.state,
            url: c.row.original.url,
            authorLogin: c.row.original.authorLogin,
            labels: c.row.original.labels,
            assignees: c.row.original.assignees,
          })}
        >
          Edit<span className="sr-only"> issue #{c.row.original.number}</span>
        </Button>
      ),
    }),
  ]), [github, frozen, onEditIssue]);

  const table = useTable({ features, columns, data: issues });
  const th = "sticky top-0 z-[1] whitespace-nowrap border-b border-border bg-card px-3 py-2 text-left text-xs font-medium text-muted-foreground";

  function openByNumber(event: FormEvent) {
    event.preventDefault();
    const num = Number.parseInt(jumpNumber.trim(), 10);
    if (!Number.isInteger(num) || num <= 0) return;
    const matched = issues.find(item => item.number === num);
    setJumpNumber("");
    onEditIssue(matched
      ? { number: matched.number, title: matched.title, state: matched.state, url: matched.url, authorLogin: matched.authorLogin, labels: matched.labels, assignees: matched.assignees }
      : { number: num });
  }

  return (
    <div className="flex min-h-0 flex-1 gap-3 max-[900px]:flex-col">
      <section aria-label="Repository issues" className={`${card} flex min-h-0 flex-[1.35_1_480px] flex-col`}>
        <div className="flex flex-none flex-wrap items-center justify-between gap-2 border-b border-border px-4 py-3">
          <div>
            <h2 className="m-0 text-sm">Issues</h2>
            <p className={`m-0 text-xs ${muted}`}>GitHub issues are tasks. Changes go through Review → Submit → Confirmed.</p>
          </div>
          <div className="flex flex-wrap items-center gap-2">
            <form onSubmit={openByNumber} className="flex items-center gap-1.5">
              <input
                type="number"
                min={1}
                step={1}
                aria-label="Edit issue by number"
                placeholder="Issue #"
                value={jumpNumber}
                disabled={frozen}
                onChange={e => setJumpNumber(e.target.value)}
                className="w-24 rounded-lg border border-border bg-card px-2 py-1 text-xs"
              />
              <Button type="submit" variant="outline" size="sm" disabled={frozen || !jumpNumber.trim()}>Edit #</Button>
            </form>
            <Button size="sm" onClick={onNewIssue} disabled={frozen}>+ New issue</Button>
          </div>
        </div>
        <div className="min-h-0 flex-1 overflow-y-auto">
          {frozen ? (
            <p className="p-4">The issue list needs a live connection. Saved issue counts are available on Overview.</p>
          ) : query.isPending ? (
            <p role="status" className="p-4">Loading issues…</p>
          ) : query.isError ? (
            <p role="alert" className="p-4">{query.error.message} <Button variant="outline" size="sm" onClick={() => void query.refetch()}>Retry</Button></p>
          ) : (
            <>
              <table className="w-full border-collapse">
                <thead>{table.getHeaderGroups().map(group => <tr key={group.id}>{group.headers.map(h => <th key={h.id} className={th}><table.FlexRender header={h} /></th>)}</tr>)}</thead>
                <tbody>
                  {table.getRowModel().rows.map(row => (
                    <tr key={row.id} className="hover:bg-muted">
                      {row.getAllCells().map(cell => (
                        <td key={cell.id} className="border-b border-border px-3 py-[9px] align-middle"><table.FlexRender cell={cell} /></td>
                      ))}
                    </tr>
                  ))}
                  {!issues.length && <tr><td colSpan={columns.length} className={`p-6 text-center ${muted}`}>No imported issues yet.</td></tr>}
                </tbody>
              </table>
              {query.hasNextPage && (
                <div className="p-3">
                  <Button variant="outline" onClick={() => void query.fetchNextPage()} disabled={query.isFetching}>Load more issues</Button>
                </div>
              )}
            </>
          )}
        </div>
      </section>

      <div className="flex min-h-0 flex-[1_1_360px] flex-col">
        <WriteHistory
          repositoryId={id}
          viewerKey={viewerKey}
          demo={demo}
          frozen={frozen}
          onResolved={onResolved}
          onEditIssue={(issueNumber, confirmed) => onEditIssue({
            number: issueNumber,
            title: confirmed?.title,
            state: confirmed?.state,
            url: confirmed?.url,
            labels: confirmed?.labels,
            assignees: confirmed?.assignees,
          })}
        />
      </div>
    </div>
  );
}

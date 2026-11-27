import { useMemo, useSyncExternalStore, type ReactNode } from "react";
import { keepPreviousData, useQuery } from "@tanstack/react-query";
import { Link, createFileRoute } from "@tanstack/react-router";
import { Button } from "@/components/ui/button";
import { AssignedToMeList } from "@/components/assigned-to-me";
import { ChevronIcon, IssueIcon, NeedsAttentionList, PullRequestIcon, WorkflowIcon } from "@/components/needs-attention";
import { segGroupClass, segItemClass, warnNoteClass } from "@/components/repository-list";
import { LineChart, type ChartSeries } from "@/components/line-chart";
import { RepositoryAccessUnverifiedError, fetchCrossRepositoryOverview, type CrossRepositoryOverview, type MetricWindow } from "@/lib/api";
import {
  crossRepositoryOverviewKey,
  currentAccessSeq,
  filterAggregateByVisibility,
  selectVisibleRows,
  selectWithheldSummary,
  useSelectedRepositories,
  useSelectedRepositoryMetadata,
  useSelectionVisibility,
  viewerKeyOf,
} from "@/lib/selected-repositories";

const windows = [7, 30, 90] as const;

export const Route = createFileRoute("/_shell/")({
  validateSearch: (search: Record<string, unknown>): { days?: MetricWindow } => ({ days: search.days === 7 || search.days === 30 || search.days === 90 ? search.days : undefined }),
  component: Overview,
});

const card = "min-h-0 min-w-0 rounded-xl border border-border bg-card";
const plural = (n: number) => (n === 1 ? "repository" : "repositories");
const coverageLabels: Record<string, string> = {
  not_synchronized: "Awaiting data",
  window_before_boundary: "Window predates imported history",
  historical_gap: "Issue event history is incomplete",
  children_incomplete: "Related activity is still importing",
  access_changed: "Refreshing after access change",
  permission_denied: "Missing Actions read permission",
};

function Overview() {
  const { session } = Route.useRouteContext();
  const days = Route.useSearch().days ?? 30;
  const selection = useSelectedRepositories(session);
  const metadata = useSelectedRepositoryMetadata(session);
  const visibility = useSelectionVisibility(session);
  const rows = selectVisibleRows(metadata, selection, visibility);
  const { withheldCount: selectionWithheld, retryAt: selectionRetryAt } = selectWithheldSummary(metadata, selection, rows, visibility);
  const [fold, toggleFold] = useFold(`repoorbit:overview-fold:${viewerKeyOf(session)}`);
  const query = useQuery({
    queryKey: [...crossRepositoryOverviewKey(session), days],
    queryFn: async ({ signal }) => {
      const startedAtSeq = currentAccessSeq();
      const overview = await fetchCrossRepositoryOverview(days, signal);
      return { overview, startedAtSeq };
    },
    enabled: rows.length > 0 || selection.hasNextPage,
    retry: (count, error) => count < 1 && error instanceof RepositoryAccessUnverifiedError,
    retryDelay: 0,
    placeholderData: keepPreviousData,
  });

  if (selection.isPending) return <p role="status" className="p-5">Loading overview…</p>;
  if (selection.isError) return <div role="alert" className={`${card} m-auto p-4 text-center`}>
    <p>Could not load repositories. Check that the local services are running.</p>
    <Button className="mt-3" onClick={() => void selection.refetch()}>Retry</Button>
  </div>;
  const data = query.data ? filterAggregateByVisibility(query.data.overview, query.data.startedAtSeq, visibility) : undefined;
  const total = data ? data.repositoriesCounted + data.repositoriesSyncing : rows.length;
  const empty = selection.hasNextPage ? !!data && !query.isError && total === 0 : rows.length === 0;
  if (empty && (data?.withheld || data?.retryAt || selectionWithheld || selectionRetryAt)) return <div className="flex flex-col gap-3">
    <h1 className="text-xl">Engineering overview</h1>
    <WithheldNote withheld={data?.withheld ?? selectionWithheld} retryAt={data?.retryAt ?? selectionRetryAt} />
  </div>;
  if (empty) return <div className="grid h-full min-h-0 place-items-center">
    <div className={`${card} max-w-[420px] p-4 text-center`}>
      <h2 className="text-lg">No repositories yet</h2>
      <p className="text-muted-foreground">Check repositories to sync under Repositories.</p>
      <Button asChild className="mt-3"><Link to="/repositories">Choose repositories</Link></Button>
    </div>
  </div>;

  const syncing = data?.repositoriesSyncing ?? 0;
  return <div className="flex h-full min-h-0 flex-col gap-3 max-[900px]:overflow-y-auto">
    <div className="flex shrink-0 flex-wrap items-end justify-between gap-2">
      <div>
        <h1 className="text-xl">Engineering overview</h1>
        <p className="m-0 text-muted-foreground">Activity and delivery across {total} {plural(total)}{syncing ? ` · ${syncing} still syncing (left out)` : ""}</p>
      </div>
      <nav aria-label="Time window" className={segGroupClass}>
        {windows.map(w => <Link key={w} to="/" search={{ days: w }} aria-current={days === w ? "page" : undefined} className={segItemClass}>{w}d</Link>)}
      </nav>
    </div>
    {!query.isError && data && <WithheldNote withheld={data.withheld} retryAt={data.retryAt} />}
    {query.isPending ? <p role="status">Loading overview…</p>
      : query.isError ? <div role="alert" className={`${card} p-4`}>
        <p className="font-medium">{query.error instanceof RepositoryAccessUnverifiedError ? "Access could not be verified. Totals are hidden until GitHub confirms your access." : query.error.message}</p>
        <Button className="mt-3" onClick={() => void query.refetch()} disabled={query.isFetching}>Retry</Button>
      </div>
      : data && <>
        <section aria-label="Totals" className={`${card} grid shrink-0 grid-cols-[repeat(auto-fit,minmax(150px,1fr))]`}>
          <Stat icon={<IssueIcon />} metric={data.openIssues} label="Open issues" sub="across repositories" />
          <Stat icon={<PullRequestIcon />} warn={!!data.reviewBacklog.value} metric={data.reviewBacklog} label="Review backlog" sub="PRs awaiting review" />
          <Stat icon={<WorkflowIcon />} warn={!!data.failingWorkflows.value} metric={data.failingWorkflows} label="Failing workflows" sub="30d latest failed" />
          <RateStat icon={<WorkflowIcon />} metric={data.buildSuccessRate} label="Build success" sub="30d completed runs" />
        </section>
        <div className="grid min-h-0 flex-1 grid-cols-[minmax(0,1.25fr)_minmax(0,1fr)] gap-3 max-[900px]:flex max-[900px]:flex-col">
          <div className="flex min-h-0 flex-col gap-3">
            <Panel id="attn" title="Needs attention" count={data.needsAttention.length} fold={fold} onToggle={toggleFold}>
              <NeedsAttentionList items={data.needsAttention} days={days} />
            </Panel>
            <Panel id="act" title="Engineering activity" fold={fold} onToggle={toggleFold} extra={<span className="text-xs text-muted-foreground">last {days} days</span>}>
              <ActivityChart daily={data.daily} />
            </Panel>
          </div>
          <div className="flex min-h-0 flex-col gap-3">
            <Panel id="mine" title="Assigned to me" count={data.assignedToMe.length} fold={fold} onToggle={toggleFold}>
              <AssignedToMeList issues={data.assignedToMe} days={days} />
            </Panel>
          </div>
        </div>
      </>}
  </div>;
}

function WithheldNote({ withheld, retryAt }: { withheld: number; retryAt: string | null }) {
  if (!withheld && !retryAt) return null;
  return <p role="status" className={warnNoteClass}>
    {withheld > 0 && `${withheld} ${plural(withheld)} hidden (access could not be verified).`}
    {retryAt && ` GitHub rate limited the access check; retry after ${new Date(retryAt).toLocaleTimeString()}.`}
  </p>;
}

const foldMemory = new Map<string, string>();
const foldEvent = "repoorbit:overview-fold";
const subscribeFold = (notify: () => void) => { window.addEventListener(foldEvent, notify); return () => window.removeEventListener(foldEvent, notify); };
function useFold(key: string) {
  const raw = useSyncExternalStore(subscribeFold, () => {
    try { return localStorage.getItem(key) ?? foldMemory.get(key) ?? "{}"; } catch { return foldMemory.get(key) ?? "{}"; }
  }, () => "{}");
  const fold = useMemo<Record<string, boolean>>(() => { try { return JSON.parse(raw); } catch { return {}; } }, [raw]);
  const toggle = (id: string) => {
    const next = JSON.stringify({ ...fold, [id]: !fold[id] });
    foldMemory.set(key, next);
    try { localStorage.setItem(key, next); } catch { /* storage unavailable: memory keeps it for this visit */ }
    window.dispatchEvent(new Event(foldEvent));
  };
  return [fold, toggle] as const;
}

function Panel({ id, title, count, extra, fold, onToggle, children }: { id: string; title: string; count?: number; extra?: ReactNode; fold: Record<string, boolean>; onToggle: (id: string) => void; children: ReactNode }) {
  const folded = !!fold[id];
  return <section aria-label={title} className={`${card} flex flex-col ${folded ? "flex-none" : "flex-[1_1_0]"} max-[900px]:flex-none`}>
    <header className="flex shrink-0 items-center gap-2 py-2 pr-3.5 pl-2">
      <button type="button" aria-expanded={!folded} onClick={() => onToggle(id)} className="group flex min-w-0 flex-1 items-center gap-2 rounded-md px-1.5 py-1 text-left hover:bg-muted">
        <ChevronIcon className="rotate-90 transition-transform group-aria-[expanded=false]:rotate-0 motion-reduce:transition-none" />
        <h2 className="text-sm">{title}</h2>
        {count != null && <span className="inline-flex items-center rounded-full border border-border px-[7px] text-[11px] text-soft">{count}</span>}
      </button>
      {!folded && extra}
    </header>
    {!folded && <div className="flex min-h-0 flex-1 flex-col overflow-y-auto [scrollbar-width:thin] max-[900px]:max-h-80">{children}</div>}
  </section>;
}

function StatTile({ icon, display, label, detail, warn }: { icon: ReactNode; display: ReactNode; label: string; detail: string; warn?: boolean }) {
  return <div className="flex flex-col gap-0.5 border-l border-border px-4 py-3 first:border-l-0">
    <span className={warn ? "text-warning" : "text-muted-foreground"}>{icon}</span>
    <b className="num text-2xl leading-tight font-semibold tracking-[-.02em]">{display}</b>
    <div className="text-[12.5px] font-medium">{label}</div>
    <div className="text-[11.5px] text-muted-foreground">{detail}</div>
  </div>;
}

function coverageFallback(reason: string | null) {
  return coverageLabels[reason ?? ""] ?? "Coverage is incomplete";
}

function Stat({ icon, metric, label, sub, warn }: { icon: ReactNode; metric: CrossRepositoryOverview["openIssues"]; label: string; sub: string; warn?: boolean }) {
  const known = metric.value !== null;
  return <StatTile icon={icon} display={known ? metric.value : "—"} label={label} detail={known ? sub : coverageFallback(metric.coverage.reason)} warn={warn} />;
}

function RateStat({ icon, metric, label, sub }: { icon: ReactNode; metric: CrossRepositoryOverview["buildSuccessRate"]; label: string; sub: string }) {
  const covered = metric.coverage.covered;
  const display = covered && metric.rate !== null ? `${(metric.rate * 100).toFixed(1)}%` : "—";
  const detail = covered ? (metric.denominator ? `${metric.numerator ?? 0} of ${metric.denominator} completed` : sub) : coverageFallback(metric.coverage.reason);
  return <StatTile icon={icon} display={display} label={label} detail={detail} />;
}

function ActivityChart({ daily }: { daily: CrossRepositoryOverview["daily"] }) {
  const days = useMemo(() => daily.map(d => d.day), [daily]);
  const series = useMemo<ChartSeries[]>(() => [
    { name: "PRs merged", unit: "PRs merged", color: "--primary", values: daily.map(d => d.pullRequestsMerged) },
    { name: "Issues opened", unit: "issues opened", color: "--chart-2", values: daily.map(d => d.issuesOpened) },
  ], [daily]);
  return <div className="flex h-full min-h-0 flex-col gap-1.5 px-3.5 pb-3">
    <span className="flex flex-wrap justify-end gap-3.5 text-xs text-muted-foreground">
      <span><i className="mr-1.5 inline-block size-2 rounded-full bg-primary" />PRs merged</span>
      <span><i className="mr-1.5 inline-block size-2 rounded-full bg-chart-2" />Issues opened</span>
    </span>
    <LineChart days={days} series={series} />
  </div>;
}

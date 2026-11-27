import { Link, createFileRoute } from "@tanstack/react-router";
import { ActivityChart } from "@/features/analytics/components/activity-chart";
import { AssignedToMeList } from "@/features/analytics/components/assigned-to-me";
import { NeedsAttentionList } from "@/features/analytics/components/needs-attention";
import { Panel, panelCard } from "@/features/analytics/components/panel";
import { RateStat, Stat } from "@/features/analytics/components/stat";
import { WithheldNote } from "@/features/analytics/components/withheld-note";
import { filterAggregateByVisibility } from "@/features/analytics/filter-aggregate";
import { plural } from "@/features/analytics/format";
import { useCrossRepositoryOverview } from "@/features/analytics/hooks/use-cross-repository-overview";
import { useFold } from "@/features/analytics/hooks/use-fold";
import {
  selectVisibleRows,
  selectWithheldSummary,
  useSelectedRepositories,
  useSelectedRepositoryMetadata,
  useSelectionVisibility,
  viewerKeyOf,
} from "@/features/repositories/access-store";
import { segGroupClass, segItemClass } from "@/features/repositories/repository-list";
import { RepositoryAccessUnverifiedError } from "@/shared/api/errors";
import type { MetricWindow } from "@/shared/api/transport";
import { Button } from "@/shared/ui/button";
import { IssueIcon, PullRequestIcon, WorkflowIcon } from "@/shared/ui/icons";

const windows = [7, 30, 90] as const;

export const Route = createFileRoute("/_shell/")({
  validateSearch: (search: Record<string, unknown>): { days?: MetricWindow } => ({ days: search.days === 7 || search.days === 30 || search.days === 90 ? search.days : undefined }),
  component: Overview,
});

function Overview() {
  const { session } = Route.useRouteContext();
  const days = Route.useSearch().days ?? 30;
  const selection = useSelectedRepositories(session);
  const metadata = useSelectedRepositoryMetadata(session);
  const visibility = useSelectionVisibility(session);
  const rows = selectVisibleRows(metadata, selection, visibility);
  const { withheldCount: selectionWithheld, retryAt: selectionRetryAt } = selectWithheldSummary(metadata, selection, rows, visibility);
  const [fold, toggleFold] = useFold(`repoorbit:overview-fold:${viewerKeyOf(session)}`);
  const query = useCrossRepositoryOverview(session, days, rows.length > 0 || selection.hasNextPage);

  if (selection.isPending) return <p role="status" className="p-5">Loading overview…</p>;
  if (selection.isError) return <div role="alert" className={`${panelCard} m-auto p-4 text-center`}>
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
    <div className={`${panelCard} max-w-[420px] p-4 text-center`}>
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
      : query.isError ? <div role="alert" className={`${panelCard} p-4`}>
        <p className="font-medium">{query.error instanceof RepositoryAccessUnverifiedError ? "Access could not be verified. Totals are hidden until GitHub confirms your access." : query.error.message}</p>
        <Button className="mt-3" onClick={() => void query.refetch()} disabled={query.isFetching}>Retry</Button>
      </div>
      : data && <>
        <section aria-label="Totals" className={`${panelCard} grid shrink-0 grid-cols-[repeat(auto-fit,minmax(150px,1fr))]`}>
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

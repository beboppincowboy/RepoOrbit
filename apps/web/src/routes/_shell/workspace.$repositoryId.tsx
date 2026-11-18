import { useQuery } from "@tanstack/react-query";
import { createFileRoute, Link } from "@tanstack/react-router";
import { describeActivity, SyncStatus } from "@/components/sync-status";
import { Button } from "@/components/ui/button";
import { fetchRepositoryOverview, fetchWorkspacePullRequests, type MetricWindow } from "@/lib/api";

const tabs = { overview: "Overview", pulls: "Pull Requests", contributors: "Contributors", activity: "Activity" } as const;
type Tab = keyof typeof tabs;
const windows = [7, 30, 90] as const;

export const Route = createFileRoute("/_shell/workspace/$repositoryId")({
  validateSearch: (search: Record<string, unknown>): { days: MetricWindow; tab: Tab } => ({
    days: search.days === 7 || search.days === 90 ? search.days : 30,
    tab: typeof search.tab === "string" && Object.hasOwn(tabs, search.tab) ? search.tab as Tab : "overview",
  }),
  component: RepositoryWorkspace,
});

type Overview = NonNullable<Awaited<ReturnType<typeof fetchRepositoryOverview>>>;
const card = "rounded-xl border border-border bg-card";
const muted = "text-xs text-muted-foreground";
const date = new Intl.DateTimeFormat("en", { month: "short", day: "numeric", timeZone: "UTC" });
const coverageLabels: Record<string, string> = {
  not_synchronized: "Awaiting data",
  window_before_boundary: "Window predates imported history",
  historical_gap: "Issue event history is incomplete",
  children_incomplete: "Related activity is still importing",
};

function RepositoryWorkspace() {
  const { repositoryId } = Route.useParams();
  const search = Route.useSearch();
  const { session } = Route.useRouteContext();
  const query = useQuery({
    queryKey: ["repositoryOverview", session.mode, session.user?.login, repositoryId, search.days],
    queryFn: ({ signal }) => fetchRepositoryOverview(repositoryId, search.days, signal),
    staleTime: 0,
    gcTime: 0,
  });
  const path = { to: Route.to, params: { repositoryId } } as const;

  if (query.isPending) return <p role="status" className="p-5">Loading repository workspace…</p>;
  if (query.isError || !query.data) return <div className={`${card} m-4 p-5`}>
    <h1 className="font-semibold">{query.error?.message ?? "Repository unavailable"}</h1>
    {query.isError && <Button className="mt-3" onClick={() => void query.refetch()}>Retry</Button>}
  </div>;
  const overview = query.data;
  const [owner, name] = overview.fullName.split("/");
  const github = `https://github.com/${encodeURIComponent(owner)}/${encodeURIComponent(name)}`;
  const unanswered = overview.issuesWithoutResponse.value;
  const selectedClass = "rounded-full px-2.5 py-1 aria-[current=page]:bg-primary/10 aria-[current=page]:font-semibold aria-[current=page]:text-primary";
  const coverage = [overview.firstResponseTime.coverage, overview.mergeTime.coverage, overview.openIssues.coverage, overview.pullRequestsMerged.coverage];
  const missing = coverage.filter(item => !item.covered).length;
  const syncDetails = session.mode === "demo" ? [] : describeActivity(overview.activity);

  return <div className="flex h-full min-w-0 flex-col gap-3 overflow-y-auto text-sm">
    <header className={`${card} sticky top-0 z-10 shrink-0 px-4 pt-3`}>
      <div className="flex flex-wrap items-center gap-x-3 gap-y-2">
        <span className="grid size-8 place-items-center rounded-lg bg-muted font-mono text-primary" aria-hidden="true">▤</span>
        <div className="min-w-0 break-words"><p className={muted}>{owner} /</p><h1 className="text-xl leading-tight font-semibold">{name}</h1></div>
        {session.mode !== "demo" && <a className="rounded-md border border-border px-2 py-1 text-xs hover:bg-muted" href={github}>View on GitHub ↗</a>}
        <SyncStatus compact syncState={overview.syncState} activity={session.mode === "demo" ? null : overview.activity} />
        {overview.activity.archived && <span className={muted}>Archived</span>}
        <nav aria-label="Activity window" className="ml-auto flex rounded-full border border-border p-0.5 text-xs">
          {windows.map(days => <Link key={days} {...path} search={{ ...search, days }} aria-current={search.days === days ? "page" : undefined} className={selectedClass}>{days}d</Link>)}
        </nav>
      </div>
      <nav aria-label="Workspace sections" className="mt-2 flex flex-wrap gap-x-4">
        {(Object.keys(tabs) as Tab[]).map(tab => <Link key={tab} {...path} search={{ ...search, tab }}
          aria-current={search.tab === tab ? "page" : undefined}
          className="border-b-2 border-transparent py-2 text-muted-foreground aria-[current=page]:border-foreground aria-[current=page]:font-semibold aria-[current=page]:text-foreground">{tabs[tab]}</Link>)}
      </nav>
    </header>

    {missing > 0 && <p role="status" className={`rounded-lg border px-3 py-2 ${missing === coverage.length ? "border-border bg-muted text-muted-foreground" : "border-warning/25 bg-warning/10 text-warning"}`}>
      {missing === coverage.length ? "Activity metrics are awaiting synchronized data." : "Partial coverage. Some metrics are unavailable for this window."}
    </p>}
    {search.tab === "overview" && <>
      <section aria-label="Repository metrics" className="grid grid-cols-[repeat(auto-fit,minmax(min(150px,100%),1fr))] gap-3">
        <Metric label="Median first response" value={duration(overview.firstResponseTime.seconds)} coverage={overview.firstResponseTime.coverage} detail={unanswered === null ? undefined : `${unanswered} without a response`} />
        <Metric label="Median merge time" value={duration(overview.mergeTime.seconds)} coverage={overview.mergeTime.coverage} detail={sample(overview.mergeTime.sampleSize)} />
        <Metric label="Open issues" value={overview.openIssues.value} coverage={overview.openIssues.coverage} detail={overview.staleOpenIssues.value === null ? undefined : `${overview.staleOpenIssues.value} stale`} />
        <Metric label="Merged PRs" value={overview.pullRequestsMerged.value} coverage={overview.pullRequestsMerged.coverage} detail={`Last ${search.days} days`} />
      </section>
      <div className="grid min-w-0 gap-3 min-[901px]:min-h-80 min-[901px]:flex-1 min-[901px]:grid-cols-[minmax(0,7fr)_minmax(260px,3fr)]">
        <Trend days={overview.daily} />
        <section className={card}>
          <header className="flex items-center justify-between px-4 pt-3"><h2 className="font-semibold">Recent activity</h2>
            <Link {...path} search={{ ...search, tab: "activity" }} className="rounded-md border border-border px-2 py-1 text-xs hover:bg-muted">All</Link>
          </header>
          <Timeline entries={overview.timeline.slice(0, 5)} github={github} />
        </section>
      </div>
    </>}
    {search.tab === "pulls" && <PullRequests key={repositoryId} id={repositoryId} viewer={[session.mode, session.user?.login]} />}
    {search.tab === "contributors" && <section className={`${card} p-4`}>
      <h2 className="font-semibold">Participation · last {search.days} days</h2>
      <p className={muted}>{overview.activeBotContributors.value === null ? "Bot participation unavailable" : `${overview.activeBotContributors.value} active bots`}</p>
      {overview.contributors.length ? <ul className="mt-2 divide-y divide-border">
        {overview.contributors.map(person => <li key={person.login} className="flex flex-wrap justify-between gap-2 py-2">
          <span className="font-medium">{person.login}</span><span className={muted}>{person.pullRequestsOpened} PRs opened · {person.reviewsSubmitted} reviews submitted</span>
        </li>)}
      </ul> : <p className="mt-3 text-muted-foreground">{overview.contributorsCoverage.covered ? "No contributor activity in this window." : coverageLabels[overview.contributorsCoverage.reason ?? ""]}</p>}
    </section>}
    {search.tab === "activity" && <section className={card}>
      {(!overview.eventsCoveredFrom || Date.parse(overview.eventsCoveredFrom) > Date.parse(overview.windowStart)) && <p className="border-b border-border bg-muted px-4 py-2 text-xs text-muted-foreground">
        {overview.eventsCoveredFrom ? `Issue event history starts ${date.format(new Date(overview.eventsCoveredFrom))}.` : "Issue event history is unavailable."}
      </p>}
      <Timeline entries={overview.timeline} github={github} />
    </section>}
    {syncDetails.length > 0 && <details className={`${card} px-3 py-2 text-xs text-muted-foreground`}>
      <summary className="cursor-pointer font-medium">Activity sync details</summary>
      <ul className="mt-2 space-y-1">{syncDetails.map(line => <li key={line}>{line}</li>)}</ul>
    </details>}
  </div>;
}

function duration(seconds: number | null) {
  if (seconds === null) return null;
  if (seconds >= 86400) return `${Math.floor(seconds / 86400)}d ${Math.floor(seconds / 3600) % 24}h`;
  if (seconds >= 3600) return `${Math.floor(seconds / 3600)}h ${Math.floor(seconds / 60) % 60}m`;
  return seconds < 60 ? `${seconds}s` : `${Math.floor(seconds / 60)}m ${seconds % 60}s`;
}

function sample(size: number | null) {
  return size === 0 ? "No qualifying samples" : size === null ? undefined : `${size} qualifying samples`;
}

function Metric({ label, value, coverage, detail }: { label: string; value: string | number | null; coverage: Overview["openIssues"]["coverage"]; detail?: string }) {
  return <div className={`${card} min-w-0 p-3`}>
    <h2 className="text-xs font-medium text-muted-foreground">{label}</h2>
    <p className="py-1 text-2xl font-semibold tabular-nums">{value ?? "—"}</p>
    <p className={muted} title={coverage.through ? `Covered through ${coverage.through}` : undefined}>
      {coverage.covered ? detail : coverageLabels[coverage.reason ?? ""]}
    </p>
  </div>;
}

function Trend({ days }: { days: Overview["daily"] }) {
  const lines = [
    { label: "Issues opened", color: "var(--primary)", points: days.map(day => day.issuesOpened) },
    { label: "PRs merged", color: "var(--success)", points: days.map(day => day.pullRequestsMerged) },
  ];
  const max = Math.max(1, ...lines.flatMap(line => line.points.map(value => value ?? 0)));
  return <section className={`${card} flex min-w-0 flex-col p-4`}>
    <div className="flex flex-wrap justify-between gap-2"><h2 className="font-semibold">Opened vs merged</h2>
      <div className="flex flex-wrap gap-3 text-xs text-muted-foreground">{lines.map(line => <span key={line.label} className="flex items-center gap-1.5">
        <i aria-hidden="true" className="size-2 rounded-full" style={{ background: line.color }} />{line.label}
        {line.points[0] === null && " (unavailable)"}
      </span>)}</div>
    </div>
    <svg viewBox="0 0 500 160" preserveAspectRatio="none" className="mt-4 h-40 min-h-40 w-full min-[901px]:flex-1" role="img" aria-label="Daily issues opened and pull requests merged. Values available in the data table.">
      {[20, 60, 100, 140].map(y => <line key={y} x1="0" x2="500" y1={y} y2={y} stroke="var(--border)" />)}
      {lines.filter(line => line.points[0] !== null).map(line => <polyline key={line.label} fill="none" stroke={line.color} strokeWidth="2" vectorEffect="non-scaling-stroke"
        points={line.points.map((point, index) => `${500 * index / (days.length - 1)},${140 - (point ?? 0) * 120 / max}`).join(" ")} />)}
    </svg>
    <div className="flex justify-between font-mono text-xs text-muted-foreground">
      <span>{date.format(new Date(days[0].day))}</span><span>{date.format(new Date(days[days.length - 1].day))}</span>
    </div>
    <details className="mt-2 text-xs text-muted-foreground"><summary className="cursor-pointer">Daily values</summary>
      <table className="mt-2 w-full text-left"><thead><tr><th>UTC day</th><th>Issues opened</th><th>PRs merged</th></tr></thead>
        <tbody>{days.map(day => <tr key={day.day}><th scope="row">{date.format(new Date(day.day))}</th><td>{day.issuesOpened ?? "—"}</td><td>{day.pullRequestsMerged ?? "—"}</td></tr>)}</tbody>
      </table>
    </details>
  </section>;
}

function Timeline({ entries, github }: { entries: Overview["timeline"]; github: string }) {
  if (!entries.length) return <p className="px-4 py-3 text-muted-foreground">No activity available in this window.</p>;
  return <ul className="divide-y divide-border px-4">
    {entries.map(entry => {
      const href = entry.number === null
        ? entry.url.startsWith(`${github}/releases/`) ? entry.url : `${github}/releases`
        : `${github}/${entry.kind === "pull_request_merged" ? "pull" : "issues"}/${entry.number}`;
      return <li className="flex items-start gap-3 py-2.5" key={`${entry.kind}-${entry.sourceId}`}>
        <span aria-hidden="true" className="rounded-md bg-muted px-2 py-1 font-mono text-primary">↳</span>
        <div className="min-w-0 flex-1 break-words"><a href={href} className="font-medium hover:underline">{entry.title || entry.kind.replaceAll("_", " ")}</a>
          <p className={muted}>{entry.kind.replaceAll("_", " ")}{entry.number !== null && ` #${entry.number}`}{entry.actor && ` · ${entry.actor.login}${entry.actor.isBot ? " (bot)" : ""}`}</p>
        </div>
        <time className={muted} dateTime={entry.occurredAt}>{date.format(new Date(entry.occurredAt))}</time>
      </li>;
    })}
  </ul>;
}

function PullRequests({ id, viewer }: { id: string; viewer: (string | undefined)[] }) {
  const query = useQuery({
    queryKey: ["workspacePullRequests", id, ...viewer],
    queryFn: ({ signal }) => fetchWorkspacePullRequests(id, signal),
    gcTime: 0,
  });
  return <section className={`${card} p-4`}>
    {query.isPending ? <p role="status">Loading pull requests…</p> : query.isError ? <p role="alert">{query.error.message}</p> :
      query.data.nodes.length ? <>
        <table className="w-full text-left"><thead><tr><th>PR</th><th>Title</th><th>State</th></tr></thead><tbody className="divide-y divide-border">
          {query.data.nodes.map(pr => <tr key={pr.id}><td className="py-2 pr-2">#{pr.number}</td><td className="pr-2">{pr.title}</td><td>{pr.mergedAt ? "Merged" : pr.state}</td></tr>)}
        </tbody></table>
        {query.data.hasNextPage && <p className={muted}>Showing the first 20 imported pull requests.</p>}
      </> : <p className="text-muted-foreground">No imported pull requests.</p>}
  </section>;
}

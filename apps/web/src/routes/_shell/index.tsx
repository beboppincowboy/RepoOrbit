import { useInfiniteQuery } from "@tanstack/react-query";
import { Link, createFileRoute } from "@tanstack/react-router";
import { Button } from "@/components/ui/button";
import { RefreshActivityButton, SyncStatus } from "@/components/sync-status";
import { selectedRepositories } from "@/lib/api";

export const Route = createFileRoute("/_shell/")({ component: Overview });

function Overview() {
  const { session } = Route.useRouteContext();
  const demo = session.mode === "demo";
  const query = useInfiniteQuery({
    queryKey: ["selectedRepositories"],
    queryFn: ({ pageParam }) => selectedRepositories(pageParam),
    initialPageParam: null as string | null,
    getNextPageParam: last => (last.hasNextPage ? last.endCursor : undefined),
  });
  const pages = query.data?.pages ?? [];
  const nodes = pages.flatMap(p => p.nodes);
  const withheld = pages.reduce((sum, p) => sum + p.withheld, 0);
  const retryAt = pages.map(p => p.retryAt).filter(Boolean).at(-1) ?? null;
  const loading = query.isPending || query.isFetchingNextPage;
  const error = query.isError;

  return <div className="flex h-full min-h-0 flex-col">
    <header className="shrink-0 border-b border-border px-6 py-4">
      <p className="text-xs font-semibold uppercase tracking-wider text-primary">RepoOrbit{demo ? " · Demo" : ""}</p>
      <h1 className="mt-1 text-2xl font-bold">Repository overview</h1>
      <p className="mt-1 max-w-2xl text-sm text-muted-foreground">{demo ? "Sample data stored in the local database. This is not current GitHub data; synchronization and history are not available in this demo." : "Repositories you connect appear here. Activity for each stream (issues, pull requests, reviews, comments, releases, issue events) imports separately from metadata; its status is shown below."}</p>
    </header>
    <div className="relative min-h-0 flex-1 overflow-y-auto px-6 py-4">
      {loading && <p role="status">Loading repositories…</p>}
      {error && <p role="alert" className="text-destructive">Could not load repositories. Check that the local services are running.</p>}
      {(withheld > 0 || retryAt) && <p role="status" className="mb-4 rounded border border-warning/40 bg-warning/10 p-3">{withheld} connected {withheld === 1 ? "repository is" : "repositories are"} hidden because GitHub access could not be verified{retryAt ? ` (rate limited; retry after ${new Date(retryAt).toLocaleTimeString()})` : ""}. This is not an empty list; reload to retry.</p>}
      {!loading && !error && nodes.length === 0 && !withheld && !retryAt && <p>{demo ? "No demo repositories yet." : <>No repositories connected yet. <Link className="underline" to="/repositories">Choose repositories to connect</Link>.</>}</p>}
      <div className="grid gap-4 md:grid-cols-2">{nodes.map(repo => <section key={repo.id} className="rounded-lg border border-border bg-card p-5">
        <h2 className="text-lg font-semibold">{repo.fullName}</h2>
        <p className="mt-1 text-sm text-muted-foreground">{repo.description}</p>
        <div className="mt-3"><SyncStatus syncState={repo.syncState} activity={demo ? null : repo.activity} metadataStatus={repo.syncState === "awaiting-sync" ? repo.metadataStatus : null} withheld={repo.issues.withheld} retryAt={repo.issues.retryAt} /></div>
        {!demo && repo.syncState !== "demo" && repo.activity && <RefreshActivityButton repositoryId={repo.id} />}
        <h3 className="mt-5 font-semibold">{demo ? "Sample issues" : "Issues"}</h3>
        {repo.issues.nodes.length ? <ul className="mt-2 space-y-2">{repo.issues.nodes.map(issue => <li key={issue.id}>{issue.title} <span className="text-sm text-muted-foreground">({issue.state})</span></li>)}</ul> : <p className="mt-2 text-muted-foreground">{repo.issues.withheld ? "Issues are hidden because access could not be verified." : demo ? "No demo issues." : repo.activity?.issues?.phase === "current" ? "No issues found in the covered period." : "Issue import is incomplete; the issue count is unknown."}</p>}
        {repo.issues.hasNextPage && <p className="mt-2 text-sm text-muted-foreground">Partial issue list; more issues are available through the API.</p>}
      </section>)}</div>
      {query.hasNextPage && <Button className="mt-6" onClick={() => query.fetchNextPage()} disabled={loading}>Load more repositories</Button>}
    </div>
  </div>;
}

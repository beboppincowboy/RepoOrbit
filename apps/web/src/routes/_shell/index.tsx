import { useEffect, useRef, type ReactNode } from "react";
import { useInfiniteQuery, useMutation, useQueryClient, type InfiniteData } from "@tanstack/react-query";
import { Link, createFileRoute } from "@tanstack/react-router";
import { Button } from "@/components/ui/button";
import { RefreshActivityButton, SyncStatus } from "@/components/sync-status";
import { fetchRepositoryDetails, fetchSelectedRepositories } from "@/lib/api";

export const Route = createFileRoute("/_shell/")({ component: Overview });

type SelectedRepositoryPage = Awaited<ReturnType<typeof fetchSelectedRepositories>>;
type SelectedRepositoryBatch = SelectedRepositoryPage["batches"][number];

function Overview() {
  const { session } = Route.useRouteContext();
  const demo = session.mode === "demo";
  const viewer = session.user?.login;
  const selectedRepositoriesQueryKey = ["selectedRepositories", session.mode, viewer] as const;
  const retryController = useRef<AbortController | null>(null);
  useEffect(() => {
    const cancelRetry = () => retryController.current?.abort();
    window.addEventListener("pagehide", cancelRetry);
    return () => {
      window.removeEventListener("pagehide", cancelRetry);
      cancelRetry();
    };
  }, [session.mode, viewer]);
  const queryClient = useQueryClient();
  const query = useInfiniteQuery({
    queryKey: selectedRepositoriesQueryKey,
    queryFn: ({ pageParam, signal }) => fetchSelectedRepositories(pageParam, signal),
    initialPageParam: null as string | null,
    getNextPageParam: last => (last.hasNextPage ? last.endCursor : undefined),
  });
  const retry = useMutation({
    mutationFn: async (batch: SelectedRepositoryBatch) => {
      const controller = new AbortController();
      retryController.current = controller;
      const details = await fetchRepositoryDetails(batch.repositories.map(repo => repo.id), controller.signal);
      controller.signal.throwIfAborted();
      queryClient.setQueryData<InfiniteData<SelectedRepositoryPage>>(selectedRepositoriesQueryKey, old => old && {
        ...old,
        pages: old.pages.map(page => ({
          ...page,
          batches: page.batches.map(current => current === batch ? { ...current, details } : current),
        })),
      });
    },
  });
  // A failed revalidation can no longer establish access to the cached rows.
  const pages = query.isRefetchError ? [] : query.data?.pages ?? [];
  const batches = pages.flatMap(p => p.batches);
  const busy = query.isFetching || retry.isPending;
  const withheld = pages.reduce((sum, p) => sum + p.withheld, 0);
  const retryAt = pages.map(p => p.retryAt).filter(Boolean).at(-1) ?? null;
  const loading = query.isPending || query.isFetchingNextPage;
  const error = query.isError;

  return <div className="flex h-full min-h-0 flex-col">
    <header className="shrink-0 border-b border-border px-6 py-4">
      <p className="text-xs font-semibold uppercase tracking-wider text-primary">RepoOrbit{demo ? " · Demo" : ""}</p>
      <h1 className="mt-1 text-2xl font-bold">Repository overview</h1>
      <p className="mt-1 max-w-2xl text-sm text-muted-foreground">{demo ? "Sample data stored in the local database. This is not current GitHub data; synchronization and history are not available in this demo." : "Repositories you connect appear here. Activity for each stream (issues, pull requests, reviews, comments, releases, issue events) imports separately from metadata; its status is shown below."}</p>
      {!demo && <Button className="mt-3" onClick={() => query.refetch()} disabled={busy}>Refresh repositories</Button>}
    </header>
    <div className="relative min-h-0 flex-1 overflow-y-auto px-6 py-4">
      {loading && <p role="status">Loading repositories…</p>}
      {error && <div role="alert" className="text-destructive">
        <p>Could not load repositories. Check that the local services are running.</p>
        {demo && !query.isFetchNextPageError && <Button onClick={() => query.refetch()} disabled={busy}>Retry repository list</Button>}
      </div>}
      {(withheld > 0 || retryAt) && <p role="status" className="mb-4 rounded border border-warning/40 bg-warning/10 p-3">{withheld} connected {withheld === 1 ? "repository is" : "repositories are"} hidden because GitHub access could not be verified{retryAt ? ` (rate limited; retry after ${new Date(retryAt).toLocaleTimeString()})` : ""}. This is not an empty list; reload to retry.</p>}
      {!loading && !error && batches.length === 0 && !withheld && !retryAt && <p>{demo ? "No demo repositories yet." : <>No repositories connected yet. <Link className="underline" to="/repositories">Choose repositories to connect</Link>.</>}</p>}
      <div className="grid gap-4 md:grid-cols-2">{batches.map(batch => {
        const { repositories, details } = batch;
        return <RepositoryBatch key={repositories[0].id} batch={batch} demo={demo}>
          {!details && <div className="col-span-full">
            <p role="alert" className="text-destructive">Could not load repository details for this batch.</p>
            <Button onClick={() => retry.mutate(batch)} disabled={busy}>{retry.isPending && retry.variables === batch ? "Retrying details…" : "Retry details"}</Button>
          </div>}
        </RepositoryBatch>;
      })}</div>
      {query.hasNextPage && !query.isRefetchError && <Button className="mt-6" onClick={() => query.fetchNextPage()} disabled={busy}>Load more repositories</Button>}
    </div>
  </div>;
}

function RepositoryBatch({ batch, demo, children }: { batch: SelectedRepositoryBatch; demo: boolean; children: ReactNode }) {
  return <>
    {children}
    {batch.repositories.map((repo, index) => {
      const detail = batch.details?.[index];
      return <section key={repo.id} className="rounded-lg border border-border bg-card p-5">
        {detail?.state === "access-unverified" ? <>
          <h2 className="text-lg font-semibold">Repository unavailable</h2>
          <SyncStatus syncState={repo.syncState} withheld={1} retryAt={detail.retryAt} />
          <p className="mt-2 text-sm text-muted-foreground">Details are hidden because access could not be verified.</p>
        </> : <>
          <h2 className="text-lg font-semibold">
            <Link to="/workspace/$repositoryId" params={{ repositoryId: repo.id }} search={{ days: 30, tab: "overview" }} className="hover:underline">{repo.fullName}</Link>
          </h2>
          {detail && <>
            <p className="mt-1 text-sm text-muted-foreground">{repo.description}</p>
            <div className="mt-3"><SyncStatus syncState={repo.syncState} activity={demo ? null : detail.activity} metadataStatus={repo.syncState === "awaiting-sync" ? repo.metadataStatus : null} retryAt={detail.issues.retryAt} /></div>
            {!demo && detail.activity && <RefreshActivityButton repositoryId={repo.id} />}
            <h3 className="mt-5 font-semibold">{demo ? "Sample issues" : "Issues"}</h3>
            {detail.issues.nodes.length ? <ul className="mt-2 space-y-2">{detail.issues.nodes.map(issue => <li key={issue.id}>{issue.title} <span className="text-sm text-muted-foreground">({issue.state})</span></li>)}</ul> : <p className="mt-2 text-muted-foreground">{demo ? "No demo issues." : detail.activity?.issues?.phase === "current" ? "No issues found in the covered period." : "Issue import is incomplete; the issue count is unknown."}</p>}
            {detail.issues.hasNextPage && <p className="mt-2 text-sm text-muted-foreground">Partial issue list; more issues are available through the API.</p>}
          </>}
        </>}
      </section>;
    })}
  </>;
}

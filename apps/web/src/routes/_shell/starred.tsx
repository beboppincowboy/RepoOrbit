import { useInfiniteQuery, useQueryClient, type InfiniteData } from "@tanstack/react-query";
import { createFileRoute, useNavigate } from "@tanstack/react-router";
import { useState } from "react";
import { Button } from "@/components/ui/button";
import { matchesListSearch, validateListSearch } from "@/lib/list-search";
import { setFollowed, starredRepositories, type ListedRepo, type StarredPage } from "@/lib/api";

export const Route = createFileRoute("/_shell/starred")({
  validateSearch: validateListSearch,
  component: Starred,
});

type StarredParam = { page: number; refresh: boolean };

function Starred() {
  const { session } = Route.useRouteContext();
  const search = Route.useSearch();
  const navigate = useNavigate({ from: "/starred" });
  const queryClient = useQueryClient();
  const demo = session.mode === "demo";
  const [error, setError] = useState<string | null>(null);
  const [savingFollow, setSavingFollow] = useState(false);

  // The first page re-reads stars from GitHub (refresh); "Load more" pages through the stored snapshot.
  const query = useInfiniteQuery({
    queryKey: ["starred"],
    queryFn: ({ pageParam }: { pageParam: StarredParam }) => starredRepositories(pageParam.page, pageParam.refresh),
    initialPageParam: { page: 1, refresh: true } as StarredParam,
    getNextPageParam: (last: StarredPage) => (last.nextPage > 0 ? { page: last.nextPage, refresh: false } : undefined),
    enabled: !demo,
  });
  const pages = query.data?.pages ?? [];
  const rows = pages.flatMap(p => p.repositories);
  const listing = pages.at(-1);
  const withheld = pages.reduce((sum, p) => sum + p.withheld, 0);
  const visible = rows.filter(r => matchesListSearch(r, search.q));
  const controlsDisabled = savingFollow || query.isFetching;

  async function toggle(repo: ListedRepo) {
    setSavingFollow(true);
    try {
      await setFollowed(repo.id, !repo.selected);
      const selected = !repo.selected;
      queryClient.setQueryData<InfiniteData<StarredPage>>(["starred"], old => old && { ...old, pages: old.pages.map(p => ({ ...p, repositories: p.repositories.map(r => (r.id === repo.id ? { ...r, selected, syncState: selected ? "awaiting-sync" as const : "not-synchronized" as const } : r)) })) });
      setError(null);
    } catch (e) { setError(e instanceof Error ? e.message : "Something went wrong."); } finally { setSavingFollow(false); }
  }

  if (demo) return <div className="h-full overflow-y-auto p-6"><h1 className="text-2xl font-bold">Starred repositories</h1><p className="mt-4 text-muted-foreground">Demo mode has no GitHub connection, so there are no stars to follow.</p></div>;
  return <div className="flex h-full min-h-0 flex-col">
    <header className="shrink-0 border-b border-border px-6 py-4">
      <div className="flex items-end justify-between gap-4">
        <div><h1 className="text-2xl font-bold">Starred repositories</h1><p className="mt-1 text-sm text-muted-foreground">Follow the repositories you want to find issues in.</p></div>
        <Button onClick={() => queryClient.resetQueries({ queryKey: ["starred"] })} disabled={controlsDisabled}>Refresh</Button>
      </div>
      <input type="search" aria-label="Search starred repositories" placeholder="Search starred repositories" value={search.q ?? ""} onChange={e => navigate({ search: prev => ({ ...prev, q: e.target.value || undefined }), replace: true })} className="mt-3 w-64 rounded-md border border-border bg-card px-3 py-1.5 text-sm" />
    </header>
    <div className="relative min-h-0 flex-1 overflow-y-auto">
      {error && <p role="alert" className="m-4 text-destructive">{error}</p>}
      {query.isError && <p role="alert" className="m-4 text-destructive">Could not load your starred repositories. Check that the local services are running.</p>}
      {listing?.refreshFailed && <p role="alert" className="m-4 rounded border border-warning/40 bg-warning/10 p-3">GitHub could not be reached{listing.retryAt ? ` (rate limited; retry after ${new Date(listing.retryAt).toLocaleTimeString()})` : ""}. {listing.refreshedAt ? "This is your last saved list, so it may be out of date." : "There is no saved list yet."}</p>}
      {listing?.refreshedAt && <p className="m-4 text-sm text-muted-foreground">Star list saved {new Date(listing.refreshedAt).toLocaleString()}.</p>}
      {withheld > 0 && <p role="status" className="m-4 rounded border border-border bg-card p-3">{withheld} private {withheld === 1 ? "entry is" : "entries are"} hidden because access could not be verified. Private repositories need the GitHub App installed.{listing?.installUrl && <> <a className="underline" href={listing.installUrl}>Install the app</a>, then press Refresh.</>}</p>}
      {query.isPending && <p role="status" className="m-4">Loading starred repositories…</p>}
      {listing && !query.isFetching && rows.length === 0 && !withheld && <p className="m-4">{listing.refreshFailed && !listing.refreshedAt ? "Your starred repositories could not be loaded." : "You have not starred any repositories."}</p>}
      <ul>{visible.map(repo => <li key={repo.id} className="flex items-start justify-between gap-4 border-b border-border px-6 py-3">
        <div>
          <h2 className="font-medium">{repo.fullName}{repo.private && <span className="ml-2 rounded bg-muted px-1.5 py-0.5 text-xs font-medium">Private</span>}</h2>
          {repo.description && <p className="mt-0.5 text-sm text-muted-foreground">{repo.description}</p>}
          <p className="mt-1 text-sm text-muted-foreground">{repo.selected ? "Following, awaiting sync" : "Not following"}</p>
        </div>
        <Button onClick={() => toggle(repo)} disabled={controlsDisabled}>{repo.selected ? "Unfollow" : "Follow"}<span className="sr-only"> {repo.fullName}</span></Button>
      </li>)}</ul>
      {query.hasNextPage && <div className="p-6"><Button onClick={() => query.fetchNextPage()} disabled={controlsDisabled}>Load more repositories</Button></div>}
    </div>
  </div>;
}

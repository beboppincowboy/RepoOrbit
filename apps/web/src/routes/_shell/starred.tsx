import { useInfiniteQuery, useQueryClient, type InfiniteData } from "@tanstack/react-query";
import { createFileRoute, useNavigate } from "@tanstack/react-router";
import { useState } from "react";
import { Button } from "@/components/ui/button";
import { SyncStatus } from "@/components/sync-status";
import { RepositoryCell, RepositoriesHeading, SourceTabs, ToolsRow, noteClass, searchBoxClass, tableCardClass, tdClass, thClass, warnNoteClass } from "@/components/repository-list";
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
      void queryClient.invalidateQueries({ queryKey: ["selectedRepositories"] });
    } catch (e) { setError(e instanceof Error ? e.message : "Something went wrong."); } finally { setSavingFollow(false); }
  }

  return <div className="flex h-full min-h-0 flex-col gap-3">
    <RepositoriesHeading>Follow the starred repositories you want to find issues in. Following starts syncing at once.</RepositoriesHeading>
    <ToolsRow>
      <SourceTabs />
      {!demo && <input type="search" aria-label="Search starred repositories" placeholder="Filter by name or description" value={search.q ?? ""} onChange={e => navigate({ search: prev => ({ ...prev, q: e.target.value || undefined }), replace: true })} className={searchBoxClass} />}
      {!demo && <Button variant="outline" size="sm" className="ml-auto" onClick={() => queryClient.resetQueries({ queryKey: ["starred"] })} disabled={controlsDisabled}>Refresh</Button>}
    </ToolsRow>
    {demo && <p className={noteClass}>Demo mode has no GitHub connection, so there are no stars to follow.</p>}
    {error && <p role="alert" className={`${noteClass} text-destructive`}>{error}</p>}
    {query.isError && <p role="alert" className={`${noteClass} text-destructive`}>Could not load your starred repositories. Check that the local services are running.</p>}
    {listing?.refreshFailed && <p role="alert" className={warnNoteClass}>GitHub could not be reached{listing.retryAt ? ` (rate limited; retry after ${new Date(listing.retryAt).toLocaleTimeString()})` : ""}. {listing.refreshedAt ? "This is your last saved list, so it may be out of date." : "There is no saved list yet."}</p>}
    {withheld > 0 && <p role="status" className={noteClass}>{withheld} private {withheld === 1 ? "entry is" : "entries are"} hidden because access could not be verified. Private repositories need the GitHub App installed.{listing?.installUrl && <> <a className="text-primary underline" href={listing.installUrl}>Install the app</a>, then press Refresh.</>}</p>}
    {!demo && <div className={tableCardClass}>
      <table className="w-full border-collapse">
        <thead><tr><th className={thClass}>Repository</th><th className={thClass}>Sync status</th><th className={`${thClass} text-right`}><span className="sr-only">Follow</span></th></tr></thead>
        <tbody>
          {visible.map(repo => <tr key={repo.id} className={repo.selected ? "bg-primary-soft" : "hover:bg-muted"}>
            <td className={tdClass}>
              <RepositoryCell as="h2" fullName={repo.fullName} isPrivate={repo.private} description={repo.description} />
              <div className="text-xs text-muted-foreground">{repo.selected ? "Following, awaiting sync" : "Not following"}</div>
            </td>
            <td className={tdClass}><SyncStatus compact syncState={repo.syncState} /></td>
            <td className={`${tdClass} text-right`}><Button variant={repo.selected ? "outline" : "default"} size="sm" onClick={() => toggle(repo)} disabled={controlsDisabled}>{repo.selected ? "Unfollow" : "Follow"}<span className="sr-only"> {repo.fullName}</span></Button></td>
          </tr>)}
          {query.isPending && <tr><td colSpan={3} role="status" className={`${tdClass} py-6 text-center text-muted-foreground`}>Loading starred repositories…</td></tr>}
          {listing && !query.isFetching && rows.length === 0 && !withheld && <tr><td colSpan={3} className={`${tdClass} py-6 text-center text-muted-foreground`}>{listing.refreshFailed && !listing.refreshedAt ? "Your starred repositories could not be loaded." : "You have not starred any repositories."}</td></tr>}
          {rows.length > 0 && visible.length === 0 && <tr><td colSpan={3} className={`${tdClass} py-6 text-center text-muted-foreground`}>No repositories match this filter.</td></tr>}
        </tbody>
      </table>
      {query.hasNextPage && <div className="p-3"><Button variant="outline" onClick={() => query.fetchNextPage()} disabled={controlsDisabled}>Load more repositories</Button></div>}
      {listing?.refreshedAt && <p className="m-0 px-3 py-2.5 text-xs text-muted-foreground">Star list saved {new Date(listing.refreshedAt).toLocaleString()}.</p>}
    </div>}
  </div>;
}

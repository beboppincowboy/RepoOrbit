import { useInfiniteQuery, useQueryClient, type InfiniteData } from "@tanstack/react-query";
import { createFileRoute, useNavigate } from "@tanstack/react-router";
import { useState } from "react";
import { Button } from "@/components/ui/button";
import { SyncStatus } from "@/components/sync-status";
import { matchesListSearch, validateListSearch } from "@/lib/list-search";
import { ownedRepositories, setSelected, type ListedRepo, type OwnedPage } from "@/lib/api";

export const Route = createFileRoute("/_shell/repositories")({
  validateSearch: validateListSearch,
  component: Repositories,
});

// Keys are the ?install= values GitHub's post-install redirect gets from the backend (repositories.Service.Installed).
const installMessages: Record<string, string> = { ok: "The GitHub App was installed. Private repositories it can reach are listed below.", rejected: "That installation could not be verified for your account, so it was ignored.", failed: "Could not confirm the installation. Try refreshing in a moment." };

type Outcome = { ok: boolean; text: string };

// Checkbox selection (R2). Nothing changes on the server until Apply; each repository's result is reported separately.
function Repositories() {
  const { session } = Route.useRouteContext();
  const search = Route.useSearch();
  const navigate = useNavigate({ from: "/repositories" });
  const queryClient = useQueryClient();
  const demo = session.mode === "demo";
  const [installResult] = useState(() => (typeof window === "undefined" ? null : new URLSearchParams(window.location.search).get("install")));
  const [draft, setDraft] = useState<Record<string, boolean>>({});
  const [outcomes, setOutcomes] = useState<Record<string, Outcome>>({});
  const [applying, setApplying] = useState(false);

  const query = useInfiniteQuery({
    queryKey: ["owned"],
    queryFn: ({ pageParam }) => ownedRepositories(pageParam),
    initialPageParam: 1,
    getNextPageParam: last => (last.nextPage > 0 ? last.nextPage : undefined),
    enabled: !demo,
  });
  const pages: OwnedPage[] = query.data?.pages ?? [];
  const rows = pages.flatMap(p => p.repositories);
  const listing = pages.at(-1);
  const checked = (repo: ListedRepo) => (repo.id in draft ? draft[repo.id] : repo.selected);
  const toAdd = rows.filter(r => draft[r.id] === true && !r.selected);
  const toRemove = rows.filter(r => draft[r.id] === false && r.selected);
  const pending = toAdd.length + toRemove.length;
  const connectedCount = rows.filter(r => r.selected).length;
  const visible = rows.filter(r => matchesListSearch(r, search.q) && (search.view !== "selected" || checked(r) || outcomes[r.id]?.ok === false));
  const firstUse = connectedCount === 0 && toRemove.length === 0;

  function setSearch(patch: Partial<typeof search>) {
    navigate({ search: prev => ({ ...prev, ...patch }), replace: true });
  }

  // Applies each change in turn. A failure only affects its own repository, so the rest still go through.
  async function apply() {
    setApplying(true);
    setOutcomes({});
    const next: Record<string, Outcome> = {};
    const done = new Map<string, boolean>();
    for (const repo of [...toAdd, ...toRemove]) {
      const selected = draft[repo.id];
      try {
        await setSelected(repo.id, selected);
        done.set(repo.id, selected);
        next[repo.id] = { ok: true, text: selected ? "Connected. Awaiting sync." : "Disconnected. RepoOrbit no longer syncs it for you." };
      } catch (e) {
        next[repo.id] = { ok: false, text: e instanceof Error ? e.message : "Something went wrong." };
      }
    }
    // Show successes at once; the refetch below reconciles with GitHub.
    queryClient.setQueryData<InfiniteData<OwnedPage>>(["owned"], old => old && { ...old, pages: old.pages.map(p => ({ ...p, repositories: p.repositories.map(r => (done.has(r.id) ? { ...r, selected: done.get(r.id)!, syncState: done.get(r.id)! ? "awaiting-sync" as const : "not-synchronized" as const } : r)) })) });
    setDraft({});
    setOutcomes(next);
    setApplying(false);
    await queryClient.invalidateQueries({ queryKey: ["owned"] });
  }

  function discard() {
    setDraft({});
    setOutcomes({});
  }

  const setChecked = (repo: ListedRepo, value: boolean) => setDraft(d => ({ ...d, [repo.id]: value }));
  const outcomeCount = Object.keys(outcomes).length;
  const failedCount = Object.values(outcomes).filter(o => !o.ok).length;

  if (demo) return <div className="h-full overflow-y-auto p-6"><h1 className="text-2xl font-bold">Repositories</h1><p className="mt-4 text-muted-foreground">Demo mode has no GitHub connection, so there is nothing to connect. Sign in with GitHub credentials configured to choose your repositories.</p></div>;

  return <div className="flex h-full min-h-0 flex-col">
    <header className="shrink-0 border-b border-border px-6 py-4">
      <div className="flex items-end justify-between gap-4">
        <div><h1 className="text-2xl font-bold">{firstUse ? "Get started" : "Your repositories"}</h1><p className="mt-1 max-w-2xl text-sm text-muted-foreground">{firstUse ? "Choose the repositories RepoOrbit should track. Nothing is synchronized until you confirm." : "Repositories on your personal GitHub account. Checked repositories are synchronized."}</p></div>
        <Button onClick={() => query.refetch()} disabled={query.isFetching || applying}>Refresh</Button>
      </div>
      <div className="mt-3 flex flex-wrap items-center gap-2">
        <input type="search" aria-label="Search repositories" placeholder="Search repositories" value={search.q ?? ""} onChange={e => setSearch({ q: e.target.value || undefined })} className="w-64 rounded-md border border-border bg-card px-3 py-1.5 text-sm" />
        <div role="group" aria-label="Show" className="flex overflow-hidden rounded-md border border-border text-sm">
          {([["All", undefined], ["Connected", "selected"]] as const).map(([label, view]) => <button key={label} aria-pressed={search.view === view} onClick={() => setSearch({ view })} className={`px-3 py-1.5 ${search.view === view ? "bg-muted font-medium" : "text-muted-foreground"}`}>{label}</button>)}
        </div>
        <button className="text-sm underline disabled:opacity-50" disabled={!visible.length || applying} onClick={() => setDraft(d => ({ ...d, ...Object.fromEntries(visible.map(r => [r.id, true])) }))}>Select all shown</button>
      </div>
    </header>

    {pending > 0 && <div role="region" aria-label="Pending changes" className="shrink-0 border-b border-border bg-card px-6 py-3 text-sm">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <p><strong>{toAdd.length} to add</strong>, <strong>{toRemove.length} to remove</strong>. Nothing changes until you apply.</p>
        <div className="flex gap-2">
          <Button onClick={discard} disabled={applying} className="bg-muted text-foreground">Discard</Button>
          <Button onClick={apply} disabled={applying}>{firstUse ? `Start syncing ${toAdd.length} ${toAdd.length === 1 ? "repository" : "repositories"}` : "Apply"}</Button>
        </div>
      </div>
      {toRemove.length > 0 && <p className="mt-2 text-muted-foreground">Removing {toRemove.map(r => r.fullName).join(", ")} stops RepoOrbit syncing {toRemove.length === 1 ? "it" : "them"} for you. Other people who selected {toRemove.length === 1 ? "it" : "them"} keep syncing. Saved issues remain available while you have access. Nothing changes on GitHub.</p>}
    </div>}

    <div className="relative min-h-0 flex-1 overflow-y-auto">
      {installResult && Object.hasOwn(installMessages, installResult) && <p role="status" className="m-4 rounded border border-border bg-card p-3">{installMessages[installResult]}</p>}
      {outcomeCount > 0 && <p role="status" className="m-4 text-sm">{outcomeCount - failedCount} applied{failedCount ? `, ${failedCount} failed` : ""}.</p>}
      {query.isError && <p role="alert" className="m-4 text-destructive">Could not load your repositories. Check that the local services are running.</p>}
      {listing?.refreshFailed && <p role="alert" className="m-4 rounded border border-warning/40 bg-warning/10 p-3">GitHub could not be reached{listing.retryAt ? ` (rate limited; retry after ${new Date(listing.retryAt).toLocaleTimeString()})` : ""}, so this shows only repositories you already connected. It is not your full list.{listing.withheld > 0 && ` ${listing.withheld} connected private ${listing.withheld === 1 ? "repository is" : "repositories are"} hidden until access can be verified.`}</p>}
      {query.isPending && <p role="status" className="m-4">Loading repositories…</p>}
      {listing && !query.isFetching && rows.length === 0 && !listing.withheld && <p className="m-4">{listing.refreshFailed ? "No connected repositories to show." : "No repositories found on your personal account."}</p>}
      {rows.length > 0 && visible.length === 0 && <p className="m-4 text-muted-foreground">No repositories match this filter.</p>}
      <ul>{visible.map(repo => <li key={repo.id} className="flex items-start gap-3 border-b border-border px-6 py-3">
        <input type="checkbox" aria-label={repo.fullName} className="mt-1 size-4" checked={checked(repo)} disabled={applying} onChange={e => setChecked(repo, e.target.checked)} />
        <div className="min-w-0 flex-1">
          <h2 className="font-medium">{repo.fullName}{repo.private && <span className="ml-2 rounded bg-muted px-1.5 py-0.5 text-xs font-medium">Private</span>}</h2>
          {repo.description && <p className="mt-0.5 text-sm text-muted-foreground">{repo.description}</p>}
          {outcomes[repo.id] && <p role={outcomes[repo.id].ok ? "status" : "alert"} className={`mt-1 text-sm ${outcomes[repo.id].ok ? "text-muted-foreground" : "text-destructive"}`}>{outcomes[repo.id].text}</p>}
        </div>
        <SyncStatus syncState={repo.syncState} />
      </li>)}</ul>
      {query.hasNextPage && <div className="p-6"><Button onClick={() => query.fetchNextPage()} disabled={query.isFetchingNextPage}>Load more repositories</Button></div>}
      <p className="p-6 text-sm text-muted-foreground">Some private repositories may need the GitHub App installed before they appear here.{listing?.installUrl ? <> <a className="underline" href={listing.installUrl}>Install the app</a>, then press Refresh.</> : " Install it on GitHub, then press Refresh."}</p>
    </div>
  </div>;
}

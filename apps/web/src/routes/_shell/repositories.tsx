import { useInfiniteQuery, useQueryClient, type InfiniteData } from "@tanstack/react-query";
import { createFileRoute, useNavigate } from "@tanstack/react-router";
import { useState } from "react";
import { Button } from "@/shared/ui/button";
import { SyncStatus } from "@/components/sync-status";
import { RepositoryCell, RepositoriesHeading, SourceTabs, ToolsRow, noteClass, searchBoxClass, segGroupClass, segItemClass, tableCardClass, tdClass, thClass, warnNoteClass } from "@/components/repository-list";
import { matchesListSearch, validateListSearch } from "@/lib/list-search";
import { flattenSelectedRepositoryRows, recordRepositoriesRemoved, useSelectedRepositories } from "@/lib/selected-repositories";
import { ownedRepositories, setSelected, type ListedRepo, type OwnedPage } from "@/lib/api";

export const Route = createFileRoute("/_shell/repositories")({
  validateSearch: validateListSearch,
  component: Repositories,
});

const installMessages: Record<string, string> = { ok: "The GitHub App was installed. Private repositories it can reach are listed below.", rejected: "That installation could not be verified for your account, so it was ignored.", failed: "Could not confirm the installation. Try refreshing in a moment." };

type Outcome = { ok: boolean; text: string };
const plural = (n: number) => (n === 1 ? "repository" : "repositories");

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
  const selection = useSelectedRepositories(session);
  const demoRows: ListedRepo[] = flattenSelectedRepositoryRows(selection)
    .filter(row => row.detail?.state === "available")
    .map(({ repo }) => ({ id: repo.id, fullName: repo.fullName, description: repo.description, private: false, selected: true, syncState: repo.syncState }));
  const pages: OwnedPage[] = query.data?.pages ?? [];
  const rows = demo ? demoRows : pages.flatMap(p => p.repositories);
  const pager = demo ? selection : query;
  const listing = pages.at(-1);
  const checked = (repo: ListedRepo) => (repo.id in draft ? draft[repo.id] : repo.selected);
  const toAdd = rows.filter(r => draft[r.id] === true && !r.selected);
  const toRemove = rows.filter(r => draft[r.id] === false && r.selected);
  const pending = toAdd.length + toRemove.length;
  const connectedCount = rows.filter(r => r.selected).length;
  const visible = rows.filter(r => matchesListSearch(r, search.q) && (search.view !== "selected" || checked(r) || outcomes[r.id]?.ok === false));
  const firstUse = !demo && connectedCount === 0 && toRemove.length === 0;
  const allShown = visible.length > 0 && visible.every(checked);

  function setSearch(patch: Partial<typeof search>) {
    navigate({ search: prev => ({ ...prev, ...patch }), replace: true });
  }

  async function apply() {
    setApplying(true);
    setOutcomes({});
    const next: Record<string, Outcome> = {};
    const done = new Map<string, boolean>();
    const removedIds: string[] = [];
    for (const repo of [...toAdd, ...toRemove]) {
      const selected = draft[repo.id];
      try {
        await setSelected(repo.id, selected);
        done.set(repo.id, selected);
        if (!selected) removedIds.push(repo.id);
        next[repo.id] = { ok: true, text: selected ? "Connected. Awaiting sync." : "Disconnected. RepoOrbit no longer syncs it for you." };
      } catch (e) {
        next[repo.id] = { ok: false, text: e instanceof Error ? e.message : "Something went wrong." };
      }
    }
    if (removedIds.length > 0) recordRepositoriesRemoved(queryClient, session, removedIds);
    queryClient.setQueryData<InfiniteData<OwnedPage>>(["owned"], old => old && { ...old, pages: old.pages.map(p => ({ ...p, repositories: p.repositories.map(r => (done.has(r.id) ? { ...r, selected: done.get(r.id)!, syncState: done.get(r.id)! ? "awaiting-sync" as const : "not-synchronized" as const } : r)) })) });
    setDraft({});
    setOutcomes(next);
    setApplying(false);
    await Promise.all([queryClient.invalidateQueries({ queryKey: ["owned"] }), queryClient.invalidateQueries({ queryKey: ["selectedRepositories"] })]);
  }

  function discard() {
    setDraft({});
    setOutcomes({});
  }

  const setChecked = (repo: ListedRepo, value: boolean) => setDraft(d => ({ ...d, [repo.id]: value }));
  const outcomeCount = Object.keys(outcomes).length;
  const failedCount = Object.values(outcomes).filter(o => !o.ok).length;

  return <div className="flex h-full min-h-0 flex-col gap-3">
    {firstUse ? <div className="flex shrink-0 flex-wrap items-center justify-between gap-2.5 rounded-xl border border-border bg-card p-4">
      <div><h1 className="text-lg">Choose what RepoOrbit keeps in sync</h1><p className="mt-0.5 text-muted-foreground">Check repositories you own, or follow starred ones you work on, then start syncing. You can change this later under Repositories.</p></div>
      <ol className="m-0 flex list-none flex-wrap gap-2 p-0 text-xs">
        {(["Login", "Choose", "Sync progress", "Workspace"] as const).map((step, i) => <li key={step} aria-current={i === 1 ? "step" : undefined} className={`flex items-center gap-1.5 ${i === 1 ? "font-semibold text-foreground" : "text-muted-foreground"}`}>
          <b className={`grid size-5 place-items-center rounded-full font-mono text-[11px] font-semibold ${i === 0 ? "bg-success text-white" : i === 1 ? "bg-primary text-primary-foreground" : "bg-muted"}`}>{i === 0 ? "✓" : i + 1}</b>{step}
        </li>)}
      </ol>
    </div> : <RepositoriesHeading>{demo ? "Demo repositories are fixed sample data; the selection cannot change." : "Check or uncheck what RepoOrbit keeps in sync, then Apply."}</RepositoriesHeading>}

    <ToolsRow>
      <SourceTabs />
      <input type="search" aria-label="Search repositories" placeholder="Filter by name or description" value={search.q ?? ""} onChange={e => setSearch({ q: e.target.value || undefined })} className={searchBoxClass} />
      <div role="group" aria-label="Show" className={segGroupClass}>
        {([["All", undefined], ["Connected", "selected"]] as const).map(([label, view]) => <button key={label} type="button" aria-pressed={search.view === view} onClick={() => setSearch({ view })} className={segItemClass}>{label}</button>)}
      </div>
      {!demo && <Button variant="outline" size="sm" className="ml-auto" onClick={() => void Promise.all([query.refetch(), queryClient.invalidateQueries({ queryKey: ["selectedRepositories"] })])} disabled={query.isFetching || applying}>Refresh</Button>}
    </ToolsRow>

    {demo && <p className={noteClass}>Demo mode has no GitHub connection, so there is nothing to connect. Sign in with GitHub credentials configured to choose your repositories.</p>}
    {installResult && Object.hasOwn(installMessages, installResult) && <p role="status" className={noteClass}>{installMessages[installResult]}</p>}
    {outcomeCount > 0 && <p role="status" className={noteClass}>{outcomeCount - failedCount} applied{failedCount ? `, ${failedCount} failed` : ""}.</p>}
    {query.isError && <p role="alert" className={`${noteClass} text-destructive`}>Could not load your repositories. Check that the local services are running.</p>}
    {listing?.refreshFailed && <p role="alert" className={warnNoteClass}>GitHub could not be reached{listing.retryAt ? ` (rate limited; retry after ${new Date(listing.retryAt).toLocaleTimeString()})` : ""}, so this shows only repositories you already connected. It is not your full list.{listing.withheld > 0 && ` ${listing.withheld} connected private ${listing.withheld === 1 ? "repository is" : "repositories are"} hidden until access can be verified.`}</p>}

    <div className={tableCardClass}>
      <table className="w-full border-collapse">
        <thead><tr>
          <th className={`${thClass} w-9`}><input type="checkbox" aria-label="Select all shown" className="accent-primary" checked={allShown} disabled={demo || !visible.length || applying} onChange={e => setDraft(d => ({ ...d, ...Object.fromEntries(visible.map(r => [r.id, e.target.checked])) }))} /></th>
          <th className={thClass}>Repository</th>
          <th className={thClass}>Sync status</th>
        </tr></thead>
        <tbody>
          {visible.map(repo => {
            const on = checked(repo);
            const outcome = outcomes[repo.id];
            const changed = repo.id in draft && draft[repo.id] !== repo.selected;
            return <tr key={repo.id} className={on ? "bg-primary-soft" : "hover:bg-muted"}>
              <td className={tdClass}><input type="checkbox" aria-label={repo.fullName} className="accent-primary" checked={on} disabled={demo || applying} onChange={e => setChecked(repo, e.target.checked)} /></td>
              <td className={tdClass}>
                <RepositoryCell fullName={repo.fullName} isPrivate={repo.private} description={repo.description} />
                {outcome?.ok && <p role="status" className="text-xs text-muted-foreground">{outcome.text}</p>}
              </td>
              <td className={tdClass}>{outcome && !outcome.ok
                ? <span role="alert" className="inline-flex items-center gap-1.5 rounded-full bg-destructive-soft px-[9px] py-0.5 text-xs text-destructive"><span aria-hidden="true" className="size-[7px] shrink-0 rounded-full bg-current" />{outcome.text}</span>
                : changed ? <span className={`inline-flex rounded-full border px-[7px] text-[11px] ${on ? "border-border text-soft" : "border-primary-soft bg-primary-soft text-primary"}`}>{on ? "Will start syncing" : "Will stop syncing"}</span>
                : <SyncStatus compact syncState={repo.syncState} />}</td>
            </tr>;
          })}
          {query.isPending && !demo && <tr><td colSpan={3} role="status" className={`${tdClass} py-6 text-center text-muted-foreground`}>Loading repositories…</td></tr>}
          {(demo || (listing && !query.isFetching)) && rows.length === 0 && !listing?.withheld && <tr><td colSpan={3} className={`${tdClass} py-6 text-center text-muted-foreground`}>{demo ? "No demo repositories yet." : listing?.refreshFailed ? "No connected repositories to show." : "No repositories found on your personal account."}</td></tr>}
          {rows.length > 0 && visible.length === 0 && <tr><td colSpan={3} className={`${tdClass} py-6 text-center text-muted-foreground`}>No repositories match this filter.</td></tr>}
        </tbody>
      </table>
      {pager.hasNextPage && <div className="p-3"><Button variant="outline" onClick={() => pager.fetchNextPage()} disabled={pager.isFetchingNextPage}>Load more repositories</Button></div>}
      {!demo && <p className="m-0 px-3 py-2.5 text-xs text-muted-foreground">Some private repositories may need the GitHub App installed before they appear here.{listing?.installUrl ? <> <a className="text-primary underline" href={listing.installUrl}>Install the app</a>, then press Refresh.</> : " Install it on GitHub, then press Refresh."}</p>}
    </div>

    {(firstUse || pending > 0) && <div role="region" aria-label="Pending changes" className="flex shrink-0 flex-wrap items-center gap-x-3 gap-y-2 rounded-xl border border-border bg-card p-4">
      {firstUse ? <>
        <span className="font-medium">{toAdd.length ? `${toAdd.length} selected` : "Pick at least one repository"}</span>
        <span className="text-xs text-muted-foreground">Nothing syncs until you start.</span>
        <span className="ml-auto flex gap-2">
          {toAdd.length > 0 && <Button variant="outline" onClick={discard} disabled={applying}>Discard</Button>}
          <Button onClick={apply} disabled={applying || !toAdd.length}>{`Start syncing ${toAdd.length ? `${toAdd.length} ` : ""}${plural(toAdd.length)}`}</Button>
        </span>
      </> : <>
        <span className="font-medium">{toAdd.length} to add · {toRemove.length} to remove</span>
        {toRemove.length > 0 && <span className="flex-[1_1_320px] text-xs text-muted-foreground">Removing {toRemove.map(r => r.fullName).join(", ")} stops RepoOrbit syncing {toRemove.length === 1 ? "it" : "them"} for you. Other people who selected {toRemove.length === 1 ? "it" : "them"} keep syncing. Saved issues remain available while you have access. Nothing changes on GitHub.</span>}
        <span className="ml-auto flex gap-2">
          <Button variant="outline" onClick={discard} disabled={applying}>Discard</Button>
          <Button onClick={apply} disabled={applying}>Apply</Button>
        </span>
      </>}
    </div>}
  </div>;
}

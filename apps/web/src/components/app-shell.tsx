import { useState, type ReactNode } from "react";
import { Link, useNavigate, useRouterState } from "@tanstack/react-router";
import { useMutation, useQueryClient, type InfiniteData } from "@tanstack/react-query";
import { fetchRepositoryDetails, fetchSelectedRepositories, logout, type Me } from "@/lib/api";
import { selectedRepositoriesKey, selectedRows, useSelectedRepositories, type SelectedBatch, type SelectedRow } from "@/lib/selected-repositories";
import { SyncDot, syncDisplay, syncLabels } from "@/components/sync-status";

// RepoOrbit mark: a planet with an orbit ring (prototype D2 logo).
export function LogoMark() {
  return <i className="grid size-[26px] place-items-center rounded-[7px] bg-primary">
    <svg width="16" height="16" viewBox="0 0 26 26" aria-hidden="true"><circle cx="13" cy="13" r="4" fill="#fff" /><ellipse cx="13" cy="13" rx="10" ry="4.5" fill="none" stroke="#fff" strokeWidth="2" transform="rotate(-25 13 13)" /></svg>
  </i>;
}

const icon = (d: ReactNode) => <svg width="16" height="16" viewBox="0 0 16 16" aria-hidden="true" className="shrink-0">{d}</svg>;
const gridIcon = icon(<path d="M2 2h5v5H2zM9 2h5v5H9zM2 9h5v5H2zM9 9h5v5H9z" fill="none" stroke="currentColor" strokeWidth="1.4" />);
const repoIcon = icon(<path d="M3 2.5h8.5v10H4.5A1.5 1.5 0 0 1 3 11zM3 11a1.5 1.5 0 0 1 1.5-1.5h7" fill="none" stroke="currentColor" strokeWidth="1.4" />);

const navbtn = "flex w-full items-center gap-2.5 whitespace-nowrap rounded-lg px-2 py-[7px] text-left text-soft hover:bg-muted aria-[current=page]:bg-primary-soft aria-[current=page]:font-semibold aria-[current=page]:text-primary max-[900px]:w-auto";
const navlabel = "px-2 pt-3 pb-1 font-mono text-[11px] font-medium uppercase tracking-[.06em] text-muted-foreground max-[900px]:hidden";
const card = "rounded-xl border border-border bg-card";

// A row's display state, mirroring the overview: access-unverified rows are withheld (no name, no link).
function rowDisplay({ repo, detail }: SelectedRow) {
  if (detail?.state !== "available") return syncDisplay({ syncState: repo.syncState, withheld: 1, retryAt: detail?.retryAt });
  return syncDisplay({ syncState: repo.syncState, activity: detail?.activity ?? null, metadataStatus: repo.syncState === "awaiting-sync" ? repo.metadataStatus : null, retryAt: detail?.issues.retryAt ?? null });
}

// Prototype D2 shell: sidebar (Overview, Repositories, then each selected repository) | top bar (breadcrumb, repository
// jump, avatar menu) | view. The viewport is fixed; each screen scrolls its own lists.
export function AppShell({ session, children }: { session: Me; children: ReactNode }) {
  const demo = session.mode === "demo";
  const pathname = useRouterState({ select: state => state.location.pathname });
  const navigate = useNavigate();
  const selection = useSelectedRepositories(session);
  const queryClient = useQueryClient();
  const retryDetails = useMutation({
    mutationFn: async (batch: SelectedBatch) => {
      const details = await fetchRepositoryDetails(batch.repositories.map(repo => repo.id));
      queryClient.setQueryData<InfiniteData<Awaited<ReturnType<typeof fetchSelectedRepositories>>>>(
        selectedRepositoriesKey(session),
        previous => previous && {
          ...previous,
          pages: previous.pages.map(page => ({
            ...page,
            batches: page.batches.map(current => current === batch ? { ...batch, details } : current),
          })),
        },
      );
    },
  });
  const failedBatches = selection.isRefetchError ? [] : selection.data?.pages.flatMap(page => page.batches.filter(batch => batch.details === null)) ?? [];
  const rows = selectedRows(selection);
  const visible = rows.filter(row => row.detail?.state === "available");
  const workspaceId = pathname.match(/^\/workspace\/([^/]+)/)?.[1];
  const current = workspaceId ? visible.find(row => row.repo.id === decodeURIComponent(workspaceId)) : undefined;
  const reposActive = pathname === "/repositories" || pathname === "/starred";
  const screen = pathname === "/" ? "Engineering overview" : reposActive ? "Repositories" : "Repository workspace";
  const [owner, name] = current?.repo.fullName.split("/") ?? [];

  function jump(value: string) {
    const row = visible.find(r => r.repo.fullName === value);
    if (row) navigate({ to: "/workspace/$repositoryId", params: { repositoryId: row.repo.id }, search: { days: 30, tab: "overview" } });
  }

  return <div data-app-shell className="mx-auto grid h-full max-w-[1600px] grid-cols-[216px_minmax(0,1fr)] gap-3 max-[900px]:grid-cols-1 max-[900px]:grid-rows-[auto_minmax(0,1fr)]">
    <aside aria-label="Main" className={`${card} flex min-h-0 flex-col gap-0.5 overflow-auto px-3 py-4 max-[900px]:flex-row max-[900px]:items-center max-[900px]:overflow-x-auto max-[900px]:p-2`}>
      <div className="flex items-center gap-2 px-2 pb-3 text-[15px] font-semibold max-[900px]:hidden"><LogoMark />RepoOrbit</div>
      <nav aria-label="Primary" className="contents">
        <div className={navlabel}>Workspace</div>
        <Link to="/" activeOptions={{ exact: true }} className={navbtn}>{gridIcon}Overview</Link>
        <Link to="/repositories" aria-current={reposActive ? "page" : undefined} className={navbtn}>{repoIcon}Repositories{rows.length > 0 && <span aria-hidden="true" className="ml-auto font-mono text-xs font-normal text-muted-foreground">{rows.length}</span>}</Link>
        {rows.length > 0 && <div className={navlabel}>Repositories</div>}
        {rows.map(row => {
          const display = rowDisplay(row);
          if (row.detail?.state !== "available") return <span key={row.repo.id} title={syncLabels[display]} className={`${navbtn} text-[12.5px] text-muted-foreground hover:bg-transparent max-[900px]:hidden`}><SyncDot display={display} />Repository unavailable</span>;
          return <Link key={row.repo.id} to="/workspace/$repositoryId" params={{ repositoryId: row.repo.id }} search={{ days: 30, tab: "overview" }} activeOptions={{ includeSearch: false }} title={`${row.repo.fullName} · ${syncLabels[display]}`} className={`${navbtn} text-[12.5px] max-[900px]:hidden`}>
            <SyncDot display={display} /><span className="truncate">{row.repo.fullName.split("/")[1]}</span>
          </Link>;
        })}
        {selection.hasNextPage && <button type="button" className={navbtn} disabled={selection.isFetching} onClick={() => void selection.fetchNextPage()}>Load more selected repositories</button>}
        {failedBatches.map(batch => <button key={batch.repositories[0].id} type="button" className={navbtn} disabled={selection.isFetching || retryDetails.isPending} onClick={() => retryDetails.mutate(batch)}>Retry details</button>)}
        {retryDetails.isError && failedBatches.length > 0 && <span role="alert" className="px-2 text-xs text-destructive">Could not verify repository details.</span>}
      </nav>
      <div className="mt-auto p-2 max-[900px]:hidden"><span className="font-mono text-xs text-muted-foreground">RepoOrbit · command center</span></div>
    </aside>
    <div className="flex min-h-0 min-w-0 flex-col gap-3">
      <header className={`${card} flex shrink-0 flex-wrap items-center gap-x-3 gap-y-2 px-3 py-2`}>
        <div className="flex min-w-0 items-center gap-1.5 text-muted-foreground">{current ? <><span>{owner} /</span> <b className="font-semibold text-foreground">{name}</b></> : <b className="font-semibold text-foreground">{screen}</b>}</div>
        <div className="ml-auto flex items-center gap-2">
          <label className="flex min-w-[200px] items-center gap-2 rounded-lg border border-border bg-background px-2.5 py-[5px] text-muted-foreground max-[900px]:hidden">
            <svg width="14" height="14" viewBox="0 0 16 16" aria-hidden="true"><circle cx="7" cy="7" r="4.5" fill="none" stroke="currentColor" strokeWidth="1.5" /><path d="M10.5 10.5L14 14" stroke="currentColor" strokeWidth="1.5" /></svg>
            <input placeholder="Jump to repository…" aria-label="Jump to repository" list="repolist" className="w-full border-0 bg-transparent text-foreground outline-none" onChange={e => { jump(e.target.value); if (visible.some(r => r.repo.fullName === e.target.value)) e.target.value = ""; }} />
            <datalist id="repolist">{visible.map(row => <option key={row.repo.id} value={row.repo.fullName} />)}</datalist>
          </label>
          <AccountMenu session={session} />
        </div>
      </header>
      {demo && <div className="shrink-0 rounded-[10px] border border-demo bg-demo-soft px-3 py-1.5 text-[13px] font-medium text-demo"><span>Demo mode</span> · fictional sample data under a demo identity. GitHub writes are disabled.</div>}
      <main className="min-h-0 flex-1 overflow-hidden">{children}</main>
    </div>
  </div>;
}

// Avatar with the signed-in identity and Sign out (real mode only).
function AccountMenu({ session }: { session: Me }) {
  const demo = session.mode === "demo";
  const login = session.user?.login ?? "";
  const [open, setOpen] = useState(false);
  const [logoutError, setLogoutError] = useState(false);
  async function signOut() {
    setLogoutError(false);
    try {
      await logout();
      window.location.assign("/login");
    } catch {
      setLogoutError(true);
    }
  }
  return <nav aria-label="Account" className="relative" onKeyDown={e => { if (e.key === "Escape") setOpen(false); }} onBlur={e => { if (!e.currentTarget.contains(e.relatedTarget)) setOpen(false); }}>
    <button type="button" aria-label="Account menu" aria-expanded={open} onClick={() => setOpen(o => !o)} className="grid size-7 place-items-center rounded-full bg-primary-soft text-[10px] font-semibold text-primary">{demo ? "DM" : login.slice(0, 2).toUpperCase()}</button>
    {(open || logoutError) && <div className="absolute right-0 top-9 z-50 flex min-w-48 flex-col gap-2 rounded-xl border border-border bg-card p-3 shadow-[0_18px_44px_rgba(15,23,42,.16)]">
      {session.user && <span className="text-muted-foreground">{demo ? "Demo identity" : "Signed in as"} <strong className="text-foreground">{login}</strong></span>}
      {logoutError && <span role="alert" className="text-destructive">Could not sign out. Try again.</span>}
      {!demo && session.user && <button type="button" className="rounded-lg border border-border px-3 py-1.5 text-left hover:bg-muted" onClick={signOut}>Sign out</button>}
    </div>}
  </nav>;
}

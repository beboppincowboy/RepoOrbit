"use client";
import { useCallback, useEffect, useState } from "react";
import { Button } from "@/components/ui/button";
import { ownedRepositories, setSelected, type OwnedPage, type ListedRepo } from "@/lib/api";
import { useSession } from "@/components/auth-shell";

// Keys are the ?install= values GitHub's post-install redirect gets from the backend (repositories.Service.Installed).
const installMessages: Record<string, string> = { ok: "The GitHub App was installed. Private repositories it can reach are listed below.", rejected: "That installation could not be verified for your account, so it was ignored.", failed: "Could not confirm the installation. Try refreshing in a moment." };

export default function Repositories() {
  const demo = useSession()?.mode === "demo";
  const [repos, setRepos] = useState<ListedRepo[]>([]);
  const [listing, setListing] = useState<OwnedPage | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);
  const [busy, setBusy] = useState<string | null>(null);
  const [installResult] = useState(() => typeof window === "undefined" ? null : new URLSearchParams(window.location.search).get("install"));

  const load = useCallback(async (next = 1) => {
    setLoading(true);
    try {
      const result = await ownedRepositories(next);
      setListing(result);
      setRepos(prev => next === 1 ? result.repositories : [...prev, ...result.repositories]);
      setError(null);
    } catch { setError("Could not load your repositories. Check that the local services are running."); } finally { setLoading(false); }
  }, []);
  useEffect(() => { if (!demo) load(1); else setLoading(false); }, [demo, load]);

  async function toggle(repo: ListedRepo) {
    setBusy(repo.id);
    try {
      await setSelected(repo.id, !repo.selected);
      setRepos(prev => prev.map(r => r.id === repo.id ? { ...r, selected: !repo.selected, syncState: repo.selected ? "not-synchronized" as const : "awaiting-sync" as const } : r));
      setError(null);
    } catch (e) { setError(e instanceof Error ? e.message : "Something went wrong."); } finally { setBusy(null); }
  }

  if (demo) return <main className="mx-auto max-w-3xl px-5 py-12"><h1 className="text-3xl font-bold">Repositories</h1><p className="mt-4 text-slate-600">Demo mode has no GitHub connection, so there is nothing to connect. Sign in with GitHub credentials configured to choose your repositories.</p></main>;
  return <main className="mx-auto max-w-3xl px-5 py-12">
    <header className="mb-6 flex items-end justify-between gap-4"><div><h1 className="text-3xl font-bold">Your repositories</h1><p className="mt-2 text-slate-600">Repositories on your personal GitHub account. Connect the ones you want RepoOrbit to track.</p></div><Button onClick={() => load(1)} disabled={loading}>Refresh</Button></header>
    {installResult && installMessages[installResult] && <p role="status" className="mb-4 rounded border border-slate-300 bg-white p-3">{installMessages[installResult]}</p>}
    {error && <p role="alert" className="mb-4 text-red-700">{error}</p>}
    {listing?.refreshFailed && <p role="alert" className="mb-4 rounded border border-amber-300 bg-amber-50 p-3 text-amber-900">GitHub could not be reached{listing.retryAt ? ` (rate limited; retry after ${new Date(listing.retryAt).toLocaleTimeString()})` : ""}, so this shows only repositories you already connected. It is not your full list.{listing.withheld > 0 && ` ${listing.withheld} connected private ${listing.withheld === 1 ? "repository is" : "repositories are"} hidden until access can be verified.`}</p>}
    {loading && <p role="status">Loading repositories…</p>}
    {listing && !loading && repos.length === 0 && !listing.withheld && <p>{listing.refreshFailed ? "No connected repositories to show." : "No repositories found on your personal account."}</p>}
    <ul className="space-y-3">{repos.map(repo => <li key={repo.id} className="flex items-start justify-between gap-4 rounded-lg border border-slate-200 bg-white p-4 shadow-sm">
      <div><h2 className="font-semibold">{repo.fullName} {repo.private && <span className="ml-1 rounded bg-slate-100 px-1.5 py-0.5 text-xs font-medium">Private</span>}</h2>{repo.description && <p className="mt-1 text-sm text-slate-600">{repo.description}</p>}<p className="mt-1 text-sm text-slate-500">{repo.selected ? "Selected, awaiting sync" : "Not synchronized"}</p></div>
      <Button onClick={() => toggle(repo)} disabled={busy === repo.id}>{repo.selected ? "Disconnect" : "Connect"}<span className="sr-only"> {repo.fullName}</span></Button>
    </li>)}</ul>
    {listing && listing.nextPage > 0 && <Button className="mt-6" onClick={() => load(listing.nextPage)} disabled={loading}>Load more repositories</Button>}
    <p className="mt-8 text-sm text-slate-600">Some private repositories may need the GitHub App installed before they appear here.{listing?.installUrl ? <> <a className="underline" href={listing.installUrl}>Install the app</a>, then press Refresh.</> : " Install it on GitHub, then press Refresh."}</p>
  </main>;
}

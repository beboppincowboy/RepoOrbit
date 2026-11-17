"use client";
import { useCallback, useEffect, useState } from "react";
import { Button } from "@/components/ui/button";
import { setFollowed, starredRepositories, type ListedRepo, type StarredPage } from "@/lib/api";
import { useSession } from "@/components/auth-shell";

export default function Starred() {
  const demo = useSession()?.mode === "demo";
  const [repos, setRepos] = useState<ListedRepo[]>([]);
  const [listing, setListing] = useState<StarredPage | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);
  const [busy, setBusy] = useState<string | null>(null);

  // Opening the page and pressing Refresh re-read GitHub; "Load more" pages through the stored snapshot.
  const load = useCallback(async (next = 1, refresh = false) => {
    setLoading(true);
    try {
      const result = await starredRepositories(next, refresh);
      setListing(result);
      setRepos(prev => next === 1 ? result.repositories : [...prev, ...result.repositories]);
      setError(null);
    } catch { setError("Could not load your starred repositories. Check that the local services are running."); } finally { setLoading(false); }
  }, []);
  useEffect(() => { if (!demo) load(1, true); else setLoading(false); }, [demo, load]);

  async function toggle(repo: ListedRepo) {
    setBusy(repo.id);
    try {
      await setFollowed(repo.id, !repo.selected);
      setRepos(prev => prev.map(r => r.id === repo.id ? { ...r, selected: !repo.selected, syncState: repo.selected ? "not-synchronized" as const : "awaiting-sync" as const } : r));
      setError(null);
    } catch (e) { setError(e instanceof Error ? e.message : "Something went wrong."); } finally { setBusy(null); }
  }

  if (demo) return <main className="mx-auto max-w-3xl px-5 py-12"><h1 className="text-3xl font-bold">Starred repositories</h1><p className="mt-4 text-slate-600">Demo mode has no GitHub connection, so there are no stars to follow.</p></main>;
  return <main className="mx-auto max-w-3xl px-5 py-12">
    <header className="mb-6 flex items-end justify-between gap-4"><div><h1 className="text-3xl font-bold">Starred repositories</h1><p className="mt-2 text-slate-600">Follow the repositories you want to find issues in.</p></div><Button onClick={() => load(1, true)} disabled={loading}>Refresh</Button></header>
    {error && <p role="alert" className="mb-4 text-red-700">{error}</p>}
    {listing?.refreshFailed && <p role="alert" className="mb-4 rounded border border-amber-300 bg-amber-50 p-3 text-amber-900">GitHub could not be reached{listing.retryAt ? ` (rate limited; retry after ${new Date(listing.retryAt).toLocaleTimeString()})` : ""}. {listing.refreshedAt ? "This is your last saved list, so it may be out of date." : "There is no saved list yet."}</p>}
    {listing?.refreshedAt && <p className="mb-4 text-sm text-slate-500">Star list saved {new Date(listing.refreshedAt).toLocaleString()}.</p>}
    {listing && listing.withheld > 0 && <p role="status" className="mb-4 rounded border border-slate-300 bg-white p-3">{listing.withheld} private {listing.withheld === 1 ? "entry is" : "entries are"} hidden because access could not be verified. Private repositories need the GitHub App installed.{listing.installUrl && <> <a className="underline" href={listing.installUrl}>Install the app</a>, then press Refresh.</>}</p>}
    {loading && <p role="status">Loading starred repositories…</p>}
    {listing && !loading && repos.length === 0 && !listing.withheld && <p>{listing.refreshFailed && !listing.refreshedAt ? "Your starred repositories could not be loaded." : "You have not starred any repositories."}</p>}
    <ul className="space-y-3">{repos.map(repo => <li key={repo.id} className="flex items-start justify-between gap-4 rounded-lg border border-slate-200 bg-white p-4 shadow-sm">
      <div><h2 className="font-semibold">{repo.fullName} {repo.private && <span className="ml-1 rounded bg-slate-100 px-1.5 py-0.5 text-xs font-medium">Private</span>}</h2>{repo.description && <p className="mt-1 text-sm text-slate-600">{repo.description}</p>}<p className="mt-1 text-sm text-slate-500">{repo.selected ? "Following, awaiting sync" : "Not following"}</p></div>
      <Button onClick={() => toggle(repo)} disabled={busy === repo.id}>{repo.selected ? "Unfollow" : "Follow"}<span className="sr-only"> {repo.fullName}</span></Button>
    </li>)}</ul>
    {listing && listing.nextPage > 0 && <Button className="mt-6" onClick={() => load(listing.nextPage)} disabled={loading}>Load more repositories</Button>}
  </main>;
}

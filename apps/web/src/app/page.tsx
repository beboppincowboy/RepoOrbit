"use client";
import Link from "next/link";
import { Button } from "@/components/ui/button";
import { useEffect, useState } from "react";
import { repositories, type RepositoryConnection } from "@/lib/api";
import { useSession } from "@/components/auth-shell";

export default function Home() {
  const demo = useSession()?.mode === "demo";
  const [data, setData] = useState<RepositoryConnection | null>(null);
  const [error, setError] = useState(false);
  const [loading, setLoading] = useState(true);
  useEffect(() => { repositories().then(d => { setData(d); setError(false); }).catch(() => setError(true)).finally(() => setLoading(false)); }, []);
  async function more() {
    if (!data?.endCursor) return;
    setLoading(true);
    try { const next = await repositories(data.endCursor); setData({ ...next, nodes: [...data.nodes, ...next.nodes] }); setError(false); } catch { setError(true); } finally { setLoading(false); }
  }
  return <main className="mx-auto max-w-5xl px-5 py-12">
    <header className="mb-8"><p className="text-sm font-semibold uppercase tracking-wider text-indigo-700">RepoOrbit{demo ? " · Demo" : ""}</p><h1 className="mt-2 text-3xl font-bold">Repository overview</h1><p className="mt-3 max-w-2xl text-slate-600">{demo ? "Sample data stored in the local database. This is not current GitHub data; synchronization and history are not available in this demo." : "Repositories you connect appear here. Synchronization and history are not available yet."}</p></header>
    {loading && <p role="status">Loading repositories…</p>}
    {error && <p role="alert" className="text-red-700">Could not load repositories. Check that the local services are running.</p>}
    {data && (data.withheld > 0 || data.retryAt) && <p role="status" className="mb-4 rounded border border-amber-300 bg-amber-50 p-3 text-amber-900">{data.withheld} connected {data.withheld === 1 ? "repository is" : "repositories are"} hidden because GitHub access could not be verified{data.retryAt ? ` (rate limited; retry after ${new Date(data.retryAt).toLocaleTimeString()})` : ""}. This is not an empty list; reload to retry.</p>}
    {data && data.nodes.length === 0 && !data.withheld && !data.retryAt && <p>{demo ? "No demo repositories yet." : <>No repositories connected yet. <Link className="underline" href="/repositories">Choose repositories to connect</Link>.</>}</p>}
    <div className="grid gap-4 md:grid-cols-2">{data?.nodes.map(repo => <section key={repo.id} className="rounded-lg border border-slate-200 bg-white p-5 shadow-sm"><h2 className="text-xl font-semibold">{repo.name}</h2><p className="mt-2 text-slate-600">{repo.description}</p>{repo.syncState === "awaiting-sync" && <p className="mt-2 text-sm font-medium text-amber-800">Selected, awaiting sync — no activity data yet.</p>}<h3 className="mt-5 font-semibold">Sample issues</h3>{repo.issues.nodes.length ? <ul className="mt-2 space-y-2">{repo.issues.nodes.map(issue => <li key={issue.id}>{issue.title} <span className="text-sm text-slate-500">({issue.state})</span></li>)}</ul> : <p className="mt-2 text-slate-500">{repo.issues.hasNextPage ? "No issues in the loaded sample." : "No demo issues."}</p>}{repo.issues.hasNextPage && <p className="mt-2 text-sm text-slate-500">Partial issue list; more sample issues are available through the API.</p>}</section>)}</div>
    {data?.hasNextPage && <Button className="mt-6" onClick={more} disabled={loading}>Load more repositories</Button>}
  </main>;
}

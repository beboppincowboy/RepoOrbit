import { useQuery } from "@tanstack/react-query";
import { Link, createFileRoute } from "@tanstack/react-router";
import type { ReactNode } from "react";
import { LogoMark } from "@/shared/ui/logo";
import { sessionQuery } from "@/features/auth/api";

type LoginSearch = { error?: string };

export const Route = createFileRoute("/login")({
  validateSearch: (search: Record<string, unknown>): LoginSearch => (typeof search.error === "string" ? { error: search.error } : {}),
  component: Login,
});

const icon = (d: ReactNode) => <svg width="16" height="16" viewBox="0 0 16 16" aria-hidden="true" className="mx-auto">{d}</svg>;
const pillars = [
  ["Analyze", "activity", icon(<path d="M2 2h5v5H2zM9 2h5v5H9zM2 9h5v5H2zM9 9h5v5H9z" fill="none" stroke="currentColor" strokeWidth="1.4" />)],
  ["Plan", "board", icon(<><rect x="2" y="2.5" width="12" height="11" rx="1.5" fill="none" stroke="currentColor" strokeWidth="1.4" /><path d="M6 2.5v11M10 2.5v11" stroke="currentColor" strokeWidth="1.4" /></>)],
  ["Track", "PRs", icon(<><circle cx="4" cy="3.5" r="1.8" fill="none" stroke="currentColor" strokeWidth="1.4" /><circle cx="4" cy="12.5" r="1.8" fill="none" stroke="currentColor" strokeWidth="1.4" /><circle cx="12" cy="12.5" r="1.8" fill="none" stroke="currentColor" strokeWidth="1.4" /><path d="M4 5.3v5.4M12 10.7V6a2 2 0 0 0-2-2H7.5" fill="none" stroke="currentColor" strokeWidth="1.4" /></>)],
  ["Monitor", "CI/CD", icon(<><circle cx="4" cy="4" r="1.8" fill="none" stroke="currentColor" strokeWidth="1.4" /><rect x="9.5" y="9.5" width="4" height="4" rx="1" fill="none" stroke="currentColor" strokeWidth="1.4" /><path d="M4 5.8V9a2 2 0 0 0 2 2h3.5" fill="none" stroke="currentColor" strokeWidth="1.4" /></>)],
] as const;
const btn = "inline-flex items-center justify-center rounded-lg border px-3 py-2.5 no-underline";
const center = (body: ReactNode) => <main className="grid h-full place-items-center">{body}</main>;

// Prototype D2 sign-in card: GitHub sign-in in real mode, the labelled demo in Demo mode.
function Login() {
  const { data: session, isError, refetch } = useQuery(sessionQuery);
  const { error } = Route.useSearch();
  if (isError) return center(<p role="alert">Could not load the session. <button className="underline" onClick={() => refetch()}>Try again</button></p>);
  if (!session) return center(<p role="status">Loading…</p>);
  return center(<div className="flex w-[min(460px,100%)] flex-col gap-3.5 rounded-xl border border-border bg-card p-7">
    <div className="flex items-center gap-2 text-[15px] font-semibold"><LogoMark />RepoOrbit</div>
    <h1 className="text-[22px]">Your engineering command center</h1>
    <p className="m-0 text-muted-foreground">Repository analytics, issue boards and CI/CD status for the repositories you work on, built on GitHub.</p>
    <div className="grid grid-cols-4 gap-2">{pillars.map(([title, sub, svg]) => <div key={title} className="rounded-[10px] border border-border p-2 text-center text-xs">{svg}<b className="block text-[13px]">{title}</b><span className="text-muted-foreground">{sub}</span></div>)}</div>
    {session.mode === "demo" ? <>
      <Link className={`${btn} border-border bg-card text-foreground hover:bg-muted`} to="/">Explore demo mode</Link>
      <p className="m-0 font-mono text-xs text-muted-foreground">Demo mode: no GitHub credentials are configured, so RepoOrbit shows sample data under a fixed demo identity. Nothing is read from or written to GitHub.</p>
    </> : session.user ? <Link className={`${btn} border-foreground bg-foreground font-medium text-background`} to="/">Continue to your repositories</Link> : <>
      {error !== undefined && <p role="alert" className="m-0 text-destructive">Sign-in did not complete. Please try again.</p>}
      <a className={`${btn} border-foreground bg-foreground font-medium text-background`} href="/auth/github/login">Continue with GitHub</a>
    </>}
  </div>);
}

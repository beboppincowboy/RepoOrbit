import type { ReactNode } from "react";
import { Link, useRouterState } from "@tanstack/react-router";
import { logout, type Me } from "@/lib/api";

// Overview, Repositories and Starred are built. Discover and Saved issues are the U7b screens: listed so the layout is
// final, but not links until they exist.
const primary = [
  { to: "/", label: "Overview", exact: true },
  { to: "/repositories", label: "Repositories", exact: false },
  { to: "/starred", label: "Starred", exact: false },
] as const;
const planned = ["Discover", "Saved issues"] as const;

const titles: Record<string, string> = { "/": "Overview", "/repositories": "Repositories", "/starred": "Starred" };

// Linear-style shell: sidebar navigation and a top bar. The viewport is fixed; each screen scrolls its own lists.
export function AppShell({ session, children }: { session: Me; children: ReactNode }) {
  const demo = session.mode === "demo";
  const pathname = useRouterState({ select: state => state.location.pathname });
  const title = titles[pathname] ?? "RepoOrbit";
  async function signOut() {
    await logout();
    window.location.assign("/login");
  }
  return <div data-app-shell className="flex h-dvh overflow-hidden bg-background text-foreground">
    <aside className="flex w-52 shrink-0 flex-col gap-3 border-r border-border bg-card p-3">
      <div className="flex items-center gap-2 px-2 py-1 text-sm font-semibold">RepoOrbit{demo && <span className="rounded bg-warning/15 px-1.5 py-0.5 text-xs font-medium text-warning">Demo</span>}</div>
      <nav aria-label="Primary" className="flex flex-col gap-0.5">
        {primary.map(item => <Link key={item.to} to={item.to} activeOptions={{ exact: item.exact }} activeProps={{ className: "bg-muted font-medium text-foreground" }} className="rounded-md px-2 py-1.5 text-sm text-muted-foreground hover:bg-muted">{item.label}</Link>)}
        {planned.map(label => <span key={label} aria-disabled="true" className="flex items-center justify-between rounded-md px-2 py-1.5 text-sm text-muted-foreground/60">{label}<span className="text-xs">Soon</span></span>)}
      </nav>
    </aside>
    <div className="flex min-w-0 flex-1 flex-col">
      <header className="flex h-12 shrink-0 items-center justify-between gap-4 border-b border-border bg-card px-4 text-sm">
        <p className="font-medium" aria-current="page">{title}</p>
        <nav aria-label="Account" className="flex items-center gap-3">
          {demo && <span className="rounded bg-warning/15 px-2 py-0.5 font-semibold text-warning">Demo mode</span>}
          {session.user && <span className="text-muted-foreground">{demo ? "Demo identity" : "Signed in as"} <strong className="text-foreground">{session.user.login}</strong></span>}
          {!demo && session.user && <button className="underline" onClick={signOut}>Sign out</button>}
        </nav>
      </header>
      <main className="min-h-0 flex-1 overflow-hidden">{children}</main>
    </div>
  </div>;
}

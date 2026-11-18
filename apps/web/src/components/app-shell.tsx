import { useState, type ReactNode } from "react";
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

// Linear-style shell: sidebar navigation and a top bar. The viewport is fixed; each screen scrolls its own lists.
export function AppShell({ session, children }: { session: Me; children: ReactNode }) {
  const demo = session.mode === "demo";
  const pathname = useRouterState({ select: state => state.location.pathname });
  const title = primary.find(item => item.to === pathname)?.label ?? "Repository workspace";
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
  return <div data-app-shell className="mx-auto flex h-dvh max-w-[1600px] flex-col gap-3 overflow-hidden bg-background p-2 text-foreground min-[901px]:flex-row min-[901px]:px-4 min-[901px]:py-3">
    <aside className="flex shrink-0 overflow-x-auto rounded-xl border border-border bg-card p-2 min-[901px]:w-[216px] min-[901px]:flex-col min-[901px]:gap-3 min-[901px]:p-3">
      <div className="hidden items-center gap-2 px-2 py-1 text-sm font-semibold min-[901px]:flex">RepoOrbit{demo && <span className="rounded bg-warning/15 px-1.5 py-0.5 text-xs font-medium text-warning">Demo</span>}</div>
      <nav aria-label="Primary" className="flex gap-0.5 whitespace-nowrap min-[901px]:flex-col">
        {primary.map(item => <Link key={item.to} to={item.to} activeOptions={{ exact: item.exact }} activeProps={{ className: "bg-muted font-medium text-foreground" }} className="rounded-md px-2 py-1.5 text-sm text-muted-foreground hover:bg-muted">{item.label}</Link>)}
        {planned.map(label => <span key={label} aria-disabled="true" className="hidden items-center justify-between rounded-md px-2 py-1.5 text-sm text-muted-foreground/60 min-[901px]:flex">{label}<span className="text-xs">Soon</span></span>)}
      </nav>
    </aside>
    <div className="flex min-h-0 min-w-0 flex-1 flex-col gap-3">
      <header className="flex min-h-11 shrink-0 flex-wrap items-center justify-between gap-x-4 gap-y-1 rounded-xl border border-border bg-card px-4 py-1.5 text-sm">
        <p className="font-medium" aria-current="page">{title}</p>
        <nav aria-label="Account" className="flex items-center gap-3">
          {demo && <span className="rounded bg-warning/15 px-2 py-0.5 font-semibold text-warning">Demo mode</span>}
          {session.user && <span className="text-muted-foreground">{demo ? "Demo identity" : "Signed in as"} <strong className="text-foreground">{session.user.login}</strong></span>}
          {logoutError && <span role="alert">Could not sign out. Try again.</span>}
          {!demo && session.user && <button className="underline" onClick={signOut}>Sign out</button>}
        </nav>
      </header>
      <main className="min-h-0 flex-1 overflow-hidden">{children}</main>
    </div>
  </div>;
}

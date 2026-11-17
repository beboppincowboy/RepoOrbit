"use client";
import { createContext, useContext, useEffect, useState, type ReactNode } from "react";
import Link from "next/link";
import { usePathname, useRouter } from "next/navigation";
import { logout, me, type Me } from "@/lib/api";

const Session = createContext<Me | null>(null);
export const useSession = () => useContext(Session);

// Resolves the session once, sends signed-out visitors to /login in real mode, and shows who is signed in.
export function AuthShell({ children }: { children: ReactNode }) {
  const [session, setSession] = useState<Me | null>(null);
  const [error, setError] = useState(false);
  const pathname = usePathname();
  const router = useRouter();
  useEffect(() => { me().then(setSession).catch(() => setError(true)); }, [pathname]);
  const needsLogin = session?.mode === "real" && !session.user && pathname !== "/login";
  useEffect(() => { if (needsLogin) router.replace("/login"); }, [needsLogin, router]);
  async function signOut() { await logout(); window.location.assign("/login"); }
  if (error) return <p role="alert" className="p-6 text-red-700">Could not reach the RepoOrbit API. Check that the local services are running.</p>;
  if (!session || needsLogin) return <p role="status" className="p-6">Loading…</p>;
  return <Session.Provider value={session}>
    <nav aria-label="Account" className="flex items-center justify-end gap-3 border-b border-slate-200 bg-white px-5 py-2 text-sm">
      {session.mode === "real" && session.user && <span className="mr-auto flex gap-4"><Link className="underline" href="/">Overview</Link><Link className="underline" href="/repositories">Repositories</Link><Link className="underline" href="/starred">Starred</Link></span>}
      {session.mode === "demo" && <span className="rounded bg-amber-100 px-2 py-0.5 font-semibold text-amber-900">Demo mode</span>}
      {session.user && <span>{session.mode === "demo" ? "Demo identity" : "Signed in as"} <strong>{session.user.login}</strong></span>}
      {session.mode === "real" && session.user && <button className="underline" onClick={signOut}>Sign out</button>}
    </nav>
    {children}
  </Session.Provider>;
}

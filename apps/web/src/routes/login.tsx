import { useQuery } from "@tanstack/react-query";
import { Link, createFileRoute } from "@tanstack/react-router";
import { sessionQuery } from "@/lib/api";

type LoginSearch = { error?: string };

export const Route = createFileRoute("/login")({
  validateSearch: (search: Record<string, unknown>): LoginSearch => (typeof search.error === "string" ? { error: search.error } : {}),
  component: Login,
});

function Login() {
  const { data: session } = useQuery(sessionQuery);
  const { error } = Route.useSearch();
  if (!session) return <p role="status" className="p-6">Loading…</p>;
  return <main className="mx-auto h-full max-w-md overflow-y-auto px-5 py-16">
    <h1 className="text-3xl font-bold">RepoOrbit</h1>
    {session.mode === "demo" ? <>
      <p className="mt-4 text-muted-foreground">Demo mode: no GitHub credentials are configured, so RepoOrbit shows sample data under a fixed demo identity. Nothing is read from or written to GitHub.</p>
      <Link className="mt-6 inline-block underline" to="/">Open the demo</Link>
    </> : session.user ? <Link className="mt-6 inline-block underline" to="/">Continue to your repositories</Link> : <>
      <p className="mt-4 text-muted-foreground">Sign in with GitHub to see your repositories.</p>
      {error !== undefined && <p role="alert" className="mt-4 text-destructive">Sign-in did not complete. Please try again.</p>}
      <a className="mt-6 inline-block rounded-md bg-primary px-4 py-2 font-semibold text-primary-foreground" href="/auth/github/login">Sign in with GitHub</a>
    </>}
  </main>;
}

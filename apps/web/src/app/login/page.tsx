"use client";
import Link from "next/link";
import { useSession } from "@/components/auth-shell";

export default function Login() {
  const session = useSession();
  const failed = typeof window !== "undefined" && new URLSearchParams(window.location.search).has("error");
  return <main className="mx-auto max-w-md px-5 py-16">
    <h1 className="text-3xl font-bold">RepoOrbit</h1>
    {session?.mode === "demo" ? <>
      <p className="mt-4 text-slate-600">Demo mode: no GitHub credentials are configured, so RepoOrbit shows sample data under a fixed demo identity. Nothing is read from or written to GitHub.</p>
      <Link className="mt-6 inline-block underline" href="/">Open the demo</Link>
    </> : session?.user ? <Link className="mt-6 inline-block underline" href="/">Continue to your repositories</Link> : <>
      <p className="mt-4 text-slate-600">Sign in with GitHub to see your repositories.</p>
      {failed && <p role="alert" className="mt-4 text-red-700">Sign-in did not complete. Please try again.</p>}
      <a className="mt-6 inline-block rounded-md bg-slate-900 px-4 py-2 font-semibold text-white" href="/auth/github/login">Sign in with GitHub</a>
    </>}
  </main>;
}

"use client";
import { useState } from "react";
import { refreshActivity, type ActivityResourceStatus, type ActivityStatus, type ChildResourceStatus } from "@/lib/api";
import { Button } from "@/components/ui/button";

const when = (iso: string) => new Date(iso).toLocaleString();
// Mirrors the backend's safeActivityCode allowlist; anything else is shown as "unknown", never as server text.
const knownCodes = new Set(["no_credential", "rate_limited", "access_unverified", "access_changed", "upstream_error", "unknown"]);
const safe = (code: string) => (knownCodes.has(code) ? code : "unknown").replace(/_/g, " ");

const failNote = (code: string | null) => (code ? ` Last attempt failed (${safe(code)}).` : "");
const nextNote = (at: string | null) => (at ? ` Next attempt ${when(at)}.` : "");

function stream(name: string, r: ActivityResourceStatus): string {
  const fail = failNote(r.failureCode);
  if (r.jobState === "dead") return `${name}: stopped after repeated failures; an operator must requeue it.${fail}`;
  if (r.jobState === "paused") return `${name}: paused pending authorization.${fail}`;
  const next = r.jobState === "queued" ? nextNote(r.nextAttemptAt) : "";
  if (r.phase === "current") return `${name}: synchronized${r.lastSuccessAt ? ` as of ${when(r.lastSuccessAt)}` : ""}.${fail}${next}`;
  const through = r.catchupThrough ? ` through ${when(r.catchupThrough)}` : "";
  return `${name}: importing (${r.phase === "backfill" || r.phase === "catchup" ? r.phase : "in progress"}${through}).${fail}${next}`;
}

function child(name: string, r: ChildResourceStatus): string {
  const fail = failNote(r.failureCode);
  const next = nextNote(r.nextAttemptAt);
  if (r.complete && !r.pendingChildren) return `${name}: complete${r.lastSuccessAt ? ` as of ${when(r.lastSuccessAt)}` : ""}.${fail}`;
  return `${name}: incomplete, ${r.pendingChildren} pull request${r.pendingChildren === 1 ? "" : "s"} pending.${fail}${next}`;
}

// Plain-language, per-stream lines. "Synchronized overall" is claimed only when the backend says so.
export function describeActivity(s: ActivityStatus): string[] {
  const lines: string[] = [];
  if (s.issues) lines.push(stream("Issues", s.issues));
  if (s.pullRequests) lines.push(stream("Pull requests", s.pullRequests));
  if (s.reviews) lines.push(child("Reviews", s.reviews));
  if (s.comments) lines.push(child("Comments", s.comments));
  if (s.releases) lines.push(stream("Releases", s.releases));
  if (s.issueEvents?.traversal) lines.push(stream("Issue events", s.issueEvents.traversal));
  if (s.issueEvents?.historicalGap) lines.push("Issue events: history before sync began is permanently unavailable; only events seen since then are recorded.");
  if (s.synchronized) lines.unshift("All activity streams are synchronized.");
  return lines;
}

export function ActivityStatusLines({ status }: { status: ActivityStatus | null }) {
  const lines = status ? describeActivity(status) : [];
  if (!lines.length) return null;
  return <ul aria-label="Activity status" className="mt-2 space-y-1 text-sm text-slate-600">{lines.map(line => <li key={line}>{line}</li>)}</ul>;
}

export function RefreshActivityButton({ repositoryId }: { repositoryId: string }) {
  const [feedback, setFeedback] = useState<{ kind: "status" | "alert"; text: string } | null>(null);
  const [busy, setBusy] = useState(false);
  async function click() {
    setBusy(true);
    setFeedback({ kind: "status", text: "Refreshing activity…" });
    try {
      const state = await refreshActivity(repositoryId);
      setFeedback({ kind: "status", text: state === "coalesced" ? "A refresh is already queued or running; it will include your request." : "Activity refresh requested." });
    } catch (e) {
      setFeedback({ kind: "alert", text: e instanceof Error ? e.message : "Could not request an activity refresh." });
    } finally { setBusy(false); }
  }
  return <div className="mt-2">
    <Button onClick={click} disabled={busy}>Refresh activity</Button>
    <div aria-live="polite">{feedback?.kind === "status" && <p role="status" className="mt-1 text-sm text-slate-600">{feedback.text}</p>}</div>
    {feedback?.kind === "alert" && <p role="alert" className="mt-1 text-sm text-red-700">{feedback.text}</p>}
  </div>;
}

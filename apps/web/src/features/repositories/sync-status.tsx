import { useState } from "react";
import { refreshActivity, type ActivityResourceStatus, type ActivityStatus, type ChildResourceStatus, type MetadataStatus, type SyncState } from "@/features/repositories/api";
import { Button } from "@/shared/ui/button";

const when = (iso: string) => new Date(iso).toLocaleString();
// Mirrors the backend's safeActivityCode allowlist; anything else is shown as "unknown", never as server text.
const knownCodes = new Set(["no_credential", "rate_limited", "access_unverified", "access_changed", "upstream_error", "unknown"]);
const safe = (code: string) => (knownCodes.has(code) ? code : "unknown").replace(/_/g, " ");

const failNote = (code: string | null) => (code ? ` Last attempt failed (${safe(code)}).` : "");
const nextNote = (at: string | null) => (at ? ` Next attempt ${when(at)}.` : "");

function stream(name: string, r: ActivityResourceStatus): string {
  if (r.phase === "disabled") return `${name}: disabled on GitHub.`;
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
  // Comments are one stream with no per-PR children, so pendingChildren is always 0 for them.
  if (!r.pendingChildren) return `${name}: importing.${fail}${next}`;
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

// Metadata lines never mention activity: that is reported separately.
export function describeMetadata(s: MetadataStatus): string[] {
  const lines: string[] = [];
  if (s.state === "running") lines.push("Metadata refresh running.");
  else if (s.state === "queued") lines.push(s.nextAttemptAt ? `Metadata refresh queued for ${when(s.nextAttemptAt)}.` : "Metadata refresh queued.");
  else if (s.state === "paused") lines.push("Metadata refresh paused: no usable GitHub authorization. It resumes after you sign in again or access is restored.");
  else if (s.state === "dead") lines.push("Metadata refresh stopped after repeated failures; an operator must requeue it.");
  lines.push(s.lastSuccessAt ? `Metadata last refreshed ${when(s.lastSuccessAt)}.` : "Metadata not refreshed yet.");
  if (s.failureCode && s.state !== "running") lines.push(`Last refresh attempt failed (${s.failureCode.replace(/_/g, " ")}).`);
  if (s.followSuspended) lines.push("You no longer star this repository, so it is not used for synchronization. Star it again to resume.");
  return lines;
}

// Every state the UI can show for one repository. "demo" is always labeled, never implied.
type SyncDisplay = "demo" | "access-unverified" | "rate-limited" | "syncing" | "stopped" | "synced" | "partial" | "awaiting" | "not-synchronized";
export const syncLabels: Record<SyncDisplay, string> = {
  demo: "Demo data",
  "access-unverified": "Access unverified",
  "rate-limited": "Rate limited",
  syncing: "Syncing",
  stopped: "Sync stopped",
  synced: "Synced",
  partial: "Partial coverage",
  awaiting: "Awaiting sync",
  "not-synchronized": "Not synchronized",
};
// Prototype `.sync` pill tones: [text color, background].
const tones: Record<SyncDisplay, [string, string]> = {
  demo: ["text-demo", "bg-demo-soft"],
  "access-unverified": ["text-destructive", "bg-destructive-soft"],
  "rate-limited": ["text-destructive", "bg-destructive-soft"],
  syncing: ["text-warning", "bg-warning-soft"],
  stopped: ["text-destructive", "bg-destructive-soft"],
  synced: ["text-success-foreground", "bg-success-soft"],
  partial: ["text-warning", "bg-warning-soft"],
  awaiting: ["text-warning", "bg-warning-soft"],
  "not-synchronized": ["text-warning", "bg-warning-soft"],
};

export function LoadingCircle({ className = "size-4" }: { className?: string }) {
  return <span aria-hidden="true" className={`inline-block shrink-0 rounded-full border-[1.5px] border-current border-r-transparent motion-safe:animate-spin ${className}`} />;
}

// The pill's 7px dot on its own (sidebar rows). Carries no label or data-sync-state; callers name the state themselves.
export function SyncDot({ display }: { display: SyncDisplay }) {
  return <span aria-hidden="true" className={`inline-block size-[7px] shrink-0 rounded-full bg-current ${tones[display][0]}`} />;
}

export type SyncInput = {
  syncState: SyncState;
  activity?: ActivityStatus | null;
  metadataStatus?: MetadataStatus | null;
  withheld?: number;
  retryAt?: string | null;
};

// Show failures before coverage and keep background work separate from established coverage.
export function syncDisplay({ syncState, activity = null, metadataStatus = null, withheld = 0, retryAt = null }: SyncInput): SyncDisplay {
  if (syncState === "demo") return "demo";
  const streams: ActivityResourceStatus[] = activity ? [activity.issues, activity.pullRequests, activity.releases, activity.issueEvents?.traversal].filter((s): s is ActivityResourceStatus => !!s) : [];
  const children: ChildResourceStatus[] = activity ? [activity.reviews, activity.comments].filter((s): s is ChildResourceStatus => !!s) : [];
  const codes = [...streams, ...children].map(s => s.failureCode);
  if (withheld > 0 || codes.includes("access_unverified") || metadataStatus?.state === "paused") return "access-unverified";
  if (retryAt || codes.includes("rate_limited")) return "rate-limited";
  if (metadataStatus?.state === "dead" || metadataStatus?.failureCode || codes.some(Boolean) || streams.some(s => s.jobState === "dead" || s.jobState === "paused")) return "stopped";
  if (activity?.synchronized) return "synced";
  if (streams.some(s => s.phase !== "current" && s.phase !== "disabled") || children.some(s => !s.complete)) return "partial";
  if (metadataStatus?.state === "running" || streams.some(s => s.jobState === "running")) return "syncing";
  if (syncState === "awaiting-sync") return "awaiting";
  return "not-synchronized";
}

// One reusable status: the state badge, then the detail lines for whatever data is passed in.
export function SyncStatus({ compact = false, ...props }: SyncInput & { compact?: boolean }) {
  const display = syncDisplay(props);
  const badge = <span className={`inline-flex items-center gap-1.5 whitespace-nowrap rounded-full px-[9px] py-0.5 text-xs leading-[1.5] ${tones[display].join(" ")}`} data-sync-state={display}>
    <SyncDot display={display} /><span className={compact && display === "access-unverified" ? "sr-only" : undefined}>{syncLabels[display]}</span>
  </span>;
  if (compact) return badge;
  const metadata = props.metadataStatus ? describeMetadata(props.metadataStatus) : [];
  const activity = props.activity ? [...(props.activity.synchronized ? [] : [props.activity.message]), ...describeActivity(props.activity)] : [];
  const list = (label: string, lines: string[]) => <ul aria-label={label} className="space-y-1 text-sm text-muted-foreground">{lines.map(line => <li key={line}>{line}</li>)}</ul>;
  return <div className="space-y-2">
    {badge}
    {metadata.length > 0 && list("Metadata status", metadata)}
    {activity.length > 0 && list("Activity status", activity)}
  </div>;
}

// Queues a repository activity refresh. Calls onAccepted for both a new job and a coalesced request.
// Displays a request failure inline and leaves the button available to retry.
export function RefreshActivityButton({ repositoryId, onAccepted }: { repositoryId: string; onAccepted: () => void }) {
  const [feedback, setFeedback] = useState<{ kind: "status" | "alert"; text: string } | null>(null);
  const [busy, setBusy] = useState(false);
  async function click() {
    setBusy(true);
    setFeedback({ kind: "status", text: "Refreshing activity…" });
    try {
      const state = await refreshActivity(repositoryId);
      onAccepted();
      setFeedback({ kind: "status", text: state === "coalesced" ? "A refresh is already queued or running; it will include your request." : "Activity refresh requested." });
    } catch (e) {
      setFeedback({ kind: "alert", text: e instanceof Error ? e.message : "Could not request an activity refresh." });
    } finally { setBusy(false); }
  }
  return <div className="mt-2">
    <Button onClick={click} disabled={busy}>Refresh activity</Button>
    <div aria-live="polite">{feedback?.kind === "status" && <p role="status" className="mt-1 text-sm text-muted-foreground">{feedback.text}</p>}</div>
    {feedback?.kind === "alert" && <p role="alert" className="mt-1 text-sm text-destructive">{feedback.text}</p>}
  </div>;
}

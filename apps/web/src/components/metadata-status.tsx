import type { MetadataStatus } from "@/lib/api";

const when = (iso: string) => new Date(iso).toLocaleString();

// Plain-language lines for the shared metadata refresh. Never mentions activity: that is reported separately.
// failureCode is a safe machine code; unknown codes get a generic sentence, never raw text.
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

export function MetadataStatusLines({ status }: { status: MetadataStatus | null }) {
  if (!status) return null;
  return <ul aria-label="Metadata status" className="mt-2 space-y-1 text-sm text-slate-600">{describeMetadata(status).map(line => <li key={line}>{line}</li>)}</ul>;
}

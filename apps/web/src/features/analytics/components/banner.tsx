import type { ReactNode } from "react";
import type { RepositoryOverview } from "@/features/analytics/api";
import { icons } from "@/features/analytics/components/workspace-parts";
import { ago, clock, day, list } from "@/features/analytics/format";
import type { ActivityResourceStatus } from "@/features/repositories/api";
import type { SyncInput } from "@/features/repositories/sync-status";

const metricsOf = (o: RepositoryOverview) => [o.firstResponseTime.coverage, o.mergeTime.coverage, o.openIssues.coverage, o.pullRequestsMerged.coverage, o.contributorsCoverage];
const streamNames: [string, (a: RepositoryOverview["activity"]) => ActivityResourceStatus | null | undefined][] = [
  ["issues", a => a.issues], ["pull requests", a => a.pullRequests], ["releases", a => a.releases], ["issue events", a => a.issueEvents?.traversal],
];

export function Banner({ overview, display, sync }: { overview: RepositoryOverview; display: string; sync: SyncInput }) {
  const a = overview.activity;
  const note = (text: ReactNode, warn = true) => <div role="status" className={`flex flex-none items-start gap-2.5 rounded-[10px] border px-3 py-2.5 text-[13px] ${warn ? "border-warning bg-warning-soft" : "border-border bg-card"}`}>
    <span className="mt-[3px]">{icons.info}</span><span>{text}</span>
  </div>;
  const streams = streamNames.map(([label, get]) => [label, get(a)] as const).filter((s): s is readonly [string, ActivityResourceStatus] => !!s[1]);
  const uncovered = metricsOf(overview).filter(c => !c.covered);
  if (display === "rate-limited") {
    const last = streams.map(([, s]) => s.lastSuccessAt).filter((t): t is string => !!t).sort().at(-1);
    const retry = streams.filter(([, s]) => s.failureCode === "rate_limited").map(([, s]) => s.nextAttemptAt).filter((t): t is string => !!t).sort()[0];
    return note(`Rate limited. Showing the last good data from ${last ? ago(last) : "an earlier sync"}${retry ? `; next retry ${clock.format(new Date(retry))}` : ""}.`);
  }
  if (display === "syncing") {
    const running = streams.filter(([, s]) => s.jobState === "running").map(([label]) => label);
    return note(`Syncing ${running.length ? list(running) : "activity"}. Figures update as data arrives.`, false);
  }
  if (display === "stopped" || display === "access-unverified") {
    const failures = [
      ...streams.map(([label, status]) => [label, status.failureCode ?? (status.jobState === "dead" || status.jobState === "paused" ? status.jobState : null)] as const),
      ["comments", a.comments?.failureCode],
      ["reviews", a.reviews?.failureCode],
      ["metadata", sync.metadataStatus?.failureCode ?? (sync.metadataStatus?.state === "paused" || sync.metadataStatus?.state === "dead" ? sync.metadataStatus.state : null)],
    ].filter(([, code]) => Boolean(code));
    return note(failures.length
      ? `Sync needs attention. ${failures.map(([label, code]) => `${label} (${code?.replace(/_/g, " ")})`).join(", ")}.`
      : "GitHub access could not be verified for sync.");
  }
  if (display === "awaiting" || display === "not-synchronized" || (uncovered.length && uncovered.every(c => c.reason === "not_synchronized"))) {
    return note("First sync has not started. Figures show — until data exists.", false);
  }
  const showHistoryGap = display !== "synced" && (a.issueEvents?.historicalGap || uncovered.some(c => c.reason === "historical_gap"));
  if (uncovered.some(c => c.reason !== "historical_gap") || showHistoryGap) {
    const reasons = new Set(uncovered.map(c => c.reason));
    const parts = [
      showHistoryGap ? (overview.eventsCoveredFrom ? `Issue events before ${day.format(new Date(overview.eventsCoveredFrom))} were never recorded` : "Issue event history is not recorded yet") : "",
      reasons.has("children_incomplete") ? `Reviews and comments are still importing${a.reviews?.pendingChildren ? ` (${a.reviews.pendingChildren} pull requests pending)` : ""}` : "",
      reasons.has("not_synchronized") ? "Some activity has not finished its first import" : "",
    ].filter(Boolean);
    return note(<><b className="font-semibold">Partial coverage.</b> {parts.length ? `${parts.join("; ")}.` : "Some figures are not covered for this window."}</>);
  }
  return null;
}

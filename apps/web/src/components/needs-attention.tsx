import { Link } from "@tanstack/react-router";
import type { AttentionItem, AttentionKind, MetricWindow } from "@/lib/api";

// Rows and icons shared by the overview lists (prototype D2 `.lrow`, `.ico`, `.mini`, I.chev / I.pr / I.issue).
export const listRowClass = "flex w-full items-center gap-3 border-b border-border px-3 py-2.5 text-left last:border-b-0 hover:bg-muted";
export const emptyListClass = "p-4 text-muted-foreground";
export const pillClass = "inline-flex shrink-0 items-center rounded-full border border-border px-[7px] text-[11px] text-soft";

export function ChevronIcon({ className }: { className?: string }) {
  return <svg width="14" height="14" viewBox="0 0 16 16" aria-hidden="true" className={`shrink-0 text-muted-foreground ${className ?? ""}`}><path d="M6 3.5l4.5 4.5L6 12.5" fill="none" stroke="currentColor" strokeWidth="1.5" /></svg>;
}
export function PullRequestIcon() {
  return <svg width="14" height="14" viewBox="0 0 16 16" aria-hidden="true"><g fill="none" stroke="currentColor" strokeWidth="1.4"><circle cx="4" cy="3.5" r="1.8" /><circle cx="4" cy="12.5" r="1.8" /><circle cx="12" cy="12.5" r="1.8" /><path d="M4 5.3v5.4M12 10.7V6a2 2 0 0 0-2-2H7.5" /></g></svg>;
}
export function IssueIcon() {
  return <svg width="14" height="14" viewBox="0 0 16 16" aria-hidden="true"><circle cx="8" cy="8" r="6" fill="none" stroke="currentColor" strokeWidth="1.4" /><circle cx="8" cy="8" r="1.4" fill="currentColor" /></svg>;
}

const shortDate = new Intl.DateTimeFormat("en", { month: "short", day: "numeric", timeZone: "UTC" });
// Problem wording per kind; `since` is the opening time, or the last update for stale issues.
const kinds: Record<AttentionKind, { label: (n: number) => string; reason: (since: string) => string; pill: string; tab: "pulls" | "overview" }> = {
  pr_awaiting_review: { label: n => `PR #${n}`, reason: () => "Awaiting first review", pill: "Review", tab: "pulls" },
  issue_no_response: { label: n => `Issue #${n}`, reason: since => `No response since ${shortDate.format(new Date(since))}`, pill: "Issues", tab: "overview" },
  stale_issue: { label: n => `Issue #${n}`, reason: () => "Open, 30+ days quiet", pill: "Issues", tab: "overview" },
};

// Body of the "Needs attention" panel: one row per concrete problem, each opening its repository workspace in the same window.
export function NeedsAttentionList({ items, days }: { items: AttentionItem[]; days: MetricWindow }) {
  if (!items.length) return <p className={emptyListClass}>Nothing needs attention right now.</p>;
  return items.map(item => {
    const kind = kinds[item.kind];
    const name = item.fullName.split("/")[1] ?? item.fullName;
    return <Link key={`${item.kind}:${item.repositoryId}:${item.number}`} to="/workspace/$repositoryId" params={{ repositoryId: item.repositoryId }} search={{ days, tab: kind.tab }} className={listRowClass}>
      <span className="grid size-7 shrink-0 place-items-center rounded-lg bg-warning-soft text-warning">{item.kind === "pr_awaiting_review" ? <PullRequestIcon /> : <IssueIcon />}</span>
      <span className="min-w-0 flex-1">
        <span className="block font-medium">{name} · {kind.label(item.number)}</span>
        <span className="block truncate text-xs text-muted-foreground">{kind.reason(item.since)} · {item.title}</span>
      </span>
      <span className={pillClass}>{kind.pill}</span>
      <ChevronIcon />
    </Link>;
  });
}

import { Link } from "@tanstack/react-router";
import type { AttentionItem, AttentionKind } from "@/features/analytics/api";
import type { MetricWindow } from "@/shared/api/transport";
import { ChevronIcon, IssueIcon, PullRequestIcon, WorkflowIcon } from "@/shared/ui/icons";

// Row styles shared by the overview lists (prototype D2 `.lrow`, `.mini`).
export const listRowClass = "flex w-full items-center gap-3 border-b border-border px-3 py-2.5 text-left last:border-b-0 hover:bg-muted";
export const emptyListClass = "p-4 text-muted-foreground";
export const pillClass = "inline-flex shrink-0 items-center rounded-full border border-border px-[7px] text-[11px] text-soft";

const shortDate = new Intl.DateTimeFormat("en", { month: "short", day: "numeric", timeZone: "UTC" });
const kinds: Record<AttentionKind, { label: (n: number) => string; reason: (since: string) => string; pill: string; tab: "pulls" | "overview" | "cicd" }> = {
  pr_awaiting_review: { label: n => `PR #${n}`, reason: () => "Awaiting first review", pill: "Review", tab: "pulls" },
  issue_no_response: { label: n => `Issue #${n}`, reason: since => `No response since ${shortDate.format(new Date(since))}`, pill: "Issues", tab: "overview" },
  stale_issue: { label: n => `Issue #${n}`, reason: () => "Open, 30+ days quiet", pill: "Issues", tab: "overview" },
  failed_workflow_run: { label: n => `Run #${n}`, reason: since => `Workflow failed ${shortDate.format(new Date(since))}`, pill: "CI/CD", tab: "cicd" },
};

// Body of the "Needs attention" panel: one row per concrete problem, each opening its repository workspace in the same window.
export function NeedsAttentionList({ items, days }: { items: AttentionItem[]; days: MetricWindow }) {
  if (!items.length) return <p className={emptyListClass}>Nothing needs attention right now.</p>;
  return items.map(item => {
    const kind = kinds[item.kind];
    const name = item.fullName.split("/")[1] ?? item.fullName;
    const icon = item.kind === "pr_awaiting_review" ? <PullRequestIcon /> : item.kind === "failed_workflow_run" ? <WorkflowIcon /> : <IssueIcon />;
    return <Link
      key={`${item.kind}:${item.repositoryId}:${item.runId ?? item.number}`}
      to="/workspace/$repositoryId"
      params={{ repositoryId: item.repositoryId }}
      search={{ days, tab: kind.tab, ...(item.runId ? { run: item.runId } : {}) }}
      className={listRowClass}
    >
      <span className="grid size-7 shrink-0 place-items-center rounded-lg bg-warning-soft text-warning">{icon}</span>
      <span className="min-w-0 flex-1">
        <span className="block font-medium">{name} · {kind.label(item.number)}</span>
        <span className="block truncate text-xs text-muted-foreground">{kind.reason(item.since)} · {item.title}</span>
      </span>
      <span className={pillClass}>{kind.pill}</span>
      <ChevronIcon />
    </Link>;
  });
}

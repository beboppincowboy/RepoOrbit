import { Link } from "@tanstack/react-router";
import { emptyListClass, listRowClass, pillClass } from "@/components/needs-attention";
import { ChevronIcon } from "@/shared/ui/icons";
import { STATUS_NAMES } from "@/components/task-drawer";
import { BOARD_STATUSES } from "@/lib/board";
import type { AssignedIssue } from "@/features/analytics/api";
import type { MetricWindow } from "@/shared/api/transport";

// Body of the "Assigned to me" panel: open issues assigned to the viewer, grouped by Board status (prototype D2 `.grp`). Each row opens its Board with the task drawer.
export function AssignedToMeList({ issues, days }: { issues: AssignedIssue[]; days: MetricWindow }) {
  if (!issues.length) return <p className={emptyListClass}>No open issues are assigned to you.</p>;
  return [...BOARD_STATUSES, null].map(status => {
    const rows = issues.filter(issue => issue.status === status);
    if (!rows.length) return null;
    const name = status ? STATUS_NAMES[status] : "No status source";
    const id = `assigned-${status ?? "none"}`;
    return <div key={id} role="group" aria-labelledby={id}>
      <h3 id={id} className="m-0 flex items-center justify-between border-b border-border bg-muted px-3 py-1.5 text-xs font-semibold text-muted-foreground">{name} <span className={pillClass}>{rows.length}</span></h3>
      {rows.map(issue => <Link key={`${issue.repositoryId}:${issue.number}`} to="/workspace/$repositoryId" params={{ repositoryId: issue.repositoryId }} search={{ days, tab: "board", issue: issue.number }} className={listRowClass}>
        <span className="min-w-0 flex-1">
          <span className="block truncate font-medium">{issue.title}</span>
          <span className="text-xs text-muted-foreground">{issue.fullName.split("/")[1] ?? issue.fullName} · #{issue.number}</span>
        </span>
        <ChevronIcon />
      </Link>)}
    </div>;
  });
}

import { Link } from "@tanstack/react-router";
import { ChevronIcon, emptyListClass, listRowClass } from "@/components/needs-attention";
import type { AssignedIssue, MetricWindow } from "@/lib/api";

// Body of the "Assigned to me" panel: a flat list of open issues assigned to the viewer (U6d groups it by status).
export function AssignedToMeList({ issues, days }: { issues: AssignedIssue[]; days: MetricWindow }) {
  if (!issues.length) return <p className={emptyListClass}>No open issues are assigned to you.</p>;
  return issues.map(issue => <Link key={`${issue.repositoryId}:${issue.number}`} to="/workspace/$repositoryId" params={{ repositoryId: issue.repositoryId }} search={{ days, tab: "overview" }} className={listRowClass}>
    <span className="min-w-0 flex-1">
      <span className="block truncate font-medium">{issue.title}</span>
      <span className="text-xs text-muted-foreground">{issue.fullName.split("/")[1] ?? issue.fullName} · #{issue.number}</span>
    </span>
    <ChevronIcon />
  </Link>);
}

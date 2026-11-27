import type { CrossRepositoryOverview } from "@/features/analytics/api";
import { newerRejection, type ViewerVisibility } from "@/features/repositories/access-store";

// Withholds what a repository rejected after the aggregate read started may have contributed: its rows, and every total.
// Returns the same object when no repository was rejected since then.
export function filterAggregateByVisibility(
  aggregate: CrossRepositoryOverview,
  aggregateStartedAtSeq: number,
  visibility: ViewerVisibility,
): CrossRepositoryOverview {
  const supersededIds = new Set<string>();
  for (const [repoId, outcome] of visibility.entries()) {
    if (newerRejection(outcome, aggregateStartedAtSeq)) {
      supersededIds.add(repoId);
    }
  }
  if (supersededIds.size === 0) {
    return aggregate;
  }
  const withheldCoverage = { covered: false, reason: "access_changed", through: null };
  return {
    ...aggregate,
    openIssues: { value: null, coverage: withheldCoverage },
    reviewBacklog: { value: null, coverage: withheldCoverage },
    needsAttention: aggregate.needsAttention.filter(item => !supersededIds.has(item.repositoryId)),
    assignedToMe: aggregate.assignedToMe.filter(item => !supersededIds.has(item.repositoryId)),
    daily: aggregate.daily.map(point => ({ day: point.day, issuesOpened: null, pullRequestsMerged: null })),
  };
}

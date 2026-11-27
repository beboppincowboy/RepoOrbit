import type { BoardStatus, DeploymentSummary } from "@/features/issues/api";
import { activityFields, type ActivityStatus, type SyncState } from "@/features/repositories/api";
import { RepositoryAccessUnverifiedError } from "@/shared/api/errors";
import { coverageFields, graphqlQuery, guardedGraphql, type Coverage, type MetricWindow, type ReviewState } from "@/shared/api/transport";

export type CountMetric = { value: number | null; coverage: Coverage };
export type MedianMetric = { seconds: number | null; sampleSize: number | null; coverage: Coverage };
export type RateMetric = { rate: number | null; numerator: number | null; denominator: number | null; coverage: Coverage };

export type RepositoryOverview = {
  fullName: string;
  syncState: SyncState;
  activity: ActivityStatus & { archived: boolean };
  openIssues: CountMetric;
  staleOpenIssues: CountMetric;
  pullRequestsMerged: CountMetric;
  openPullRequests: CountMetric;
  firstResponseTime: MedianMetric;
  issuesWithoutResponse: Pick<CountMetric, "value">;
  mergeTime: MedianMetric;
  eventsCoveredFrom: string | null;
  windowStart: string;
  activeBotContributors: CountMetric;
  contributorsCoverage: Coverage;
  // Up to 20 humans, then up to 5 bots (isBot); empty unless contributorsCoverage is covered.
  contributors: { login: string; isBot: boolean; pullRequestsOpened: number; reviewsSubmitted: number; commentsCreated: number }[];
  timeline: { url: string; sourceId: number; kind: string; occurredAt: string; title: string; number: number | null; actor: { login: string; isBot: boolean } | null }[];
  daily: { day: string; issuesOpened: number | null; pullRequestsMerged: number | null }[];
};

const countFields = `value coverage { ${coverageFields} }`;
const medianFields = `seconds sampleSize coverage { ${coverageFields} }`;
const rateFields = `rate numerator denominator coverage { ${coverageFields} }`;

// Fetches stored repository analytics after server-side access verification. Null means the repo is missing,
// unselected, or denied. Unverified access and temporary transport failures reject with their typed errors.
export async function fetchRepositoryOverview(id: string, days: MetricWindow, signal: AbortSignal): Promise<RepositoryOverview | null> {
  const data = await guardedGraphql<{
    repositoryOverview: RepositoryOverview | null;
  }>(`query RepositoryOverview($id: ID!, $days: Int!) {
    repositoryOverview(id: $id, days: $days) {
      fullName syncState activity { ${activityFields} archived }
      openIssues { ${countFields} } staleOpenIssues { ${countFields} } pullRequestsMerged { ${countFields} } openPullRequests { ${countFields} }
      firstResponseTime { ${medianFields} } mergeTime { ${medianFields} } issuesWithoutResponse { value }
      activeBotContributors { ${countFields} } contributorsCoverage { ${coverageFields} }
      contributors { login isBot pullRequestsOpened reviewsSubmitted commentsCreated }
      windowStart eventsCoveredFrom
      daily { day issuesOpened pullRequestsMerged }
      timeline { sourceId kind occurredAt title number url actor { login isBot } }
    }
  }`, { id, days }, "Could not load repository analytics", signal);
  return data.repositoryOverview;
}

export type WorkspacePullRequest = { id: string; number: number; title: string; state: string; url: string; createdAt: string; mergedAt: string | null; reviewState: ReviewState | null; author: { login: string; type: string } | null };

// Fetches imported PRs in descending source ID order. Start with null, then pass the previous endCursor.
// A withheld page rejects with RepositoryAccessUnverifiedError. Titles are raw GitHub text.
export async function fetchWorkspacePullRequests(id: string, after: string | null, signal: AbortSignal) {
  const { data, errors } = await graphqlQuery<{
    pullRequestEvidence: { nodes: WorkspacePullRequest[]; withheld: number; retryAt: string | null; hasNextPage: boolean; endCursor: string | null };
  }>(`query WorkspacePullRequests($id: ID!, $after: ID) {
    pullRequestEvidence(repositoryIds: [$id], first: 20, after: $after, newestFirst: true) { nodes { id number title state url createdAt mergedAt reviewState author { login type } } withheld retryAt hasNextPage endCursor }
  }`, { id, after }, signal);
  if (!data || errors?.length) throw new Error("Could not load pull requests");
  if (data.pullRequestEvidence.withheld) throw new RepositoryAccessUnverifiedError(data.pullRequestEvidence.retryAt ?? null);
  return data.pullRequestEvidence;
}

export type AttentionKind = "pr_awaiting_review" | "issue_no_response" | "stale_issue" | "failed_workflow_run";
// since: when the item was opened, or for stale_issue / failed_workflow_run when it was last updated (RFC 3339).
export type AttentionItem = { kind: AttentionKind; repositoryId: string; fullName: string; number: number; title: string; url: string; since: string; runId: string | null };
// status is null when the repository's status labels are off.
export type AssignedIssue = { repositoryId: string; fullName: string; number: number; title: string; url: string; updatedAt: string; status: BoardStatus | null };
// Totals, the trend and Needs attention cover counted repositories. Assigned issues require issue coverage.
// Withheld counts unverified repositories without naming them. Lists hold at most 25 rows.
export type CrossRepositoryOverview = {
  days: MetricWindow;
  windowStart: string;
  now: string;
  repositoriesCounted: number;
  repositoriesSyncing: number;
  withheld: number;
  retryAt: string | null;
  openIssues: CountMetric;
  reviewBacklog: CountMetric;
  failingWorkflows: CountMetric;
  buildSuccessRate: RateMetric;
  needsAttention: AttentionItem[];
  assignedToMe: AssignedIssue[];
  daily: { day: string; issuesOpened: number | null; pullRequestsMerged: number | null }[];
};

// Fetches aggregate analytics for the viewer’s selected repositories. Unverified access rejects with
// RepositoryAccessUnverifiedError. A selection-limit error reports the server’s limit to the caller.
export async function fetchCrossRepositoryOverview(days: MetricWindow, signal: AbortSignal) {
  const { data, errors } = await graphqlQuery<{ crossRepositoryOverview: CrossRepositoryOverview | null }>(`query CrossRepositoryOverview($days: Int! = 30) {
    crossRepositoryOverview(days: $days) {
      days windowStart now repositoriesCounted repositoriesSyncing withheld retryAt
      openIssues { value coverage { ${coverageFields} } }
      reviewBacklog { value coverage { ${coverageFields} } }
      failingWorkflows { value coverage { ${coverageFields} } }
      buildSuccessRate { rate numerator denominator coverage { ${coverageFields} } }
      needsAttention { kind repositoryId fullName number title url since runId }
      assignedToMe { repositoryId fullName number title url updatedAt status }
      daily { day issuesOpened pullRequestsMerged }
    }
  }`, { days }, signal);
  const accessError = errors?.find(error => error.extensions?.code === "ACCESS_UNVERIFIED");
  if (accessError) throw new RepositoryAccessUnverifiedError(accessError.extensions?.retryAt ?? null);
  const selectionLimit = errors?.find(error => error.extensions?.code === "OVERVIEW_SELECTION_LIMIT");
  if (selectionLimit?.message) throw new Error(selectionLimit.message);
  if (!data?.crossRepositoryOverview || errors?.length) throw new Error("Could not load the overview. Check that the local services are running.");
  return data.crossRepositoryOverview;
}

export type ActionsDailyPoint = { day: string; succeeded: number | null; failed: number | null; other: number | null };
export type WorkflowFailureSummary = {
  workflowId: string;
  workflowName: string;
  failedRuns: number;
  totalRuns: number;
  latestRunId: string;
  latestStatus: string;
  latestConclusion: string;
  latestAt: string;
  htmlUrl: string;
};
export type WorkflowRunSummary = {
  id: string;
  workflowId: string;
  workflowName: string;
  runNumber: number;
  event: string;
  status: string;
  conclusion: string;
  headBranch: string;
  headSha: string;
  htmlUrl: string;
  createdAt: string;
  runStartedAt: string | null;
  updatedAt: string;
  durationSeconds: number | null;
  queueSeconds: number | null;
};
export type WorkflowJobDetail = {
  id: string;
  runId: string;
  name: string;
  status: string;
  conclusion: string;
  htmlUrl: string;
  createdAt: string;
  startedAt: string | null;
  completedAt: string | null;
  durationSeconds: number | null;
  queueSeconds: number | null;
};
export type WorkflowRunDetail = { run: WorkflowRunSummary; jobs: WorkflowJobDetail[] };
export type RepositoryActions = {
  days: MetricWindow;
  windowStart: string;
  now: string;
  actionsCoverage: Coverage;
  deploymentsCoverage: Coverage;
  buildSuccessRate: RateMetric;
  failedJobs: CountMetric;
  runningJobs: CountMetric;
  durationP50: MedianMetric;
  durationP95: MedianMetric;
  queueWaitP50: MedianMetric;
  daily: ActionsDailyPoint[];
  failuresByWorkflow: WorkflowFailureSummary[];
  recentRuns: WorkflowRunSummary[];
  deployments: DeploymentSummary[];
  deploymentUnavailable: boolean;
};

export async function fetchRepositoryActions(id: string, days: MetricWindow, signal: AbortSignal): Promise<RepositoryActions | null> {
  const data = await guardedGraphql<{ repositoryActions: RepositoryActions | null }>(`query RepositoryActions($id: ID!, $days: Int! = 30) {
    repositoryActions(id: $id, days: $days) {
      days windowStart now deploymentUnavailable
      actionsCoverage { ${coverageFields} }
      deploymentsCoverage { ${coverageFields} }
      buildSuccessRate { ${rateFields} }
      failedJobs { ${countFields} }
      runningJobs { ${countFields} }
      durationP50 { ${medianFields} }
      durationP95 { ${medianFields} }
      queueWaitP50 { ${medianFields} }
      daily { day succeeded failed other }
      failuresByWorkflow { workflowId workflowName failedRuns totalRuns latestRunId latestStatus latestConclusion latestAt htmlUrl }
      recentRuns { id workflowId workflowName runNumber event status conclusion headBranch headSha htmlUrl createdAt runStartedAt updatedAt durationSeconds queueSeconds }
      deployments { id environment sha ref state description htmlUrl createdAt updatedAt }
    }
  }`, { id, days }, "Could not load CI/CD metrics", signal);
  return data.repositoryActions;
}

export async function fetchWorkflowRun(repositoryId: string, runId: string, signal: AbortSignal): Promise<WorkflowRunDetail | null> {
  const data = await guardedGraphql<{ workflowRun: WorkflowRunDetail | null }>(`query WorkflowRun($id: ID!, $runId: ID!) {
    workflowRun(repositoryId: $id, runId: $runId) {
      run { id workflowId workflowName runNumber event status conclusion headBranch headSha htmlUrl createdAt runStartedAt updatedAt durationSeconds queueSeconds }
      jobs { id runId name status conclusion htmlUrl createdAt startedAt completedAt durationSeconds queueSeconds }
    }
  }`, { id: repositoryId, runId }, "Could not load workflow run details", signal);
  return data.workflowRun;
}

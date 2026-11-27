import { clearAllIssueDrafts } from "@/lib/drafts";

export type Issue = { id: string; repositoryId: string; title: string; state: string };
const ACCESS_UNVERIFIED_MESSAGE = "Access could not be verified";
// Wire values of the backend's repositories.SyncState; "demo" appears only in Demo mode.
export type SyncState = "demo" | "awaiting-sync" | "not-synchronized";
// Shared metadata refresh status (null when not selected, and in Demo mode). Times are RFC 3339; failureCode is a safe machine code.
// "paused" means waiting for a usable GitHub credential; "dead" needs an operator requeue.
export type MetadataStatus = { state: "not-scheduled" | "queued" | "running" | "paused" | "dead"; lastSuccessAt: string | null; nextAttemptAt: string | null; paused: boolean; failureCode: string | null; followSuspended: boolean };
export type ActivityResourceStatus = { phase: string; boundary: string; catchupThrough: string | null; lastSuccessAt: string | null; failureCode: string | null; jobState: string; nextAttemptAt: string | null };
export type ChildResourceStatus = { complete: boolean; pendingChildren: number; lastSuccessAt: string | null; failureCode: string | null; nextAttemptAt: string | null };
export type ActivityStatus = { synchronized: boolean; message: string; issues: ActivityResourceStatus | null; pullRequests: ActivityResourceStatus | null; releases: ActivityResourceStatus | null; reviews: ChildResourceStatus | null; comments: ChildResourceStatus | null; issueEvents: { traversal: ActivityResourceStatus | null; historicalGap: boolean } | null; futureResources: string[] };

// fullName is "owner/name".
export type SelectedRepository = { id: string; fullName: string; description: string; syncState: SyncState; metadataStatus: MetadataStatus | null };
export type IssueConnection = { nodes: Issue[]; hasNextPage: boolean; withheld: number; retryAt: string | null };
export type RepositoryDetail =
  | { state: "available"; activity: ActivityStatus | null; issues: IssueConnection }
  | { state: "access-denied" }
  | { state: "access-unverified"; retryAt: string | null };
// withheld counts selected repositories hidden because access could not be verified; retryAt is set when GitHub rate limited the check.
export type RepositoryConnection = { nodes: SelectedRepository[]; endCursor: string | null; hasNextPage: boolean; withheld: number; retryAt: string | null };
// One row of a REST repository list. `selected` means connected on the owned list and followed on the starred list.
export type ListedRepo = { id: string; fullName: string; description: string; private: boolean; selected: boolean; syncState: SyncState };
export type OwnedPage = { repositories: ListedRepo[]; nextPage: number; refreshFailed: boolean; withheld: number; retryAt: string | null; installUrl: string };
export type StarredPage = { repositories: ListedRepo[]; nextPage: number; refreshedAt: string | null; refreshFailed: boolean; withheld: number; retryAt: string | null; installUrl: string };
export type ViewerSession = { mode: "demo" | "real"; user: { login: string; demo: boolean } | null; csrfToken: string | null };
let csrfToken: string | null = null;
async function fetchViewerSession(): Promise<ViewerSession> {
  const response = await fetch("/auth/me", { cache: "no-store" });
  if (!response.ok) throw new Error("Unable to load session");
  const result: ViewerSession = await response.json();
  csrfToken = result.csrfToken;
  return result;
}
// Fetches the current session on workspace entry and supplies the CSRF token for subsequent requests.
export const sessionQuery = { queryKey: ["session"], queryFn: fetchViewerSession, staleTime: 0 } as const;
// Ends the current session in Go. The caller owns navigation and local query state. Rejects on HTTP or network failure.
export async function logout() {
  const response = await fetch("/auth/logout", { method: "POST", headers: { "X-CSRF-Token": csrfToken ?? "" } });
  if (!response.ok) throw new Error("Could not sign out. Try again.");
  clearAllIssueDrafts();
}
type GraphQLResponse<Data> = {
  data?: Data | null;
  errors?: { message?: string; path?: (string | number)[]; extensions?: { code?: string; retryAt?: string } }[];
};

// A network failure or temporary HTTP gateway error. The message explains why a saved view may be shown.
export class GraphQLUnavailableError extends Error {}

async function graphqlQuery<Data>(queryText: string, variables: Record<string, unknown>, signal?: AbortSignal): Promise<GraphQLResponse<Data>> {
  const response = await fetch("/graphql", { method: "POST", headers: { "Content-Type": "application/json", "X-CSRF-Token": csrfToken ?? "" }, cache: "no-store", signal, body: JSON.stringify({ query: queryText, variables }) }).catch((error: unknown) => {
    if (signal?.aborted || !(error instanceof TypeError)) throw error;
    throw new GraphQLUnavailableError("RepoOrbit could not reach the server.");
  });
  if ([502, 503, 504].includes(response.status)) throw new GraphQLUnavailableError("The server is temporarily unavailable.");
  if (!response.ok) throw new Error("Unable to load repositories");
  return response.json();
}
const activityResourceFields = "phase boundary catchupThrough lastSuccessAt failureCode jobState nextAttemptAt";
const childResourceFields = "complete pendingChildren lastSuccessAt failureCode nextAttemptAt";
const activityFields = `synchronized message issues { ${activityResourceFields} } pullRequests { ${activityResourceFields} } releases { ${activityResourceFields} } reviews { ${childResourceFields} } comments { ${childResourceFields} } issueEvents { traversal { ${activityResourceFields} } historicalGap } futureResources`;
const detailChunk = 8;
type DetailResponse = Partial<Record<`a${number}`, { activity: ActivityStatus | null } | null>>
  & Partial<Record<`r${number}`, IssueConnection | null>>;

export async function fetchRepositoryDetails(ids: string[], signal?: AbortSignal): Promise<RepositoryDetail[]> {
  if (ids.length === 0 || ids.length > detailChunk) throw new RangeError("Expected one to eight repository IDs");
  const declarations = ids.map((_, i) => `$id${i}: ID!`).join(", ");
  const fields = ids.map((_, i) => `a${i}: repository(id: $id${i}) { activity { ${activityFields} } } r${i}: issues(repositoryIds: [$id${i}], first: 5) { nodes { id repositoryId title state } hasNextPage withheld retryAt }`).join(" ");
  const { data, errors = [] } = await graphqlQuery<DetailResponse>(`query RepositoryDetails(${declarations}) { ${fields} }`, Object.fromEntries(ids.map((id, i) => [`id${i}`, id])), signal);
  if (!data || errors.some(error => error.extensions?.code !== "ACCESS_UNVERIFIED" || !error.path?.length)) {
    throw new Error("Unable to load repository details");
  }
  return ids.map((_, i) => {
    const repoAlias = `a${i}` as const;
    const issuesAlias = `r${i}` as const;
    const hasRepoAlias = Object.hasOwn(data, repoAlias);
    const hasIssuesAlias = Object.hasOwn(data, issuesAlias);
    const repository = data[repoAlias];
    const issues = data[issuesAlias];
    const repoError = errors.find(error => error.path?.[0] === repoAlias);
    const accessError = repoError ?? errors.find(error => error.path?.[0] === issuesAlias);
    if (hasRepoAlias && repository === null && !repoError) {
      return { state: "access-denied" };
    }
    if (accessError || issues?.withheld) {
      return { state: "access-unverified", retryAt: accessError?.extensions?.retryAt ?? issues?.retryAt ?? null };
    }
    if (!hasRepoAlias || !hasIssuesAlias || !repository || !issues || typeof repository !== "object" || typeof issues !== "object") {
      throw new Error("Unable to load repository details");
    }
    return { state: "available", activity: repository.activity, issues };
  });
}

export type SelectedRepositoryBatch = { repositories: SelectedRepository[]; details: RepositoryDetail[] | null };
export type SelectedRepositoryPage = Omit<RepositoryConnection, "nodes"> & { batches: SelectedRepositoryBatch[] };

// Fetches up to 20 verified selections from GraphQL. Start with null, then pass the previous endCursor.
// Withheld rows count toward the page size and appear only in the withheld count. An empty page can still have a next cursor.
// Rejects on request failure or cancellation.
export async function fetchSelectedRepositoryMetadataPage(after: string | null, signal?: AbortSignal): Promise<RepositoryConnection> {
  const { data, errors } = await graphqlQuery<{ selectedRepositories: RepositoryConnection }>(`query SelectedRepositories($after: ID) { selectedRepositories(first: 20, after: $after) { nodes { id fullName description syncState metadataStatus { state lastSuccessAt nextAttemptAt paused failureCode followSuspended } } endCursor hasNextPage withheld retryAt } }`, { after }, signal);
  if (!data || errors?.length) throw new Error("Unable to load repositories");
  return data.selectedRepositories;
}

// Enriches one metadata-page promise with ordered detail batches, issuing at most two detail requests at a time.
// Metadata failure and cancellation reject the promise. A failed batch keeps its repositories with null details.
// Explicit access failures appear as access-unverified details. The selected list and protected details are authorized separately.
export async function fetchSelectedRepositoryDetailsPage(metadataPage: Promise<RepositoryConnection>, signal?: AbortSignal): Promise<SelectedRepositoryPage> {
  const { nodes, ...connection } = await metadataPage;
  const batches: SelectedRepositoryBatch[] = [];
  for (let i = 0; i < nodes.length; i += detailChunk) {
    batches.push({ repositories: nodes.slice(i, i + detailChunk), details: null });
  }
  for (let i = 0; i < batches.length; i += 2) {
    signal?.throwIfAborted();
    const pair = batches.slice(i, i + 2);
    const results = await Promise.allSettled(pair.map(batch => fetchRepositoryDetails(batch.repositories.map(repo => repo.id), signal)));
    results.forEach((result, index) => {
      if (result.status === "fulfilled") pair[index].details = result.value;
    });
  }
  signal?.throwIfAborted();
  return { ...connection, batches };
}
export async function ownedRepositories(page = 1): Promise<OwnedPage> {
  const response = await fetch(`/api/repositories/owned?page=${page}`, { cache: "no-store" });
  if (!response.ok) throw new Error("Unable to load your repositories");
  return response.json();
}
// Connects (selected=true) or disconnects an owned repository. Throws a user-facing Error on failure.
export async function setSelected(id: string, selected: boolean) {
  const response = await fetch(`/api/repositories/${encodeURIComponent(id)}/selection`, { method: selected ? "PUT" : "DELETE", headers: { "X-CSRF-Token": csrfToken ?? "" } });
  if (response.status === 503) throw new Error("GitHub could not verify access right now. Try again shortly.");
  if (!response.ok) throw new Error(selected ? "Could not connect this repository." : "Could not disconnect this repository.");
}
// Loads one page of the stored star snapshot; refresh=true first re-reads the stars from GitHub.
export async function starredRepositories(page = 1, refresh = false): Promise<StarredPage> {
  const response = await fetch(`/api/repositories/starred?page=${page}${refresh ? "&refresh=1" : ""}`, { cache: "no-store" });
  if (!response.ok) throw new Error("Unable to load your starred repositories");
  return response.json();
}
// Follows (followed=true) or unfollows a starred repository. Throws a user-facing Error on failure.
export async function setFollowed(id: string, followed: boolean) {
  const base = `/api/repositories/${encodeURIComponent(id)}`;
  const response = await fetch(followed ? `${base}/follow` : `${base}/selection`, { method: followed ? "PUT" : "DELETE", headers: { "X-CSRF-Token": csrfToken ?? "" } });
  if (response.status === 503) throw new Error("GitHub could not verify access right now. Try again shortly.");
  if (!response.ok) throw new Error(followed ? "Could not follow this repository. Refresh your stars and try again." : "Could not unfollow this repository.");
}
// Requests an activity refresh. Returns "accepted" or "coalesced" (202); throws a user-facing Error otherwise.
export async function refreshActivity(id: string): Promise<"accepted" | "coalesced"> {
  const response = await fetch(`/api/repositories/${encodeURIComponent(id)}/activity/refresh`, { method: "POST", headers: { "X-CSRF-Token": csrfToken ?? "" } });
  if (response.status === 202) return (await response.json().catch(() => null))?.state === "coalesced" ? "coalesced" : "accepted";
  if (response.status === 409) throw new Error("Activity sync stopped after repeated failures; an operator must requeue it.");
  if (response.status === 404) throw new Error("This repository is no longer eligible for activity sync.");
  if (response.status === 503) throw new Error("GitHub could not verify access right now. Try again shortly.");
  throw new Error("Could not request an activity refresh.");
}

export type MetricWindow = 7 | 30 | 90;
export type Coverage = { covered: boolean; reason: string | null; through: string | null };
export type CountMetric = { value: number | null; coverage: Coverage };
export type MedianMetric = { seconds: number | null; sampleSize: number | null; coverage: Coverage };
export type RateMetric = { rate: number | null; numerator: number | null; denominator: number | null; coverage: Coverage };

type RepositoryOverview = {
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

// GitHub access is unverified. retryAt is a UTC timestamp, or null when the server supplies no retry time.
export class RepositoryAccessUnverifiedError extends Error {
  constructor(readonly retryAt: string | null) {
    super(ACCESS_UNVERIFIED_MESSAGE);
  }
}

const coverageFields = "covered reason through";
const countFields = `value coverage { ${coverageFields} }`;
const medianFields = `seconds sampleSize coverage { ${coverageFields} }`;
const rateFields = `rate numerator denominator coverage { ${coverageFields} }`;

async function guardedGraphql<T>(query: string, variables: Record<string, unknown>, fallbackError: string, signal?: AbortSignal): Promise<T> {
  const { data, errors } = await graphqlQuery<T>(query, variables, signal);
  const accessError = errors?.find(error => error.extensions?.code === "ACCESS_UNVERIFIED");
  if (accessError) throw new RepositoryAccessUnverifiedError(accessError.extensions?.retryAt ?? null);
  if (!data || errors?.length) throw new Error(fallbackError);
  return data;
}

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

// "merged" | "closed", or for an open PR the human reviewers' latest decision; null while its review history is not imported.
export type ReviewState = "awaiting_review" | "commented" | "approved" | "changes_requested" | "merged" | "closed";
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
export type DeploymentSummary = {
  id: string;
  environment: string;
  sha: string;
  ref: string;
  state: string;
  description: string;
  htmlUrl: string;
  createdAt: string;
  updatedAt: string;
};
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
export type DeliveryCheck = {
  runId: string;
  workflowName: string;
  status: string;
  conclusion: string;
  htmlUrl: string;
  updatedAt: string;
  failingJobName: string | null;
  failingJobUrl: string | null;
};
export type LinkedPullRequestDelivery = {
  number: number;
  title: string;
  state: string;
  url: string;
  reviewState: ReviewState | null;
  headSha: string;
  checks: DeliveryCheck[];
  deployments: DeploymentSummary[];
};
export type IssueDelivery = {
  issueNumber: number;
  actionsCoverage: Coverage;
  deploymentsCoverage: Coverage;
  deploymentUnavailable: boolean;
  pullRequests: LinkedPullRequestDelivery[];
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

export async function fetchIssueDelivery(repositoryId: string, issueNumber: number, signal: AbortSignal): Promise<IssueDelivery | null> {
  const data = await guardedGraphql<{ issueDelivery: IssueDelivery | null }>(`query IssueDelivery($id: ID!, $issueNumber: Int!) {
    issueDelivery(repositoryId: $id, issueNumber: $issueNumber) {
      issueNumber deploymentUnavailable
      actionsCoverage { ${coverageFields} }
      deploymentsCoverage { ${coverageFields} }
      pullRequests {
        number title state url reviewState headSha
        checks { runId workflowName status conclusion htmlUrl updatedAt failingJobName failingJobUrl }
        deployments { id environment sha ref state description htmlUrl createdAt updatedAt }
      }
    }
  }`, { id: repositoryId, issueNumber }, "Could not load delivery lifecycle", signal);
  return data.issueDelivery;
}

export type BoardStatus = "backlog" | "todo" | "in_progress" | "in_review" | "done";
// status is null when the repository's status labels are off. multipleStatus flags an open issue carrying more than one
// status label. updatedAt is RFC 3339.
export type BoardIssue = { number: number; title: string; url: string; state: "open" | "closed"; updatedAt: string; authorLogin: string | null; labels: string[]; assignees: string[]; status: BoardStatus | null; multipleStatus: boolean };
// The open issues (500 most recently updated), closed issues (50 most recently closed) and the included numbers, each once.
// truncated is true when a cap left issues out.
export type RepositoryBoard = { statusLabels: boolean; truncated: boolean; issues: BoardIssue[] };

// Fetches the Board snapshot, adding the issues numbered in include (at most 100, for staged cards and an opened issue).
// Null means the repo is missing, unselected, or denied. Unverified access and temporary transport failures reject
// with their typed errors.
export async function fetchRepositoryBoard(id: string, include: number[], signal?: AbortSignal): Promise<RepositoryBoard | null> {
  const data = await guardedGraphql<{ repositoryBoard: RepositoryBoard | null }>(`query RepositoryBoard($id: ID!, $include: [Int!]) {
    repositoryBoard(id: $id, include: $include) {
      statusLabels truncated
      issues { number title url state updatedAt authorLogin labels assignees status multipleStatus }
    }
  }`, { id, include }, "Could not load the board", signal);
  return data.repositoryBoard;
}


export type WorkspaceIssue = {
  id: string;
  repositoryId: string;
  number: number;
  title: string;
  state: string;
  url: string;
  authorLogin: string | null;
  updatedAt: string;
  labels: string[];
  assignees: string[];
};

export type IssueCapabilities = {
  canCreateIssue: boolean;
  canCreateIssueWithMetadata: boolean;
  canManageIssues: boolean;
};

export type IssueCreateDraft = {
  title: string;
  body?: string;
  labels?: string[];
  assignees?: string[];
};

export type IssueDelta = {
  add?: string[];
  remove?: string[];
};

// An issue's Board-relevant state: a staged card's snapshot, and the `current` of a 409 "changed on GitHub".
export type BoardSnapshot = { state: "open" | "closed"; labels: string[]; assignees: string[]; status: BoardStatus; updatedAt: string };

export type IssueUpdateDraft = {
  labels?: IssueDelta;
  assignees?: IssueDelta;
  state?: "open" | "closed";
  // Board changes only: the state the change was staged over. When GitHub's Board status, assignee set or (while open)
  // status-label set differs, the update answers 409 changed_on_github and nothing is recorded or sent.
  expect?: { status: BoardStatus; assignees: string[]; statusLabels: string[] };
};

export type WriteOperationIssue = {
  number: number;
  url: string;
  title?: string;
  state: string;
  labels?: string[];
  assignees?: string[];
};

export type WriteOperationOutcome = {
  issue?: WriteOperationIssue;
  unapplied?: string[];
  created?: string[]; // status_labels only: the labels the operation created (the others already existed)
  unappliedCount?: number;
  error?: string;
  allStepsAnswered?: boolean;
  redacted?: boolean;
};

export type WriteOperation = {
  id: number;
  repositoryId: number;
  // "cleanup" is the GitHub App removing a status label from a closed issue (actor ""); its id is from a separate sequence.
  kind: "create" | "update" | "status_labels" | "cleanup";
  state: "pending" | "unknown" | "succeeded" | "failed";
  issueNumber: number | null;
  draft: IssueCreateDraft | IssueUpdateDraft | StatusLabelsDraft | CleanupDraft | null;
  outcome: WriteOperationOutcome | null;
  actor: string;
  searchUrl?: string;
  createdAt: string;
  updatedAt: string;
};

export type StatusLabelsDraft = { labels: string[] };
export type CleanupDraft = { label: string };

// A repository's status source: whether status labels are on, whether the viewer (Admin or Maintain) may change that,
// and, only for such a viewer while they are off, which of the three labels already exist on GitHub.
export type StatusLabelState = { enabled: boolean; canChange: boolean; labels?: { name: string; exists: boolean }[] };

export type IssueWriteErrorKind =
  | "validation"
  | "unauthenticated"
  | "forbidden"
  | "not_found"
  | "duplicate_create"
  | "recheck_unresolved"
  | "unprocessable"
  | "unverified"
  | "transport"
  | "server"
  | "changed_on_github"; // a Board update's expect no longer matches GitHub; current holds the live state

export type WriteDisposition = "not-recorded" | "possibly-recorded" | null;

export class IssueWriteError extends Error {
  readonly status: number;
  readonly kind: IssueWriteErrorKind;
  readonly disposition: WriteDisposition;
  readonly retryAfter: string | null;
  readonly operationId: number | null;
  readonly operation: WriteOperation | null;
  readonly current: BoardSnapshot | null;

  constructor(
    message: string,
    opts: {
      status: number;
      kind: IssueWriteErrorKind;
      disposition?: WriteDisposition;
      retryAfter?: string | null;
      operationId?: number | null;
      operation?: WriteOperation | null;
      current?: BoardSnapshot | null;
    },
  ) {
    super(message);
    this.status = opts.status;
    this.kind = opts.kind;
    this.disposition = opts.disposition ?? null;
    this.retryAfter = opts.retryAfter ?? null;
    this.operationId = opts.operationId ?? null;
    this.operation = opts.operation ?? null;
    this.current = opts.current ?? null;
  }
}

const SERVER_WRITE_RETENTION_DAYS = 30;
const CLOCK_SKEW_MARGIN_DAYS = 1;
const WRITE_RECOVERY_WINDOW_MS = (SERVER_WRITE_RETENTION_DAYS - CLOCK_SKEW_MARGIN_DAYS) * 24 * 60 * 60 * 1000;
const WRITE_RECOVERY_FUTURE_SKEW_MS = 5 * 60 * 1000;

export function generateIdempotencyKey(): string {
  if (typeof crypto !== "undefined" && typeof crypto.randomUUID === "function") {
    return `ro-${crypto.randomUUID()}`;
  }
  return `ro-${Date.now()}-${Math.random().toString(36).slice(2, 12)}`;
}

export function canReplayWithSameKey(attemptedAt: string | null | undefined, now: number = Date.now()): boolean {
  if (!attemptedAt) return false;
  const parsed = Date.parse(attemptedAt);
  if (!Number.isFinite(parsed)) return false;
  const age = now - parsed;
  return age >= -WRITE_RECOVERY_FUTURE_SKEW_MS && age <= WRITE_RECOVERY_WINDOW_MS;
}

export type WriteFailureClassification =
  | { outcome: "duplicate_create"; operationId: number; message: string }
  | { outcome: "changed_on_github"; current: BoardSnapshot; message: string }
  | { outcome: "recheck_unresolved"; operation: WriteOperation | null; message: string }
  | { outcome: "possibly_recorded"; message: string }
  | { outcome: "not_recorded"; message: string };

export function mayHaveBeenRecorded(error: unknown, opts: { isReplay?: boolean } = {}): boolean {
  if (!(error instanceof IssueWriteError)) return true;
  if (error.kind === "duplicate_create" || error.kind === "recheck_unresolved") return false;
  if (opts.isReplay) return true;
  if (error.kind === "transport" || error.kind === "server") return true;
  if (error.disposition === "possibly-recorded") return true;
  if (error.disposition === "not-recorded") return false;
  if (error.status === 503) return true;
  return false;
}

export function classifyWriteFailure(error: unknown, opts: { isReplay?: boolean } = {}): WriteFailureClassification {
  const message = error instanceof Error ? error.message : "Could not submit to GitHub.";
  if (error instanceof IssueWriteError) {
    if (error.kind === "duplicate_create" && error.operationId) {
      return { outcome: "duplicate_create", operationId: error.operationId, message };
    }
    if (error.kind === "changed_on_github" && error.current) {
      return { outcome: "changed_on_github", current: error.current, message };
    }
    if (error.kind === "recheck_unresolved") {
      return { outcome: "recheck_unresolved", operation: error.operation, message };
    }
  }
  return mayHaveBeenRecorded(error, opts)
    ? { outcome: "possibly_recorded", message }
    : { outcome: "not_recorded", message };
}

function parseWriteDisposition(header: string | null): WriteDisposition {
  const value = header?.trim().toLowerCase();
  if (value === "not-recorded" || value === "possibly-recorded") return value;
  return null;
}

function parseRetryAfter(header: string | null): string | null {
  if (!header) return null;
  const seconds = Number(header);
  if (Number.isFinite(seconds) && seconds > 0) {
    return new Date(Date.now() + seconds * 1000).toISOString();
  }
  const parsed = Date.parse(header);
  return Number.isFinite(parsed) ? new Date(parsed).toISOString() : null;
}

async function toIssueWriteError(response: Response): Promise<IssueWriteError> {
  const retryAfter = parseRetryAfter(response.headers.get("Retry-After"));
  const disposition = parseWriteDisposition(response.headers.get("RepoOrbit-Write-Disposition"));
  const text = (await response.text().catch(() => "")).trim();
  let parsed: Record<string, unknown> | null = null;
  if (text.startsWith("{")) {
    try {
      parsed = JSON.parse(text) as Record<string, unknown>;
    } catch { /* plain text error */ }
  }
  if (response.status === 409) {
    if (parsed && parsed.current && typeof parsed.current === "object") {
      return new IssueWriteError("This issue changed on GitHub after the change was staged.", { status: 409, kind: "changed_on_github", disposition, current: parsed.current as BoardSnapshot });
    }
    if (parsed && typeof parsed.operationId === "number") {
      const msg = typeof parsed.error === "string" && parsed.error ? parsed.error : "An earlier create of this issue is still unresolved; recheck it instead of resubmitting.";
      return new IssueWriteError(msg, { status: 409, kind: "duplicate_create", disposition, operationId: parsed.operationId });
    }
    if (parsed && typeof parsed.id === "number" && typeof parsed.state === "string") {
      return new IssueWriteError("The issue on GitHub could not be confirmed as matching this operation yet.", {
        status: 409,
        kind: "recheck_unresolved",
        disposition,
        operation: parsed as unknown as WriteOperation,
      });
    }
  }
  const fallback: Record<number, [IssueWriteErrorKind, string]> = {
    400: ["validation", "Check the issue fields and try again."],
    401: ["unauthenticated", "Your GitHub session expired. Sign in again to write to GitHub."],
    403: ["forbidden", "Your current GitHub permission does not allow this action."],
    404: ["not_found", "This repository or issue is not available for writing."],
    422: ["unprocessable", "GitHub could not accept this request."],
    502: ["transport", "No reliable answer from the server. Your draft is preserved; check Write history before retrying."],
    503: ["unverified", "GitHub access could not be verified right now. Try again shortly."],
    504: ["transport", "The request timed out before a reliable answer arrived. Your draft is preserved; check Write history before retrying."],
  };
  const [kind, defaultMessage] = fallback[response.status] ?? ["server", "Could not complete the issue request."];
  return new IssueWriteError(text || defaultMessage, { status: response.status, kind, disposition, retryAfter });
}

export async function fetchWorkspaceIssues(id: string, after: string | null, signal: AbortSignal) {
  const { data, errors } = await graphqlQuery<{
    issues: { nodes: WorkspaceIssue[]; withheld: number; retryAt: string | null; hasNextPage: boolean; endCursor: string | null };
  }>(`query WorkspaceIssues($id: ID!, $after: ID) {
    issues(repositoryIds: [$id], first: 20, after: $after) {
      nodes { id repositoryId number title state url authorLogin updatedAt labels assignees }
      withheld retryAt hasNextPage endCursor
    }
  }`, { id, after }, signal);
  if (!data || errors?.length) throw new Error("Could not load issues");
  if (data.issues.withheld) throw new RepositoryAccessUnverifiedError(data.issues.retryAt ?? null);
  return data.issues;
}

// Fetches one imported issue by number (null when it has not been imported), so a restored editor knows its author and state.
export async function fetchWorkspaceIssue(id: string, number: number, signal: AbortSignal): Promise<WorkspaceIssue | null> {
  const { data, errors } = await graphqlQuery<{ issues: { nodes: WorkspaceIssue[]; withheld: number; retryAt: string | null } }>(`query WorkspaceIssue($id: ID!, $number: Int!) {
    issues(repositoryIds: [$id], first: 1, number: $number) {
      nodes { id repositoryId number title state url authorLogin updatedAt labels assignees }
      withheld retryAt
    }
  }`, { id, number }, signal);
  if (!data || errors?.length) throw new Error("Could not load the issue");
  if (data.issues.withheld) throw new RepositoryAccessUnverifiedError(data.issues.retryAt ?? null);
  return data.issues.nodes[0] ?? null;
}

export async function fetchIssueCapabilities(id: string, signal?: AbortSignal): Promise<IssueCapabilities> {
  const response = await fetch(`/api/repositories/${encodeURIComponent(id)}/issue-capabilities`, { cache: "no-store", signal });
  if (!response.ok) throw await toIssueWriteError(response);
  return response.json();
}

// Lists the users GitHub would accept as assignees (empty when the viewer cannot assign), for editor suggestions.
export async function fetchAssignableUsers(id: string, signal?: AbortSignal): Promise<string[]> {
  const response = await fetch(`/api/repositories/${encodeURIComponent(id)}/assignees`, { cache: "no-store", signal });
  if (!response.ok) throw await toIssueWriteError(response);
  return ((await response.json()) as { assignees: string[] }).assignees;
}

export async function fetchWriteOperations(id: string, signal?: AbortSignal): Promise<WriteOperation[]> {
  const response = await fetch(`/api/repositories/${encodeURIComponent(id)}/write-operations`, { cache: "no-store", signal });
  if (!response.ok) throw await toIssueWriteError(response);
  const body: { operations: WriteOperation[] } = await response.json();
  return body.operations ?? [];
}

export async function fetchWriteOperation(operationId: number, signal?: AbortSignal): Promise<WriteOperation> {
  const response = await fetch(`/api/write-operations/${encodeURIComponent(String(operationId))}`, { cache: "no-store", signal });
  if (!response.ok) throw await toIssueWriteError(response);
  return response.json();
}

export async function submitIssueCreate(repositoryId: string, idempotencyKey: string, draft: IssueCreateDraft): Promise<WriteOperation> {
  return submitIssueWrite(`/api/repositories/${encodeURIComponent(repositoryId)}/issues`, idempotencyKey, draft);
}

export async function submitIssueUpdate(repositoryId: string, issueNumber: number, idempotencyKey: string, draft: IssueUpdateDraft): Promise<WriteOperation> {
  return submitIssueWrite(`/api/repositories/${encodeURIComponent(repositoryId)}/issues/${encodeURIComponent(String(issueNumber))}/updates`, idempotencyKey, draft);
}

export async function fetchStatusLabels(repositoryId: string, signal?: AbortSignal): Promise<StatusLabelState> {
  const response = await fetch(`/api/repositories/${encodeURIComponent(repositoryId)}/status-labels`, { cache: "no-store", signal });
  if (!response.ok) throw await toIssueWriteError(response);
  return response.json();
}

// Turns status labels on: a reviewed write that creates the missing labels and reuses existing ones.
export async function enableStatusLabels(repositoryId: string, idempotencyKey: string): Promise<WriteOperation> {
  return submitIssueWrite(`/api/repositories/${encodeURIComponent(repositoryId)}/status-labels`, idempotencyKey, {});
}

// Turns status labels off. Nothing is written to GitHub; the labels stay.
export async function disableStatusLabels(repositoryId: string): Promise<void> {
  const response = await fetch(`/api/repositories/${encodeURIComponent(repositoryId)}/status-labels`, {
    method: "DELETE",
    headers: { "X-CSRF-Token": csrfToken ?? "" },
    cache: "no-store",
  }).catch(() => {
    throw new IssueWriteError("Could not reach the server. Check the setting before trying again.", { status: 0, kind: "transport" });
  });
  if (!response.ok) throw await toIssueWriteError(response);
}

async function submitIssueWrite(path: string, idempotencyKey: string, draft: IssueCreateDraft | IssueUpdateDraft | Record<string, never>): Promise<WriteOperation> {
  const response = await fetch(path, {
    method: "POST",
    headers: { "Content-Type": "application/json", "X-CSRF-Token": csrfToken ?? "", "Idempotency-Key": idempotencyKey },
    cache: "no-store",
    body: JSON.stringify(draft),
  }).catch(() => {
    throw new IssueWriteError("No reliable answer from the server. Your draft is preserved; check Write history before retrying.", { status: 0, kind: "transport" });
  });
  if (!response.ok) throw await toIssueWriteError(response);
  return response.json();
}

export async function recheckWriteOperation(operationId: number, issueNumber?: number): Promise<WriteOperation> {
  const payload = issueNumber !== undefined ? { issueNumber } : {};
  const response = await fetch(`/api/write-operations/${encodeURIComponent(String(operationId))}/recheck`, {
    method: "POST",
    headers: { "Content-Type": "application/json", "X-CSRF-Token": csrfToken ?? "" },
    cache: "no-store",
    body: JSON.stringify(payload),
  }).catch(() => {
    throw new IssueWriteError("Could not reach the server to recheck this operation. Try again shortly.", { status: 0, kind: "transport" });
  });
  if (!response.ok) throw await toIssueWriteError(response);
  return response.json();
}

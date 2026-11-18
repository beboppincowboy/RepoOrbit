export type Issue = { id: string; repositoryId: string; title: string; state: string };
// Wire values of the backend's repositories.SyncState; "demo" appears only in Demo mode.
export type SyncState = "demo" | "awaiting-sync" | "not-synchronized";
// Shared metadata refresh status (null when not selected, and in Demo mode). Times are RFC 3339; failureCode is a safe machine code.
// "paused" means waiting for a usable GitHub credential; "dead" needs an operator requeue.
export type MetadataStatus = { state: "not-scheduled" | "queued" | "running" | "paused" | "dead"; lastSuccessAt: string | null; nextAttemptAt: string | null; paused: boolean; failureCode: string | null; followSuspended: boolean };
// Activity coverage is separate from metadata: synchronized is true only when every activity stream is caught up and fresh.
export type ActivityResourceStatus = { phase: string; boundary: string; catchupThrough: string | null; lastSuccessAt: string | null; failureCode: string | null; jobState: string; nextAttemptAt: string | null };
export type ChildResourceStatus = { complete: boolean; pendingChildren: number; lastSuccessAt: string | null; failureCode: string | null; nextAttemptAt: string | null };
export type ActivityStatus = { synchronized: boolean; message: string; issues: ActivityResourceStatus | null; pullRequests: ActivityResourceStatus | null; releases: ActivityResourceStatus | null; reviews: ChildResourceStatus | null; comments: ChildResourceStatus | null; issueEvents: { traversal: ActivityResourceStatus | null; historicalGap: boolean } | null; futureResources: string[] };
// fullName is "owner/name".
type SelectedRepository = { id: string; fullName: string; description: string; syncState: SyncState; metadataStatus: MetadataStatus | null };
type IssueConnection = { nodes: Issue[]; hasNextPage: boolean; withheld: number; retryAt: string | null };
type RepositoryDetail =
  | { state: "available"; activity: ActivityStatus | null; issues: IssueConnection }
  | { state: "access-unverified"; retryAt: string | null };
// withheld counts selected repositories hidden because access could not be verified; retryAt is set when GitHub rate limited the check.
type RepositoryConnection = { nodes: SelectedRepository[]; endCursor: string | null; hasNextPage: boolean; withheld: number; retryAt: string | null };
// One row of a REST repository list. `selected` means connected on the owned list and followed on the starred list.
export type ListedRepo = { id: string; fullName: string; description: string; private: boolean; selected: boolean; syncState: SyncState };
export type OwnedPage = { repositories: ListedRepo[]; nextPage: number; refreshFailed: boolean; withheld: number; retryAt: string | null; installUrl: string };
export type StarredPage = { repositories: ListedRepo[]; nextPage: number; refreshedAt: string | null; refreshFailed: boolean; withheld: number; retryAt: string | null; installUrl: string };
export type Me = { mode: "demo" | "real"; user: { login: string; demo: boolean } | null; csrfToken: string | null };
let csrfToken: string | null = null;
// me() fetches the session and, as a side effect, stores its CSRF token in this module for every later
// mutating call (logout, setSelected, setFollowed, GraphQL). The workspace loads it before any of them.
export async function me(): Promise<Me> {
  const response = await fetch("/auth/me", { cache: "no-store" });
  if (!response.ok) throw new Error("Unable to load session");
  const result: Me = await response.json();
  csrfToken = result.csrfToken;
  return result;
}
// Fetches the current session on workspace entry and supplies the CSRF token for subsequent requests.
export const sessionQuery = { queryKey: ["session"], queryFn: me, staleTime: 0 } as const;
// Ends the current session in Go. The caller owns navigation and local query state. Rejects on HTTP or network failure.
export async function logout() {
  const response = await fetch("/auth/logout", { method: "POST", headers: { "X-CSRF-Token": csrfToken ?? "" } });
  if (!response.ok) throw new Error("Could not sign out. Try again.");
}
type GraphQLResponse<Data> = {
  data?: Data | null;
  errors?: { path?: (string | number)[]; extensions?: { code?: string; retryAt?: string } }[];
};

async function graphqlQuery<Data>(queryText: string, variables: Record<string, unknown>, signal?: AbortSignal): Promise<GraphQLResponse<Data>> {
  const response = await fetch("/graphql", { method: "POST", headers: { "Content-Type": "application/json", "X-CSRF-Token": csrfToken ?? "" }, cache: "no-store", signal, body: JSON.stringify({ query: queryText, variables }) });
  if (!response.ok) throw new Error("Unable to load repositories");
  return response.json();
}
// The backend caps GraphQL nesting at 6 levels (a leaf one level deeper is rejected) and query complexity at 1000.
// So the list query stays shallow, and activity and issues come from aliased queries of at most 8 repositories each:
// repository(id) and issues(repositoryIds) are each one level shallower than selectedRepositories { nodes { activity { issueEvents { traversal } } } }.
const activityResourceFields = "phase boundary catchupThrough lastSuccessAt failureCode jobState nextAttemptAt";
const childResourceFields = "complete pendingChildren lastSuccessAt failureCode nextAttemptAt";
const activityFields = `synchronized message issues { ${activityResourceFields} } pullRequests { ${activityResourceFields} } releases { ${activityResourceFields} } reviews { ${childResourceFields} } comments { ${childResourceFields} } issueEvents { traversal { ${activityResourceFields} } historicalGap } futureResources`;
const detailChunk = 8;
type DetailResponse = Partial<Record<`a${number}`, { activity: ActivityStatus | null } | null>>
  & Partial<Record<`r${number}`, IssueConnection | null>>;

// Rechecks access and fetches activity and up to five issues per ID, in input order.
// Accepts one to eight IDs. Access-unverified entries carry only an optional retry time.
// Rejects an invalid batch size, cancellation, or a request or unexpected GraphQL error.
export async function fetchRepositoryDetails(ids: string[], signal?: AbortSignal): Promise<RepositoryDetail[]> {
  if (ids.length === 0 || ids.length > detailChunk) throw new RangeError("Expected one to eight repository IDs");
  const declarations = ids.map((_, i) => `$id${i}: ID!`).join(", ");
  const fields = ids.map((_, i) => `a${i}: repository(id: $id${i}) { activity { ${activityFields} } } r${i}: issues(repositoryIds: [$id${i}], first: 5) { nodes { id repositoryId title state } hasNextPage withheld retryAt }`).join(" ");
  const { data, errors = [] } = await graphqlQuery<DetailResponse>(`query RepositoryDetails(${declarations}) { ${fields} }`, Object.fromEntries(ids.map((id, i) => [`id${i}`, id])), signal);
  if (!data || errors.some(error => error.extensions?.code !== "ACCESS_UNVERIFIED" || !error.path?.length)) {
    throw new Error("Unable to load repository details");
  }
  return ids.map((_, i) => {
    const repository = data[`a${i}`];
    const issues = data[`r${i}`];
    const accessError = errors.find(error => error.path?.[0] === `a${i}` || error.path?.[0] === `r${i}`);
    if (repository === null || accessError || issues?.withheld) {
      return { state: "access-unverified", retryAt: accessError?.extensions?.retryAt ?? issues?.retryAt ?? null };
    }
    if (!repository || !issues) throw new Error("Unable to load repository details");
    return { state: "available", activity: repository.activity, issues };
  });
}

type SelectedRepositoryBatch = { repositories: SelectedRepository[]; details: RepositoryDetail[] | null };
type SelectedRepositoryPage = Omit<RepositoryConnection, "nodes"> & { batches: SelectedRepositoryBatch[] };

// Fetches one page of selected repositories, then their details with at most two requests in flight.
// Pass null for the first page or the previous page’s endCursor. Batches preserve list order and retain IDs for retry.
// List failures and cancellation reject the call. A failed detail batch has null details.
export async function fetchSelectedRepositories(after: string | null, signal?: AbortSignal): Promise<SelectedRepositoryPage> {
  const { data, errors } = await graphqlQuery<{ selectedRepositories: RepositoryConnection }>(`query SelectedRepositories($after: ID) { selectedRepositories(first: 20, after: $after) { nodes { id fullName description syncState metadataStatus { state lastSuccessAt nextAttemptAt paused failureCode followSuspended } } endCursor hasNextPage withheld retryAt } }`, { after }, signal);
  if (!data || errors?.length) throw new Error("Unable to load repositories");
  const { nodes, ...connection } = data.selectedRepositories;
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
// Loads one page of repositories on the user's personal GitHub account (selected or not) from GitHub.
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
type Coverage = { covered: boolean; reason: string | null; through: string | null };
type CountMetric = { value: number | null; coverage: Coverage };
type MedianMetric = { seconds: number | null; sampleSize: number | null; coverage: Coverage };

type RepositoryOverview = {
  fullName: string;
  syncState: SyncState;
  activity: ActivityStatus & { archived: boolean };
  openIssues: CountMetric;
  staleOpenIssues: CountMetric;
  pullRequestsMerged: CountMetric;
  firstResponseTime: MedianMetric;
  issuesWithoutResponse: Pick<CountMetric, "value">;
  mergeTime: MedianMetric;
  eventsCoveredFrom: string | null;
  windowStart: string;
  activeBotContributors: CountMetric;
  contributorsCoverage: Coverage;
  contributors: { login: string; pullRequestsOpened: number; reviewsSubmitted: number }[];
  timeline: { url: string; sourceId: number; kind: string; occurredAt: string; title: string; number: number | null; actor: { login: string; isBot: boolean } | null }[];
  daily: { day: string; issuesOpened: number | null; pullRequestsMerged: number | null }[];
};

export async function fetchRepositoryOverview(id: string, days: MetricWindow, signal: AbortSignal) {
  const coverage = "covered reason through";
  const count = `value coverage { ${coverage} }`;
  const median = `seconds sampleSize coverage { ${coverage} }`;
  const { data, errors } = await graphqlQuery<{
    repositoryOverview: RepositoryOverview | null;
  }>(`query RepositoryOverview($id: ID!, $days: Int!) {
    repositoryOverview(id: $id, days: $days) {
      fullName syncState activity { ${activityFields} archived }
      openIssues { ${count} } staleOpenIssues { ${count} } pullRequestsMerged { ${count} }
      firstResponseTime { ${median} } mergeTime { ${median} } issuesWithoutResponse { value }
      activeBotContributors { ${count} } contributorsCoverage { ${coverage} }
      contributors { login pullRequestsOpened reviewsSubmitted }
      windowStart eventsCoveredFrom
      daily { day issuesOpened pullRequestsMerged }
      timeline { sourceId kind occurredAt title number url actor { login isBot } }
    }
  }`, { id, days }, signal);
  if (errors?.some(error => error.extensions?.code === "ACCESS_UNVERIFIED")) {
    throw new Error("Access could not be verified");
  }
  if (!data || errors?.length) throw new Error("Could not load repository analytics");
  return data.repositoryOverview;
}

export async function fetchWorkspacePullRequests(id: string, signal: AbortSignal) {
  const { data, errors } = await graphqlQuery<{
    pullRequestEvidence: { nodes: { id: string; number: number; title: string; state: string; mergedAt: string | null }[]; withheld: number; hasNextPage: boolean };
  }>(`query WorkspacePullRequests($id: ID!) {
    pullRequestEvidence(repositoryIds: [$id], first: 20) { nodes { id number title state mergedAt } withheld hasNextPage }
  }`, { id }, signal);
  if (!data || errors?.length) throw new Error("Could not load pull requests");
  if (data.pullRequestEvidence.withheld) throw new Error("Access could not be verified");
  return data.pullRequestEvidence;
}

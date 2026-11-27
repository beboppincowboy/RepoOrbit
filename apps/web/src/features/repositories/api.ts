import { getCsrfToken, graphqlQuery, restFetch } from "@/shared/api/transport";

export type Issue = { id: string; repositoryId: string; title: string; state: string };
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

const activityResourceFields = "phase boundary catchupThrough lastSuccessAt failureCode jobState nextAttemptAt";
const childResourceFields = "complete pendingChildren lastSuccessAt failureCode nextAttemptAt";
export const activityFields = `synchronized message issues { ${activityResourceFields} } pullRequests { ${activityResourceFields} } releases { ${activityResourceFields} } reviews { ${childResourceFields} } comments { ${childResourceFields} } issueEvents { traversal { ${activityResourceFields} } historicalGap } futureResources`;
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
  const response = await restFetch(`/api/repositories/owned?page=${page}`);
  if (!response.ok) throw new Error("Unable to load your repositories");
  return response.json();
}
// Connects (selected=true) or disconnects an owned repository. Throws a user-facing Error on failure.
export async function setSelected(id: string, selected: boolean) {
  const response = await fetch(`/api/repositories/${encodeURIComponent(id)}/selection`, { method: selected ? "PUT" : "DELETE", headers: { "X-CSRF-Token": getCsrfToken() } });
  if (response.status === 503) throw new Error("GitHub could not verify access right now. Try again shortly.");
  if (!response.ok) throw new Error(selected ? "Could not connect this repository." : "Could not disconnect this repository.");
}
// Loads one page of the stored star snapshot; refresh=true first re-reads the stars from GitHub.
export async function starredRepositories(page = 1, refresh = false): Promise<StarredPage> {
  const response = await restFetch(`/api/repositories/starred?page=${page}${refresh ? "&refresh=1" : ""}`);
  if (!response.ok) throw new Error("Unable to load your starred repositories");
  return response.json();
}
// Follows (followed=true) or unfollows a starred repository. Throws a user-facing Error on failure.
export async function setFollowed(id: string, followed: boolean) {
  const base = `/api/repositories/${encodeURIComponent(id)}`;
  const response = await fetch(followed ? `${base}/follow` : `${base}/selection`, { method: followed ? "PUT" : "DELETE", headers: { "X-CSRF-Token": getCsrfToken() } });
  if (response.status === 503) throw new Error("GitHub could not verify access right now. Try again shortly.");
  if (!response.ok) throw new Error(followed ? "Could not follow this repository. Refresh your stars and try again." : "Could not unfollow this repository.");
}
// Requests an activity refresh. Returns "accepted" or "coalesced" (202); throws a user-facing Error otherwise.
export async function refreshActivity(id: string): Promise<"accepted" | "coalesced"> {
  const response = await fetch(`/api/repositories/${encodeURIComponent(id)}/activity/refresh`, { method: "POST", headers: { "X-CSRF-Token": getCsrfToken() } });
  if (response.status === 202) return (await response.json().catch(() => null))?.state === "coalesced" ? "coalesced" : "accepted";
  if (response.status === 409) throw new Error("Activity sync stopped after repeated failures; an operator must requeue it.");
  if (response.status === 404) throw new Error("This repository is no longer eligible for activity sync.");
  if (response.status === 503) throw new Error("GitHub could not verify access right now. Try again shortly.");
  throw new Error("Could not request an activity refresh.");
}

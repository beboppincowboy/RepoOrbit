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
export type Repository = { id: string; fullName: string; description: string; syncState: SyncState; metadataStatus: MetadataStatus | null; activity: ActivityStatus | null; issues: { nodes: Issue[]; hasNextPage: boolean; withheld: number; retryAt: string | null } };
// withheld counts selected repositories hidden because access could not be verified; retryAt is set when GitHub rate limited the check.
export type RepositoryConnection = { nodes: Repository[]; endCursor: string | null; hasNextPage: boolean; withheld: number; retryAt: string | null };
// One row of a REST repository list. `selected` means connected on the owned list and followed on the starred list.
export type ListedRepo = { id: string; fullName: string; description: string; private: boolean; selected: boolean; syncState: SyncState };
export type OwnedPage = { repositories: ListedRepo[]; nextPage: number; refreshFailed: boolean; withheld: number; retryAt: string | null; installUrl: string };
export type StarredPage = { repositories: ListedRepo[]; nextPage: number; refreshedAt: string | null; refreshFailed: boolean; withheld: number; retryAt: string | null; installUrl: string };
export type Me = { mode: "demo" | "real"; user: { login: string; demo: boolean } | null; csrfToken: string | null };
let csrfToken: string | null = null;
// me() fetches the session and, as a side effect, stores its CSRF token in this module for every later
// mutating call (logout, setSelected, setFollowed, GraphQL). Call it (AuthShell does) before any of them.
export async function me(): Promise<Me> {
  const response = await fetch("/auth/me", { cache: "no-store" });
  if (!response.ok) throw new Error("Unable to load session");
  const result: Me = await response.json();
  csrfToken = result.csrfToken;
  return result;
}
// Session query. staleTime 0: every entry into the workspace re-reads /auth/me, as the old AuthShell did on navigation.
export const sessionQuery = { queryKey: ["session"], queryFn: me, staleTime: 0 } as const;
export async function logout() {
  await fetch("/auth/logout", { method: "POST", headers: { "X-CSRF-Token": csrfToken ?? "" } });
}
async function graphqlQuery(queryText: string, variables: Record<string, unknown>) {
  const response = await fetch("/graphql", { method: "POST", headers: { "Content-Type": "application/json", "X-CSRF-Token": csrfToken ?? "" }, cache: "no-store", body: JSON.stringify({ query: queryText, variables }) });
  if (!response.ok) throw new Error("Unable to load repositories");
  const result = await response.json();
  if (result.errors || !result.data) throw new Error("Unable to load repositories");
  return result.data;
}
// The backend caps GraphQL nesting at 6 levels (a leaf one level deeper is rejected) and query complexity at 1000.
// So the list query stays shallow, and activity and issues come from aliased queries of at most 8 repositories each:
// repository(id) and issues(repositoryIds) are each one level shallower than selectedRepositories { nodes { activity { issueEvents { traversal } } } }.
const activityFields = `synchronized message issues { phase boundary catchupThrough lastSuccessAt failureCode jobState nextAttemptAt } pullRequests { phase boundary catchupThrough lastSuccessAt failureCode jobState nextAttemptAt } releases { phase boundary catchupThrough lastSuccessAt failureCode jobState nextAttemptAt } reviews { complete pendingChildren lastSuccessAt failureCode nextAttemptAt } comments { complete pendingChildren lastSuccessAt failureCode nextAttemptAt } issueEvents { traversal { phase boundary catchupThrough lastSuccessAt failureCode jobState nextAttemptAt } historicalGap } futureResources`;
const detailChunk = 8;
type Details = { activity: ActivityStatus | null; issues: Repository["issues"] };
async function repositoryDetails(ids: string[]): Promise<Details[]> {
  const declarations = ids.map((_, i) => `$id${i}: ID!`).join(", ");
  const fields = ids.map((_, i) => `a${i}: repository(id: $id${i}) { activity { ${activityFields} } } r${i}: issues(repositoryIds: [$id${i}], first: 5) { nodes { id repositoryId title state } hasNextPage withheld retryAt }`).join(" ");
  const data = await graphqlQuery(`query RepositoryDetails(${declarations}) { ${fields} }`, Object.fromEntries(ids.map((id, i) => [`id${i}`, id])));
  return ids.map((_, i) => ({ activity: data[`a${i}`]?.activity ?? null, issues: data[`r${i}`] }));
}
// Loads one page of the viewer's SELECTED repositories (not every accessible one) with their activity and first 5 issues.
// Pass the previous page's endCursor as `after`.
export async function selectedRepositories(after: string | null = null): Promise<RepositoryConnection> {
  const { selectedRepositories: connection } = await graphqlQuery(`query SelectedRepositories($after: ID) { selectedRepositories(first: 20, after: $after) { nodes { id fullName description syncState metadataStatus { state lastSuccessAt nextAttemptAt paused failureCode followSuspended } } endCursor hasNextPage withheld retryAt } }`, { after });
  const listed: Array<Omit<Repository, "activity" | "issues">> = connection.nodes;
  const chunks: string[][] = [];
  for (let i = 0; i < listed.length; i += detailChunk) chunks.push(listed.slice(i, i + detailChunk).map(repo => repo.id));
  const details = (await Promise.all(chunks.map(repositoryDetails))).flat();
  const nodes: Repository[] = listed.map((repo, i) => {
    // A repository GitHub no longer lets the viewer read (access changed after the list was loaded) shows as access unverified; the rest of the page still loads.
    if (!details[i].activity) return { ...repo, activity: null, issues: { nodes: [], hasNextPage: false, withheld: 1, retryAt: null } };
    return { ...repo, activity: details[i].activity, issues: details[i].issues };
  });
  return { ...connection, nodes };
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

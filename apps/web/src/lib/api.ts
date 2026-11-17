export type Issue = { id: string; repositoryId: string; title: string; state: string };
// Wire values of the backend's repositories.SyncState; "demo" appears only in Demo mode.
export type SyncState = "demo" | "awaiting-sync" | "not-synchronized";
// Shared metadata refresh status (null when not selected, and in Demo mode). Times are RFC 3339; failureCode is a safe machine code.
// "paused" means waiting for a usable GitHub credential; "dead" needs an operator requeue.
export type MetadataStatus = { state: "not-scheduled" | "queued" | "running" | "paused" | "dead"; lastSuccessAt: string | null; nextAttemptAt: string | null; paused: boolean; failureCode: string | null; followSuspended: boolean };
// Activity coverage is separate from metadata: synchronized stays false until activity ingestion exists.
export type ActivityResourceStatus = { phase: string; boundary: string; catchupThrough: string | null; lastSuccessAt: string | null; failureCode: string | null; jobState: string; nextAttemptAt: string | null };
export type ActivityStatus = { synchronized: boolean; message: string; issues: ActivityResourceStatus | null; futureResources: string[] };
// fullName is "owner/name".
export type Repository = { id: string; fullName: string; description: string; syncState: SyncState; metadataStatus: MetadataStatus | null; activity: ActivityStatus; issues: { nodes: Issue[]; hasNextPage: boolean; withheld: number; retryAt: string | null } };
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
// Loads one page of the viewer's SELECTED repositories (not every accessible one), then one extra GraphQL
// request for each repository's first 5 issues. Pass the previous page's endCursor as `after`.
export async function selectedRepositories(after: string | null = null): Promise<RepositoryConnection> {
  const { selectedRepositories: connection } = await graphqlQuery(`query SelectedRepositories($after: ID) { selectedRepositories(first: 20, after: $after) { nodes { id fullName description syncState metadataStatus { state lastSuccessAt nextAttemptAt paused failureCode followSuspended } activity { synchronized message issues { phase boundary catchupThrough lastSuccessAt failureCode jobState nextAttemptAt } futureResources } } endCursor hasNextPage withheld retryAt } }`, { after });
  if (!connection.nodes.length) return connection;
  // One aliased issues field per repository so each repo gets its own page and hasNextPage.
  const ids: string[] = connection.nodes.map((repo: Repository) => repo.id);
  const fields = ids.map((_, i) => `r${i}: issues(repositoryIds: [$id${i}], first: 5) { nodes { id repositoryId title state } hasNextPage withheld retryAt }`).join(" ");
  const issues = await graphqlQuery(`query Issues(${ids.map((_, i) => `$id${i}: ID!`).join(", ")}) { ${fields} }`, Object.fromEntries(ids.map((id, i) => [`id${i}`, id])));
  return { ...connection, nodes: connection.nodes.map((repo: Repository, i: number) => ({ ...repo, issues: issues[`r${i}`] })) };
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

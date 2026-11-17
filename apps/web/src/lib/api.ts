export type Issue = { id: string; repositoryId: string; title: string; state: string };
export type Repository = { id: string; name: string; description: string; syncState: string; issues: { nodes: Issue[]; hasNextPage: boolean } };
// withheld counts selected repositories hidden because access could not be verified; retryAt is set when GitHub rate limited the check.
export type RepositoryConnection = { nodes: Repository[]; endCursor: string | null; hasNextPage: boolean; withheld: number; retryAt: string | null };
export type OwnedRepo = { id: string; name: string; description: string; private: boolean; selected: boolean; syncState: string };
export type OwnedPage = { repositories: OwnedRepo[]; nextPage: number; refreshFailed: boolean; withheld: number; retryAt: string | null; installUrl: string };
export type Me = { mode: "demo" | "real"; user: { login: string; demo: boolean } | null; csrfToken: string | null };
let csrfToken: string | null = null;
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
async function query(query: string, variables: Record<string, unknown>) {
  const response = await fetch("/graphql", { method: "POST", headers: { "Content-Type": "application/json", "X-CSRF-Token": csrfToken ?? "" }, cache: "no-store", body: JSON.stringify({ query, variables }) });
  if (!response.ok) throw new Error("Unable to load repositories");
  const result = await response.json();
  if (result.errors || !result.data) throw new Error("Unable to load repositories");
  return result.data;
}
export async function repositories(after: string | null = null): Promise<RepositoryConnection> {
  const { repositories } = await query(`query Repositories($after: ID) { repositories(first: 20, after: $after) { nodes { id name description syncState } endCursor hasNextPage withheld retryAt } }`, { after });
  if (!repositories.nodes.length) return repositories;
  // One aliased issues field per repository so each repo gets its own page and hasNextPage.
  const ids: string[] = repositories.nodes.map((repo: Repository) => repo.id);
  const fields = ids.map((_, i) => `r${i}: issues(repositoryIds: [$id${i}], first: 5) { nodes { id repositoryId title state } hasNextPage }`).join(" ");
  const issues = await query(`query Issues(${ids.map((_, i) => `$id${i}: ID!`).join(", ")}) { ${fields} }`, Object.fromEntries(ids.map((id, i) => [`id${i}`, id])));
  return { ...repositories, nodes: repositories.nodes.map((repo: Repository, i: number) => ({ ...repo, issues: issues[`r${i}`] })) };
}
export async function ownedRepositories(page = 1): Promise<OwnedPage> {
  const response = await fetch(`/api/repositories/owned?page=${page}`, { cache: "no-store" });
  if (!response.ok) throw new Error("Unable to load your repositories");
  return response.json();
}
export async function setSelected(id: string, selected: boolean) {
  const response = await fetch(`/api/repositories/${encodeURIComponent(id)}/selection`, { method: selected ? "PUT" : "DELETE", headers: { "X-CSRF-Token": csrfToken ?? "" } });
  if (response.status === 503) throw new Error("GitHub could not verify access right now. Try again shortly.");
  if (!response.ok) throw new Error(selected ? "Could not connect this repository." : "Could not disconnect this repository.");
}

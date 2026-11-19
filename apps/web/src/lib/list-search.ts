// URL search params shared by the repository lists. Shareable: `?q=` filters by name or description, `view=selected` shows only connected rows.
export type ListSearch = { q?: string; view?: "selected" };

export function validateListSearch(search: Record<string, unknown>): ListSearch {
  const q = typeof search.q === "string" && search.q.trim() ? search.q : undefined;
  return { q, view: search.view === "selected" ? "selected" : undefined };
}

export function matchesListSearch(repo: { fullName: string; description: string }, q: string | undefined) {
  if (!q) return true;
  const needle = q.trim().toLowerCase();
  return repo.fullName.toLowerCase().includes(needle) || repo.description.toLowerCase().includes(needle);
}

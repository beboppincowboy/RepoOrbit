import { useInfiniteQuery } from "@tanstack/react-query";
import { fetchSelectedRepositories, type Me } from "@/lib/api";

// Must stay identical to the key and options in routes/_shell/index.tsx so the shell, the overview and the
// Repositories screen share one fetch (fetchSelectedRepositories also fires the per-batch detail requests).
export const selectedRepositoriesKey = (session: Me) => ["selectedRepositories", session.mode, session.user?.login] as const;

// Infinite query over the viewer's selected repositories. Network: each page also fetches activity/issue details per batch.
export function useSelectedRepositories(session: Me) {
  return useInfiniteQuery({
    queryKey: selectedRepositoriesKey(session),
    queryFn: ({ pageParam, signal }) => fetchSelectedRepositories(pageParam, signal),
    initialPageParam: null as string | null,
    getNextPageParam: last => (last.hasNextPage ? last.endCursor : undefined),
  });
}

type Page = Awaited<ReturnType<typeof fetchSelectedRepositories>>;
type Batch = Page["batches"][number];
// detail is undefined when the batch's detail request failed (or has not loaded); access-unverified rows carry that state.
export type SelectedRow = { repo: Batch["repositories"][number]; detail: NonNullable<Batch["details"]>[number] | undefined };

// Flattens the loaded pages into rows. Like the overview, a failed revalidation drops every cached row: it can no longer
// establish access to them. Access-unverified rows ARE included: callers must withhold their names and links.
export function selectedRows(query: ReturnType<typeof useSelectedRepositories>): SelectedRow[] {
  if (query.isRefetchError) return [];
  return (query.data?.pages ?? []).flatMap(p => p.batches.flatMap(b => b.repositories.map((repo, i) => ({ repo, detail: b.details?.[i] }))));
}

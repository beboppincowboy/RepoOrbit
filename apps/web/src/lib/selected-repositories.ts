import { useInfiniteQuery, useQueryClient } from "@tanstack/react-query";
import { fetchSelectedRepositoryMetadataPage, fetchSelectedRepositoryDetailsPage, type ViewerSession } from "@/lib/api";

// Exact key for selection pages with details. Invalidate by prefix to refresh the viewer’s metadata pages too.
export const selectedRepositoriesKey = (session: ViewerSession) => ["selectedRepositories", session.mode, session.user?.login] as const;

function useSelectedRepositoryMetadataPage(session: ViewerSession) {
  const queryClient = useQueryClient();
  return (after: string | null) => queryClient.fetchQuery({
    queryKey: [...selectedRepositoriesKey(session), "metadata-page", after],
    queryFn: ({ signal }) => fetchSelectedRepositoryMetadataPage(after, signal),
  });
}

// Pages the viewer’s verified selections and enriches each page with activity and issue details.
export function useSelectedRepositories(session: ViewerSession) {
  const fetchPage = useSelectedRepositoryMetadataPage(session);
  return useInfiniteQuery({
    queryKey: selectedRepositoriesKey(session),
    queryFn: ({ pageParam, signal }) => fetchSelectedRepositoryDetailsPage(fetchPage(pageParam), signal),
    initialPageParam: null as string | null,
    getNextPageParam: last => (last.hasNextPage ? last.endCursor : undefined),
  });
}

// Pages the same verified selection metadata without waiting for activity or issue details.
export function useSelectedRepositoryMetadata(session: ViewerSession) {
  const fetchPage = useSelectedRepositoryMetadataPage(session);
  return useInfiniteQuery({
    queryKey: [...selectedRepositoriesKey(session), "metadata"],
    queryFn: ({ pageParam }) => fetchPage(pageParam),
    initialPageParam: null as string | null,
    getNextPageParam: last => (last.hasNextPage ? last.endCursor : undefined),
  });
}

type Page = Awaited<ReturnType<typeof fetchSelectedRepositoryDetailsPage>>;
export type SelectedBatch = Page["batches"][number];
// detail is undefined when the batch's detail request failed (or has not loaded); access-unverified rows carry that state.
export type SelectedRow = { repo: SelectedBatch["repositories"][number]; detail: NonNullable<SelectedBatch["details"]>[number] | undefined };

// Flattens the loaded pages into rows. Like the overview, a failed revalidation drops every cached row: it can no longer
// establish access to them. Access-unverified rows ARE included: callers must withhold their names and links.
export function flattenSelectedRepositoryRows(query: ReturnType<typeof useSelectedRepositories>): SelectedRow[] {
  if (query.isRefetchError) return [];
  return (query.data?.pages ?? []).flatMap(p => p.batches.flatMap(b => b.repositories.map((repo, i) => ({ repo, detail: b.details?.[i] }))));
}

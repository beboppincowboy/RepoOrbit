import { keepPreviousData, useQuery } from "@tanstack/react-query";
import { fetchCrossRepositoryOverview } from "@/features/analytics/api";
import { currentAccessSeq } from "@/features/repositories/access-store";
import { crossRepositoryOverviewKey } from "@/features/repositories/query-keys";
import { RepositoryAccessUnverifiedError } from "@/shared/api/errors";
import type { MetricWindow, ViewerSession } from "@/shared/api/transport";

export function useCrossRepositoryOverview(session: ViewerSession, days: MetricWindow, enabled: boolean) {
  return useQuery({
    queryKey: [...crossRepositoryOverviewKey(session), days],
    queryFn: async ({ signal }) => {
      const startedAtSeq = currentAccessSeq();
      const overview = await fetchCrossRepositoryOverview(days, signal);
      return { overview, startedAtSeq };
    },
    enabled,
    retry: (count, error) => count < 1 && error instanceof RepositoryAccessUnverifiedError,
    retryDelay: 0,
    placeholderData: keepPreviousData,
  });
}

import { useEffect } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { fetchRepositoryOverview, type RepositoryOverview } from "@/features/analytics/api";
import {
  currentAccessSeq,
  recordAccessDenied,
  recordOverviewSnapshot,
  recordOverviewUnavailable,
  selectRepositoryVisibility,
  useRepositoryVisibility,
  useSelectedRepositories,
  useSelectedRepositoryMetadata,
} from "@/features/repositories/access-store";
import { repositoryOverviewKey, repositorySnapshotKey, selectedRepositoriesKey } from "@/features/repositories/query-keys";
import { syncDisplay, type SyncInput } from "@/features/repositories/sync-status";
import { GraphQLUnavailableError, RepositoryAccessUnverifiedError } from "@/shared/api/errors";
import type { MetricWindow, ViewerSession } from "@/shared/api/transport";

// The repository overview read, the saved snapshot shown when it cannot be refreshed, and what GitHub access allows showing.
// Refetches on its own once a rate-limited access check may be retried.
export function useRepositoryOverview(session: ViewerSession, repositoryId: string, days: MetricWindow) {
  const queryClient = useQueryClient();
  const demo = session.mode === "demo";
  const overviewKey = repositoryOverviewKey(session, repositoryId, days);
  const snapshotKey = repositorySnapshotKey(session, repositoryId, days);
  const query = useQuery({
    queryKey: overviewKey,
    queryFn: async ({ signal }) => {
      const startedAtSeq = currentAccessSeq();
      try {
        const data = await fetchRepositoryOverview(repositoryId, days, signal);
        signal.throwIfAborted();
        if (data) recordOverviewSnapshot(queryClient, session, repositoryId, days, data, startedAtSeq);
        else recordAccessDenied(queryClient, session, repositoryId);
        return data;
      } catch (error) {
        if (!signal.aborted) {
          recordOverviewUnavailable(queryClient, session, repositoryId, error, startedAtSeq);
        }
        throw error;
      }
    },
    placeholderData: (previous, query) => query?.queryKey[1] === session.mode && query.queryKey[2] === session.user?.login && query.queryKey[3] === repositoryId ? previous : undefined,
    gcTime: 0,
    refetchOnWindowFocus: ({ state }) => !(state.error instanceof RepositoryAccessUnverifiedError && state.error.retryAt),
    retry: (count, error) => !(error instanceof RepositoryAccessUnverifiedError || error instanceof GraphQLUnavailableError) && count < 3,
    refetchInterval: ({ state }) => {
      if (state.error instanceof RepositoryAccessUnverifiedError && state.error.retryAt) return false;
      if (!state.data) return false;
      const { syncState, activity } = state.data;
      const display = syncDisplay({ syncState, activity });
      if (display === "syncing") return 3000;
      if (display === "awaiting") return 10000;
      if (display !== "partial") return false;
      const streams = [activity.issues, activity.pullRequests, activity.releases, activity.issueEvents?.traversal];
      const children = [activity.reviews, activity.comments];
      if (streams.some(stream => stream?.jobState === "running" && stream.phase !== "current")) return 3000;
      const queued = streams.some(stream => stream?.jobState === "queued" && stream.phase !== "current")
        || children.some(child => child && !child.complete && child.nextAttemptAt);
      return queued ? 10000 : false;
    },
  });
  const snapshotReason = query.error instanceof GraphQLUnavailableError ? query.error.message
    : query.error instanceof RepositoryAccessUnverifiedError && query.error.retryAt ? "GitHub rate-limited access checks." : null;
  const snapshot = snapshotReason ? queryClient.getQueryState<RepositoryOverview>(snapshotKey) : undefined;
  const overview = query.isError ? snapshot?.data : query.data;
  const selection = useSelectedRepositories(session);
  const metadata = useSelectedRepositoryMetadata(session);
  const repoVisibility = useRepositoryVisibility(session, repositoryId);
  const rowVisibility = selectRepositoryVisibility(metadata, selection, repoVisibility, repositoryId);
  const row = rowVisibility.row;
  const accessLost = query.isSuccess && overview === null;
  const accessRejected = query.error instanceof RepositoryAccessUnverifiedError || accessLost;
  const accessRetryAt = query.error instanceof RepositoryAccessUnverifiedError ? query.error.retryAt : null;
  const retryAt = accessRetryAt ?? (rowVisibility.availability === "unverified" ? rowVisibility.retryAt : null);
  const retryTime = retryAt ? Date.parse(retryAt) : NaN;
  const rateLimited = Number.isFinite(retryTime);
  const refetchOverview = query.refetch;
  useEffect(() => {
    const delay = retryTime - Date.now();
    if (!accessRejected || !Number.isFinite(delay)) return;
    const timer = window.setTimeout(() => {
      void refetchOverview();
      void queryClient.invalidateQueries({ queryKey: selectedRepositoriesKey(session) });
    }, Math.max(1000, delay + 1000));
    return () => window.clearTimeout(timer);
  }, [accessRejected, queryClient, refetchOverview, retryTime, session]);
  const sync: SyncInput | null = overview ? {
    syncState: overview.syncState,
    activity: demo ? null : overview.activity,
    metadataStatus: row?.repo.metadataStatus ?? null,
    retryAt: row?.detail?.state === "available" ? row.detail.issues.retryAt : retryAt,
  } : null;
  const display = sync ? syncDisplay(sync) : null;

  const unavailable = !snapshot && (accessRejected || rowVisibility.availability === "unverified" || rowVisibility.availability === "denied");

  return { query, overview, snapshot, snapshotReason, selection, row, rowVisibility, retryAt, rateLimited, sync, display, unavailable };
}

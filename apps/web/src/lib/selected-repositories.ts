import { useSyncExternalStore } from "react";
import { useInfiniteQuery, useQueryClient, type InfiniteData, type QueryClient } from "@tanstack/react-query";
import {
  GraphQLUnavailableError,
  RepositoryAccessUnverifiedError,
  fetchRepositoryDetails,
  fetchSelectedRepositoryDetailsPage,
  fetchSelectedRepositoryMetadataPage,
  type CrossRepositoryOverview,
  type MetricWindow,
  type RepositoryDetail,
  type SelectedRepository,
  type SelectedRepositoryBatch,
  type SelectedRepositoryPage,
  type ViewerSession,
} from "@/lib/api";
import { clearRepositoryIssueDrafts } from "@/lib/drafts";
import { repositorySnapshotKey, repositorySnapshotPrefix, repositorySnapshotsKey } from "@/lib/repository-snapshots";

// Exact key for selection pages with details. Invalidate by prefix to refresh the viewer's metadata pages too.
export const selectedRepositoriesKey = (session: ViewerSession) => ["selectedRepositories", session.mode, session.user?.login] as const;

export function viewerKeyOf(session: ViewerSession): string {
  return `${session.mode}:${session.user?.login ?? ""}`;
}

export type RepositoryAccessOutcome =
  | { status: "verified"; seq: number }
  | { status: "unverified"; retryAt: string | null; seq: number }
  | { status: "denied"; seq: number };

export type ViewerVisibility = ReadonlyMap<string, RepositoryAccessOutcome>;

type SelectionPageWithSeq = SelectedRepositoryPage & { startedAtSeq: number };
export type SelectedBatch = SelectedRepositoryBatch;
export type SelectedRow = { repo: SelectedRepository; detail: RepositoryDetail | undefined };

export type RowAvailability = "available" | "loading" | "detail-failed" | "unverified" | "denied";

export type VisibleSelectedRow = {
  repo: SelectedRepository;
  detail: RepositoryDetail | undefined;
  availability: RowAvailability;
  displayName: string;
  canRevealName: boolean;
  retryAt: string | null;
  pageStartedAtSeq: number;
};

// --- Internal viewer-scoped visibility overlay and retry registry -------------------------------

let accessSeq = 0;
export function currentAccessSeq(): number {
  return accessSeq;
}
function nextAccessSeq(): number {
  accessSeq += 1;
  return accessSeq;
}

const emptyVisibility: ViewerVisibility = new Map();
const visibilityByViewer = new Map<string, Map<string, RepositoryAccessOutcome>>();
const generationByViewer = new Map<string, number>();
const retryControllersByViewer = new Map<string, Map<string, AbortController>>();
const listeners = new Set<() => void>();

function notifyVisibility() {
  for (const listener of listeners) listener();
}

function subscribeVisibility(listener: () => void) {
  listeners.add(listener);
  return () => {
    listeners.delete(listener);
  };
}

function getViewerVisibility(viewerKey: string): ViewerVisibility {
  return visibilityByViewer.get(viewerKey) ?? emptyVisibility;
}

function setViewerOutcome(viewerKey: string, repositoryId: string, outcome: RepositoryAccessOutcome) {
  const prev = visibilityByViewer.get(viewerKey);
  const next = new Map(prev);
  next.set(repositoryId, outcome);
  visibilityByViewer.set(viewerKey, next);
  notifyVisibility();
}

function bumpViewerGeneration(viewerKey: string): number {
  const next = (generationByViewer.get(viewerKey) ?? 0) + 1;
  generationByViewer.set(viewerKey, next);
  return next;
}

function currentViewerGeneration(viewerKey: string): number {
  return generationByViewer.get(viewerKey) ?? 0;
}

function abortViewerRetries(viewerKey: string) {
  const active = retryControllersByViewer.get(viewerKey);
  if (!active) return;
  for (const controller of active.values()) controller.abort();
  active.clear();
}

function batchKeyOf(batch: SelectedBatch): string {
  return batch.repositories.map(repo => repo.id).join(",");
}

// --- Query hooks --------------------------------------------------------------------------------

export function useSelectionVisibility(session: ViewerSession): ViewerVisibility {
  const key = viewerKeyOf(session);
  return useSyncExternalStore(
    subscribeVisibility,
    () => getViewerVisibility(key),
    () => emptyVisibility,
  );
}

function useSelectedRepositoryMetadataPage(session: ViewerSession) {
  const queryClient = useQueryClient();
  return (after: string | null) => queryClient.fetchQuery({
    queryKey: [...selectedRepositoriesKey(session), "metadata-page", after],
    queryFn: ({ signal }) => fetchSelectedRepositoryMetadataPage(after, signal),
  });
}

// Pages the viewer's verified selections and enriches each page with activity and issue details.
// Applies per-alias access outcomes through Selection commands inside this module.
export function useSelectedRepositories(session: ViewerSession) {
  const fetchPage = useSelectedRepositoryMetadataPage(session);
  const queryClient = useQueryClient();
  const viewerKey = viewerKeyOf(session);
  return useInfiniteQuery({
    queryKey: selectedRepositoriesKey(session),
    queryFn: async ({ pageParam, signal }): Promise<SelectionPageWithSeq> => {
      if (pageParam === null) {
        bumpViewerGeneration(viewerKey);
        abortViewerRetries(viewerKey);
      }
      const startedAtSeq = currentAccessSeq();
      const page = await fetchSelectedRepositoryDetailsPage(fetchPage(pageParam), signal);
      signal.throwIfAborted();
      for (const batch of page.batches) {
        if (!batch.details) continue;
        batch.repositories.forEach((repo, index) => {
          const detail = batch.details?.[index];
          if (!detail) return;
          applyDetailOutcome(queryClient, session, repo.id, detail, startedAtSeq);
        });
      }
      return { ...page, startedAtSeq };
    },
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

// --- Pure query selectors (zero cache or storage mutation) --------------------------------------

// Flattens the loaded pages into rows. A failed revalidation drops every cached row.
export function flattenSelectedRepositoryRows(query: ReturnType<typeof useSelectedRepositories>): SelectedRow[] {
  if (query.isRefetchError) return [];
  return (query.data?.pages ?? []).flatMap(p => p.batches.flatMap(b => b.repositories.map((repo, i) => ({ repo, detail: b.details?.[i] }))));
}

function deriveAvailability(
  detail: RepositoryDetail | undefined,
  batchFailed: boolean,
  pageStartedAtSeq: number,
  overlay: RepositoryAccessOutcome | undefined,
): { availability: RowAvailability; retryAt: string | null } {
  const overlayWins = overlay !== undefined && overlay.seq > pageStartedAtSeq;
  if (overlayWins) {
    if (overlay.status === "denied") return { availability: "denied", retryAt: null };
    if (overlay.status === "unverified") return { availability: "unverified", retryAt: overlay.retryAt };
  }
  if (detail?.state === "access-denied") return { availability: "denied", retryAt: null };
  if (detail?.state === "access-unverified") return { availability: "unverified", retryAt: detail.retryAt };
  if (detail?.state === "available") return { availability: "available", retryAt: detail.issues.retryAt ?? null };
  if (overlay?.status === "denied") return { availability: "denied", retryAt: null };
  if (overlay?.status === "unverified") return { availability: "unverified", retryAt: overlay.retryAt };
  if (batchFailed) return { availability: "detail-failed", retryAt: null };
  return { availability: "loading", retryAt: null };
}

// Pure selector merging metadata pages, detail batches, and the viewer's live visibility overlay.
export function selectVisibleRows(
  metadata: ReturnType<typeof useSelectedRepositoryMetadata>,
  selection: ReturnType<typeof useSelectedRepositories>,
  visibility: ViewerVisibility,
): VisibleSelectedRow[] {
  if (metadata.isRefetchError || selection.isRefetchError) return [];
  const detailByRepo = new Map<string, { detail: RepositoryDetail | undefined; batchFailed: boolean; startedAtSeq: number }>();
  for (const page of selection.data?.pages ?? []) {
    for (const batch of page.batches) {
      const batchFailed = batch.details === null;
      batch.repositories.forEach((repo, i) => {
        detailByRepo.set(repo.id, { detail: batch.details?.[i], batchFailed, startedAtSeq: page.startedAtSeq });
      });
    }
  }
  return (metadata.data?.pages ?? []).flatMap(page =>
    page.nodes.map(repo => {
      const entry = detailByRepo.get(repo.id);
      const pageStartedAtSeq = entry?.startedAtSeq ?? 0;
      const overlay = visibility.get(repo.id);
      const { availability, retryAt } = deriveAvailability(entry?.detail, entry?.batchFailed ?? false, pageStartedAtSeq, overlay);
      const canRevealName = availability !== "unverified" && availability !== "denied";
      const displayName = canRevealName ? (repo.fullName.split("/")[1] || "Repository") : "Repository";
      return {
        repo,
        detail: entry?.detail,
        availability,
        displayName,
        canRevealName,
        retryAt,
        pageStartedAtSeq,
      };
    }),
  );
}

// Pure selector for a single repository's live visibility (works even if the repository is outside loaded Selection pages).
export function selectRowVisibility(
  rows: readonly VisibleSelectedRow[],
  visibility: ViewerVisibility,
  repositoryId: string,
): { row: VisibleSelectedRow | undefined; availability: RowAvailability; retryAt: string | null; canRevealName: boolean } {
  const row = rows.find(item => item.repo.id === repositoryId);
  if (row) {
    return { row, availability: row.availability, retryAt: row.retryAt, canRevealName: row.canRevealName };
  }
  const overlay = visibility.get(repositoryId);
  if (overlay?.status === "denied") return { row: undefined, availability: "denied", retryAt: null, canRevealName: false };
  if (overlay?.status === "unverified") return { row: undefined, availability: "unverified", retryAt: overlay.retryAt, canRevealName: false };
  if (overlay?.status === "verified") return { row: undefined, availability: "available", retryAt: null, canRevealName: true };
  return { row: undefined, availability: "loading", retryAt: null, canRevealName: false };
}

// Pure selector computing withheld count, latest retry timestamp, and rate-limit flag across metadata, details, and overlay.
export function selectWithheldSummary(
  metadata: ReturnType<typeof useSelectedRepositoryMetadata>,
  selection: ReturnType<typeof useSelectedRepositories>,
  rows: readonly VisibleSelectedRow[],
  visibility: ViewerVisibility,
): { withheldCount: number; retryAt: string | null; retryTime: number; limited: boolean } {
  const metadataPages = metadata.isRefetchError ? [] : metadata.data?.pages ?? [];
  const selectionPages = selection.isRefetchError ? [] : selection.data?.pages ?? [];
  const withheldCount = selectionPages.reduce((sum, p) => sum + p.withheld, 0)
    || metadataPages.reduce((sum, p) => sum + p.withheld, 0);
  const retryCandidates: (string | null | undefined)[] = [
    ...metadataPages.map(page => page.retryAt),
    ...selectionPages.map(page => page.retryAt),
    ...rows.map(row => (row.availability === "unverified" ? row.retryAt : null)),
    ...[...visibility.values()].map(outcome => (outcome.status === "unverified" ? outcome.retryAt : null)),
  ];
  const retryAt = retryCandidates.filter((value): value is string => Boolean(value)).sort().at(-1) ?? null;
  const retryTime = retryAt ? Date.parse(retryAt) : NaN;
  return { withheldCount, retryAt, retryTime, limited: Number.isFinite(retryTime) };
}

// Pure selector for failed detail batches eligible for manual retry.
export function selectFailedDetailBatches(selection: ReturnType<typeof useSelectedRepositories>): SelectedBatch[] {
  if (selection.isRefetchError) return [];
  return selection.data?.pages.flatMap(page => page.batches.filter(batch => batch.details === null)) ?? [];
}

// Pure selector filtering an aggregate CrossRepositoryOverview against any newer access rejections.
// Preserves separately authorized rows (including repositories from unloaded Selection pages) while hiding
// rejected names, issue titles, links, and totals that still include a rejected repository until a fresh aggregate resolves.
export function filterAggregateByVisibility(
  aggregate: CrossRepositoryOverview,
  aggregateStartedAtSeq: number,
  visibility: ViewerVisibility,
): { data: CrossRepositoryOverview; totalsWithheld: boolean; supersededCount: number } {
  const supersededIds = new Set<string>();
  for (const [repoId, outcome] of visibility.entries()) {
    if ((outcome.status === "denied" || outcome.status === "unverified") && outcome.seq > aggregateStartedAtSeq) {
      supersededIds.add(repoId);
    }
  }
  if (supersededIds.size === 0) {
    return { data: aggregate, totalsWithheld: false, supersededCount: 0 };
  }
  const withheldCoverage = { covered: false, reason: "access_changed", through: null };
  return {
    supersededCount: supersededIds.size,
    totalsWithheld: true,
    data: {
      ...aggregate,
      openIssues: { value: null, coverage: withheldCoverage },
      reviewBacklog: { value: null, coverage: withheldCoverage },
      needsAttention: aggregate.needsAttention.filter(item => !supersededIds.has(item.repositoryId)),
      assignedToMe: aggregate.assignedToMe.filter(item => !supersededIds.has(item.repositoryId)),
      daily: aggregate.daily.map(point => ({ day: point.day, issuesOpened: null, pullRequestsMerged: null })),
    },
  };
}

// --- Explicit state-transition commands (sole mutation owner) -----------------------------------

function applyDetailOutcome(
  queryClient: QueryClient,
  session: ViewerSession,
  repositoryId: string,
  detail: RepositoryDetail,
  startedAtSeq: number,
) {
  if (detail.state === "access-denied") {
    recordAccessDenied(queryClient, session, repositoryId);
  } else if (detail.state === "access-unverified") {
    recordAccessUnverified(queryClient, session, repositoryId, detail.retryAt, { preserveSnapshot: Boolean(detail.retryAt) });
  } else {
    recordAccessVerified(session, repositoryId, startedAtSeq);
  }
}

// Records definitive unavailability for one repository: updates visibility overlay, purges all saved
// overview snapshots across metric windows, and wipes repository drafts and staged Board cards.
export function recordAccessDenied(queryClient: QueryClient, session: ViewerSession, repositoryId: string): void {
  const viewerKey = viewerKeyOf(session);
  setViewerOutcome(viewerKey, repositoryId, { status: "denied", seq: nextAccessSeq() });
  queryClient.removeQueries({ queryKey: repositorySnapshotPrefix(session, repositoryId) });
  clearRepositoryIssueDrafts(viewerKey, repositoryId);
}

// Records temporary access uncertainty for one repository: updates visibility overlay and preserves
// browser drafts. Removes saved snapshots unless preserveSnapshot is true (rate-limited with retryAt).
export function recordAccessUnverified(
  queryClient: QueryClient,
  session: ViewerSession,
  repositoryId: string,
  retryAt: string | null,
  opts: { preserveSnapshot?: boolean } = {},
): void {
  const viewerKey = viewerKeyOf(session);
  setViewerOutcome(viewerKey, repositoryId, { status: "unverified", retryAt, seq: nextAccessSeq() });
  if (!opts.preserveSnapshot) {
    queryClient.removeQueries({ queryKey: repositorySnapshotPrefix(session, repositoryId) });
  }
}

// Records verified live access for one repository unless a newer access rejection arrived after startedAtSeq.
export function recordAccessVerified(session: ViewerSession, repositoryId: string, startedAtSeq?: number): boolean {
  const viewerKey = viewerKeyOf(session);
  const existing = getViewerVisibility(viewerKey).get(repositoryId);
  if (startedAtSeq !== undefined && existing && existing.status !== "verified" && existing.seq > startedAtSeq) {
    return false;
  }
  setViewerOutcome(viewerKey, repositoryId, { status: "verified", seq: nextAccessSeq() });
  return true;
}

// Saves a live RepositoryOverview snapshot and marks access verified unless a newer rejection superseded the read.
export function recordOverviewSnapshot<T>(
  queryClient: QueryClient,
  session: ViewerSession,
  repositoryId: string,
  days: MetricWindow,
  data: T,
  startedAtSeq: number,
): void {
  if (!recordAccessVerified(session, repositoryId, startedAtSeq)) return;
  queryClient.setQueryData(repositorySnapshotKey(session, repositoryId, days), data);
}

// Routes an overview fetch error through Selection commands: preserves snapshots on temporary outage
// or rate-limited ACCESS_UNVERIFIED (with retryAt), purges snapshots otherwise, and never wipes drafts.
export function recordOverviewUnavailable(
  queryClient: QueryClient,
  session: ViewerSession,
  repositoryId: string,
  error: unknown,
): void {
  if (error instanceof RepositoryAccessUnverifiedError) {
    recordAccessUnverified(queryClient, session, repositoryId, error.retryAt, { preserveSnapshot: Boolean(error.retryAt) });
    return;
  }
  if (error instanceof GraphQLUnavailableError) {
    return;
  }
  queryClient.removeQueries({ queryKey: repositorySnapshotPrefix(session, repositoryId) });
}

// Purges saved snapshots when a user explicitly disconnects or unfollows a repository.
export function recordRepositoryRemoved(queryClient: QueryClient, session: ViewerSession, repositoryId: string): void {
  queryClient.removeQueries({ queryKey: repositorySnapshotPrefix(session, repositoryId) });
}

// Versioned, cancellable manual retry for one failed detail batch.
// Discards stale completions after batch replacement, navigation, sign-out, or newer per-repo rejections.
export async function retryDetailBatch(
  queryClient: QueryClient,
  session: ViewerSession,
  batch: SelectedBatch,
): Promise<void> {
  const viewerKey = viewerKeyOf(session);
  const batchKey = batchKeyOf(batch);
  const startGeneration = currentViewerGeneration(viewerKey);
  const startSeq = currentAccessSeq();
  const repoStartSeqs = new Map<string, number>();
  const existingVisibility = getViewerVisibility(viewerKey);
  for (const repo of batch.repositories) {
    repoStartSeqs.set(repo.id, existingVisibility.get(repo.id)?.seq ?? 0);
  }

  let controllers = retryControllersByViewer.get(viewerKey);
  if (!controllers) {
    controllers = new Map();
    retryControllersByViewer.set(viewerKey, controllers);
  }
  controllers.get(batchKey)?.abort();
  const controller = new AbortController();
  controllers.set(batchKey, controller);

  try {
    const fetchedDetails = await fetchRepositoryDetails(batch.repositories.map(repo => repo.id), controller.signal);
    if (controller.signal.aborted || currentViewerGeneration(viewerKey) !== startGeneration) {
      return;
    }
    const latestVisibility = getViewerVisibility(viewerKey);
    const mergedDetails: RepositoryDetail[] = fetchedDetails.map((detail, i) => {
      const repoId = batch.repositories[i].id;
      const latest = latestVisibility.get(repoId);
      const prevSeq = repoStartSeqs.get(repoId) ?? 0;
      // Never let a delayed retry overwrite a newer rejection that arrived while the retry was in flight.
      if (latest && latest.status !== "verified" && latest.seq > prevSeq) {
        return latest.status === "denied"
          ? { state: "access-denied" }
          : { state: "access-unverified", retryAt: latest.retryAt };
      }
      applyDetailOutcome(queryClient, session, repoId, detail, startSeq);
      return detail;
    });

    queryClient.setQueryData<InfiniteData<SelectionPageWithSeq>>(
      selectedRepositoriesKey(session),
      previous => {
        if (!previous || currentViewerGeneration(viewerKey) !== startGeneration) return previous;
        const stillPresent = previous.pages.some(page => page.batches.some(current => current === batch));
        if (!stillPresent) return previous;
        return {
          ...previous,
          pages: previous.pages.map(page => ({
            ...page,
            batches: page.batches.map(current => (current === batch ? { ...batch, details: mergedDetails } : current)),
          })),
        };
      },
    );
  } finally {
    if (controllers.get(batchKey) === controller) {
      controllers.delete(batchKey);
    }
  }
}

// Aborts any in-flight manual detail retries for the current viewer (e.g. on navigation).
export function cancelDetailRetries(session: ViewerSession): void {
  abortViewerRetries(viewerKeyOf(session));
}

// Resets all viewer-scoped Selection caches, overlay entries, and in-flight retries (e.g. on sign-out).
export function resetViewerSelection(queryClient: QueryClient, session: ViewerSession): void {
  const viewerKey = viewerKeyOf(session);
  bumpViewerGeneration(viewerKey);
  abortViewerRetries(viewerKey);
  visibilityByViewer.delete(viewerKey);
  queryClient.removeQueries({ queryKey: selectedRepositoriesKey(session) });
  queryClient.removeQueries({ queryKey: repositorySnapshotsKey(session) });
  notifyVisibility();
}

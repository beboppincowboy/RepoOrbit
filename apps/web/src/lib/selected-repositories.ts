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

export const selectedRepositoriesKey = (session: ViewerSession) => ["selectedRepositories", session.mode, session.user?.login] as const;

export const crossRepositoryOverviewKey = (session: ViewerSession) => ["crossRepositoryOverview", session.mode, session.user?.login] as const;

export function viewerKeyOf(session: ViewerSession): string {
  return `${session.mode}:${session.user?.login ?? ""}`;
}

export type RepositoryAccessOutcome =
  | { status: "verified"; seq: number }
  | { status: "unverified"; retryAt: string | null; seq: number }
  | { status: "denied"; seq: number; draftsCleared?: boolean };

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
};

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
const selectionPassByViewer = new Map<string, { ids: Set<string>; complete: boolean }>();
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

function newerRejection(
  outcome: RepositoryAccessOutcome | undefined,
  sinceSeq: number,
): Exclude<RepositoryAccessOutcome, { status: "verified" }> | null {
  return outcome && outcome.status !== "verified" && outcome.seq > sinceSeq ? outcome : null;
}

function refetchAggregateAfterRejection(queryClient: QueryClient, session: ViewerSession) {
  const filters = { queryKey: crossRepositoryOverviewKey(session) };
  // Invalidation alone reuses an in-flight first read that predates the rejection.
  void queryClient.cancelQueries(filters).then(() => queryClient.invalidateQueries(filters));
}

function newerDenial(outcome: RepositoryAccessOutcome | undefined, sinceSeq: number): boolean {
  return newerRejection(outcome, sinceSeq)?.status === "denied";
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

export function useSelectionVisibility(session: ViewerSession): ViewerVisibility {
  const key = viewerKeyOf(session);
  return useSyncExternalStore(
    subscribeVisibility,
    () => getViewerVisibility(key),
    () => emptyVisibility,
  );
}

export function useRepositoryVisibility(session: ViewerSession, repositoryId: string): RepositoryAccessOutcome | undefined {
  const key = viewerKeyOf(session);
  return useSyncExternalStore(
    subscribeVisibility,
    () => getViewerVisibility(key).get(repositoryId),
    () => undefined,
  );
}

function useSelectedRepositoryMetadataPage(session: ViewerSession) {
  const queryClient = useQueryClient();
  return (after: string | null) => queryClient.fetchQuery({
    queryKey: [...selectedRepositoriesKey(session), "metadata-page", after],
    queryFn: ({ signal }) => fetchSelectedRepositoryMetadataPage(after, signal),
  });
}

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
      const updates: { repositoryId: string; detail: RepositoryDetail }[] = [];
      for (const batch of page.batches) {
        if (!batch.details) continue;
        batch.repositories.forEach((repo, index) => {
          const detail = batch.details?.[index];
          if (detail) updates.push({ repositoryId: repo.id, detail });
        });
      }
      applyDetailOutcomes(queryClient, session, updates, startedAtSeq);
      recordSelectionPage(queryClient, session, page, pageParam === null);
      return { ...page, startedAtSeq };
    },
    initialPageParam: null as string | null,
    getNextPageParam: last => (last.hasNextPage ? last.endCursor : undefined),
  });
}

export function useSelectedRepositoryMetadata(session: ViewerSession) {
  const fetchPage = useSelectedRepositoryMetadataPage(session);
  return useInfiniteQuery({
    queryKey: [...selectedRepositoriesKey(session), "metadata"],
    queryFn: ({ pageParam }) => fetchPage(pageParam),
    initialPageParam: null as string | null,
    getNextPageParam: last => (last.hasNextPage ? last.endCursor : undefined),
  });
}

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
  const rejection = newerRejection(overlay, pageStartedAtSeq);
  if (rejection) {
    return rejection.status === "denied"
      ? { availability: "denied", retryAt: null }
      : { availability: "unverified", retryAt: rejection.retryAt };
  }
  if (detail?.state === "access-denied") return { availability: "denied", retryAt: null };
  if (detail?.state === "access-unverified") return { availability: "unverified", retryAt: detail.retryAt };
  if (detail?.state === "available") return { availability: "available", retryAt: detail.issues.retryAt ?? null };
  if (overlay?.status === "denied") return { availability: "denied", retryAt: null };
  if (overlay?.status === "unverified") return { availability: "unverified", retryAt: overlay.retryAt };
  if (batchFailed) return { availability: "detail-failed", retryAt: null };
  return { availability: "loading", retryAt: null };
}

function buildVisibleRow(
  repo: SelectedRepository,
  entry: { detail: RepositoryDetail | undefined; batchFailed: boolean; startedAtSeq: number } | undefined,
  overlay: RepositoryAccessOutcome | undefined,
): VisibleSelectedRow {
  const pageStartedAtSeq = entry?.startedAtSeq ?? 0;
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
  };
}

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
    page.nodes.map(repo => buildVisibleRow(repo, detailByRepo.get(repo.id), visibility.get(repo.id))),
  );
}

function outcomeVisibility(overlay: RepositoryAccessOutcome | undefined): {
  row: undefined;
  availability: RowAvailability;
  retryAt: string | null;
  canRevealName: boolean;
} {
  if (overlay?.status === "denied") return { row: undefined, availability: "denied", retryAt: null, canRevealName: false };
  if (overlay?.status === "unverified") return { row: undefined, availability: "unverified", retryAt: overlay.retryAt, canRevealName: false };
  if (overlay?.status === "verified") return { row: undefined, availability: "available", retryAt: null, canRevealName: true };
  return { row: undefined, availability: "loading", retryAt: null, canRevealName: false };
}

export function selectRepositoryVisibility(
  metadata: ReturnType<typeof useSelectedRepositoryMetadata>,
  selection: ReturnType<typeof useSelectedRepositories>,
  overlay: RepositoryAccessOutcome | undefined,
  repositoryId: string,
): { row: VisibleSelectedRow | undefined; availability: RowAvailability; retryAt: string | null; canRevealName: boolean } {
  if (!metadata.isRefetchError && !selection.isRefetchError) {
    let targetRepo: SelectedRepository | undefined;
    for (const page of metadata.data?.pages ?? []) {
      const found = page.nodes.find(node => node.id === repositoryId);
      if (found) {
        targetRepo = found;
        break;
      }
    }
    if (targetRepo) {
      let entry: { detail: RepositoryDetail | undefined; batchFailed: boolean; startedAtSeq: number } | undefined;
      for (const page of selection.data?.pages ?? []) {
        for (const batch of page.batches) {
          const index = batch.repositories.findIndex(repo => repo.id === repositoryId);
          if (index !== -1) {
            entry = { detail: batch.details?.[index], batchFailed: batch.details === null, startedAtSeq: page.startedAtSeq };
          }
        }
      }
      const row = buildVisibleRow(targetRepo, entry, overlay);
      return { row, availability: row.availability, retryAt: row.retryAt, canRevealName: row.canRevealName };
    }
  }
  return outcomeVisibility(overlay);
}

export function selectWithheldSummary(
  metadata: ReturnType<typeof useSelectedRepositoryMetadata>,
  selection: ReturnType<typeof useSelectedRepositories>,
  rows: readonly VisibleSelectedRow[],
  visibility: ViewerVisibility,
): { withheldCount: number; retryAt: string | null; retryTime: number; limited: boolean } {
  const metadataPages = metadata.isRefetchError ? [] : metadata.data?.pages ?? [];
  const selectionPages = selection.isRefetchError ? [] : selection.data?.pages ?? [];
  let selectionWithheld = 0;
  let retryAt: string | null = null;
  const considerRetryAt = (candidate: string | null | undefined) => {
    if (candidate && (!retryAt || candidate > retryAt)) retryAt = candidate;
  };
  for (const page of selectionPages) {
    selectionWithheld += page.withheld;
    considerRetryAt(page.retryAt);
  }
  let metadataWithheld = 0;
  for (const page of metadataPages) {
    metadataWithheld += page.withheld;
    considerRetryAt(page.retryAt);
  }
  for (const row of rows) {
    if (row.availability === "unverified") considerRetryAt(row.retryAt);
  }
  for (const outcome of visibility.values()) {
    if (outcome.status === "unverified") considerRetryAt(outcome.retryAt);
  }
  const withheldCount = selectionWithheld || metadataWithheld;
  const retryTime = retryAt ? Date.parse(retryAt) : NaN;
  return { withheldCount, retryAt, retryTime, limited: Number.isFinite(retryTime) };
}

export function selectFailedDetailBatches(selection: ReturnType<typeof useSelectedRepositories>): SelectedBatch[] {
  if (selection.isRefetchError) return [];
  return selection.data?.pages.flatMap(page => page.batches.filter(batch => batch.details === null)) ?? [];
}

export function filterAggregateByVisibility(
  aggregate: CrossRepositoryOverview,
  aggregateStartedAtSeq: number,
  visibility: ViewerVisibility,
): CrossRepositoryOverview {
  const supersededIds = new Set<string>();
  for (const [repoId, outcome] of visibility.entries()) {
    if (newerRejection(outcome, aggregateStartedAtSeq)) {
      supersededIds.add(repoId);
    }
  }
  if (supersededIds.size === 0) {
    return aggregate;
  }
  const withheldCoverage = { covered: false, reason: "access_changed", through: null };
  return {
    ...aggregate,
    openIssues: { value: null, coverage: withheldCoverage },
    reviewBacklog: { value: null, coverage: withheldCoverage },
    needsAttention: aggregate.needsAttention.filter(item => !supersededIds.has(item.repositoryId)),
    assignedToMe: aggregate.assignedToMe.filter(item => !supersededIds.has(item.repositoryId)),
    daily: aggregate.daily.map(point => ({ day: point.day, issuesOpened: null, pullRequestsMerged: null })),
  };
}

type OutcomeTransition =
  | { status: "denied" }
  | { status: "unverified"; retryAt: string | null; preserveSnapshot: boolean }
  | { status: "verified" };

function applyOutcomeEntry(
  queryClient: QueryClient | undefined,
  session: ViewerSession,
  viewerKey: string,
  existing: RepositoryAccessOutcome | undefined,
  repositoryId: string,
  transition: OutcomeTransition,
  startedAtSeq?: number,
): { outcome?: RepositoryAccessOutcome; rejected: boolean; accepted: boolean } {
  if (transition.status === "denied") {
    if (existing?.status === "denied" && existing.draftsCleared) {
      // seq feeds only staleness checks, never rendering: advance it so reads started before this denial still
      // lose, without a new Map or a re-render.
      existing.seq = nextAccessSeq();
      return { rejected: false, accepted: true };
    }
    queryClient?.removeQueries({ queryKey: repositorySnapshotPrefix(session, repositoryId) });
    void clearRepositoryIssueDrafts(viewerKey, repositoryId);
    return { outcome: { status: "denied", seq: nextAccessSeq(), draftsCleared: true }, rejected: true, accepted: true };
  }
  if (transition.status === "unverified") {
    if (startedAtSeq !== undefined && newerDenial(existing, startedAtSeq)) {
      return { rejected: false, accepted: false };
    }
    if (!transition.preserveSnapshot) {
      queryClient?.removeQueries({ queryKey: repositorySnapshotPrefix(session, repositoryId) });
    }
    if (existing?.status === "unverified" && existing.retryAt === transition.retryAt) {
      existing.seq = nextAccessSeq();
      return { rejected: false, accepted: true };
    }
    return { outcome: { status: "unverified", retryAt: transition.retryAt, seq: nextAccessSeq() }, rejected: true, accepted: true };
  }
  if (startedAtSeq !== undefined && newerRejection(existing, startedAtSeq)) {
    return { rejected: false, accepted: false };
  }
  if (existing?.status === "verified") {
    return { rejected: false, accepted: true };
  }
  return { outcome: { status: "verified", seq: nextAccessSeq() }, rejected: false, accepted: true };
}

function applyDetailOutcomes(
  queryClient: QueryClient,
  session: ViewerSession,
  updates: readonly { repositoryId: string; detail: RepositoryDetail }[],
  startedAtSeq: number,
) {
  if (updates.length === 0) return;
  const viewerKey = viewerKeyOf(session);
  const current = getViewerVisibility(viewerKey);
  let next: Map<string, RepositoryAccessOutcome> | null = null;
  let rejected = false;
  for (const { repositoryId, detail } of updates) {
    const existing = (next ?? current).get(repositoryId);
    const transition: OutcomeTransition =
      detail.state === "access-denied" ? { status: "denied" }
        : detail.state === "access-unverified" ? { status: "unverified", retryAt: detail.retryAt, preserveSnapshot: Boolean(detail.retryAt) }
          : { status: "verified" };
    const result = applyOutcomeEntry(queryClient, session, viewerKey, existing, repositoryId, transition, startedAtSeq);
    if (result.outcome) {
      next ??= new Map(current);
      next.set(repositoryId, result.outcome);
    }
    if (result.rejected) rejected = true;
  }
  if (next) {
    visibilityByViewer.set(viewerKey, next);
    notifyVisibility();
  }
  if (rejected) refetchAggregateAfterRejection(queryClient, session);
}

function recordSingleOutcome(
  queryClient: QueryClient | undefined,
  session: ViewerSession,
  repositoryId: string,
  transition: OutcomeTransition,
  startedAtSeq?: number,
): boolean {
  const viewerKey = viewerKeyOf(session);
  const result = applyOutcomeEntry(
    queryClient,
    session,
    viewerKey,
    getViewerVisibility(viewerKey).get(repositoryId),
    repositoryId,
    transition,
    startedAtSeq,
  );
  if (result.outcome) setViewerOutcome(viewerKey, repositoryId, result.outcome);
  if (result.rejected && queryClient) refetchAggregateAfterRejection(queryClient, session);
  return result.accepted;
}

export function recordAccessDenied(queryClient: QueryClient, session: ViewerSession, repositoryId: string): void {
  recordSingleOutcome(queryClient, session, repositoryId, { status: "denied" });
}

export function recordAccessUnverified(
  queryClient: QueryClient,
  session: ViewerSession,
  repositoryId: string,
  retryAt: string | null,
  opts: { preserveSnapshot?: boolean; startedAtSeq?: number } = {},
): void {
  recordSingleOutcome(
    queryClient,
    session,
    repositoryId,
    { status: "unverified", retryAt, preserveSnapshot: Boolean(opts.preserveSnapshot) },
    opts.startedAtSeq,
  );
}

export function recordAccessVerified(session: ViewerSession, repositoryId: string, startedAtSeq?: number): boolean {
  return recordSingleOutcome(undefined, session, repositoryId, { status: "verified" }, startedAtSeq);
}

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

export function recordOverviewUnavailable(
  queryClient: QueryClient,
  session: ViewerSession,
  repositoryId: string,
  error: unknown,
  startedAtSeq: number,
): void {
  if (error instanceof RepositoryAccessUnverifiedError) {
    recordAccessUnverified(queryClient, session, repositoryId, error.retryAt, { preserveSnapshot: Boolean(error.retryAt), startedAtSeq });
    return;
  }
  if (error instanceof GraphQLUnavailableError) {
    return;
  }
  queryClient.removeQueries({ queryKey: repositorySnapshotPrefix(session, repositoryId) });
}

export function recordRepositoriesRemoved(queryClient: QueryClient, session: ViewerSession, repositoryIds: Iterable<string>): void {
  recordOutOfSelection(queryClient, session, repositoryIds);
}

export function recordRepositoryRemoved(queryClient: QueryClient, session: ViewerSession, repositoryId: string): void {
  recordRepositoriesRemoved(queryClient, session, [repositoryId]);
}

function recordOutOfSelection(queryClient: QueryClient, session: ViewerSession, repositoryIds: Iterable<string>) {
  const viewerKey = viewerKeyOf(session);
  const current = getViewerVisibility(viewerKey);
  let next: Map<string, RepositoryAccessOutcome> | null = null;
  for (const repositoryId of repositoryIds) {
    queryClient.removeQueries({ queryKey: repositorySnapshotPrefix(session, repositoryId) });
    if (current.get(repositoryId)?.status !== "denied") {
      next ??= new Map(current);
      next.set(repositoryId, { status: "denied", seq: nextAccessSeq() });
    }
  }
  if (next) {
    visibilityByViewer.set(viewerKey, next);
    notifyVisibility();
    refetchAggregateAfterRejection(queryClient, session);
  }
}

function recordSelectionPage(queryClient: QueryClient, session: ViewerSession, page: SelectedRepositoryPage, startsPass: boolean) {
  const viewerKey = viewerKeyOf(session);
  const pass = startsPass ? { ids: new Set<string>(), complete: true } : selectionPassByViewer.get(viewerKey);
  if (!pass) return;
  for (const batch of page.batches) for (const repo of batch.repositories) pass.ids.add(repo.id);
  pass.complete &&= page.withheld === 0 && !page.retryAt;
  selectionPassByViewer.set(viewerKey, pass);
  if (page.hasNextPage) return;
  selectionPassByViewer.delete(viewerKey);
  if (!pass.complete) return;
  const absent = new Set<string>();
  for (const [key] of queryClient.getQueriesData({ queryKey: repositorySnapshotsKey(session) })) {
    if (typeof key[3] === "string" && !pass.ids.has(key[3])) absent.add(key[3]);
  }
  for (const [repositoryId, outcome] of getViewerVisibility(viewerKey)) {
    if (outcome.status !== "denied" && !pass.ids.has(repositoryId)) absent.add(repositoryId);
  }
  if (absent.size > 0) recordOutOfSelection(queryClient, session, absent);
}

export async function retryDetailBatch(
  queryClient: QueryClient,
  session: ViewerSession,
  batch: SelectedBatch,
): Promise<void> {
  const viewerKey = viewerKeyOf(session);
  const batchKey = batchKeyOf(batch);
  const startGeneration = currentViewerGeneration(viewerKey);
  const startSeq = currentAccessSeq();

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
    const updates: { repositoryId: string; detail: RepositoryDetail }[] = [];
    const mergedDetails: RepositoryDetail[] = fetchedDetails.map((detail, i) => {
      const repoId = batch.repositories[i].id;
      const latest = latestVisibility.get(repoId);
      const rejection = newerRejection(latest, startSeq);
      if (rejection) {
        return rejection.status === "denied"
          ? { state: "access-denied" }
          : { state: "access-unverified", retryAt: rejection.retryAt };
      }
      updates.push({ repositoryId: repoId, detail });
      return detail;
    });
    applyDetailOutcomes(queryClient, session, updates, startSeq);

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
  } catch (error) {
    if (controller.signal.aborted || currentViewerGeneration(viewerKey) !== startGeneration) return;
    throw error;
  } finally {
    if (controllers.get(batchKey) === controller) {
      controllers.delete(batchKey);
    }
  }
}

export function cancelDetailRetries(session: ViewerSession): void {
  abortViewerRetries(viewerKeyOf(session));
}

export function resetViewerSelection(queryClient: QueryClient, session: ViewerSession): void {
  const viewerKey = viewerKeyOf(session);
  bumpViewerGeneration(viewerKey);
  abortViewerRetries(viewerKey);
  visibilityByViewer.delete(viewerKey);
  selectionPassByViewer.delete(viewerKey);
  queryClient.removeQueries({ queryKey: selectedRepositoriesKey(session) });
  queryClient.removeQueries({ queryKey: repositorySnapshotsKey(session) });
  notifyVisibility();
}

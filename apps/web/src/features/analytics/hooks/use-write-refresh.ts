import { useQueryClient } from "@tanstack/react-query";
import type { WriteOperation } from "@/features/issues/api";
import { workspaceIssuesKey, writeOperationsQueryKey } from "@/features/issues/query-keys";
import { viewerKeyOf } from "@/features/repositories/access-store";
import { crossRepositoryOverviewKey, repositoryOverviewPrefix } from "@/features/repositories/query-keys";
import type { ViewerSession } from "@/shared/api/transport";

// Invalidates the reads an issue write makes stale: write history, the issue list and both overviews.
export function useWriteRefresh(session: ViewerSession, repositoryId: string, onWriteRecorded: () => void) {
  const queryClient = useQueryClient();
  const viewerKey = viewerKeyOf(session);
  function handleWriteRecorded(op: WriteOperation) {
    onWriteRecorded();
    void queryClient.invalidateQueries({ queryKey: writeOperationsQueryKey(viewerKey, repositoryId) });
    if (op.state === "succeeded" || op.issueNumber) refreshIssueReads();
  }

  function refreshIssueReads() {
    void queryClient.invalidateQueries({ queryKey: workspaceIssuesKey(repositoryId, viewerKey) });
    void queryClient.invalidateQueries({ queryKey: repositoryOverviewPrefix(session, repositoryId) });
    void queryClient.invalidateQueries({ queryKey: crossRepositoryOverviewKey(session) });
  }

  function handleBoardWrite() {
    void queryClient.invalidateQueries({ queryKey: writeOperationsQueryKey(viewerKey, repositoryId) });
    refreshIssueReads();
  }

  return { handleWriteRecorded, handleBoardWrite };
}

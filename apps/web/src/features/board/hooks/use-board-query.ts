import { useEffect, type Dispatch, type SetStateAction } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { fetchRepositoryBoard } from "@/features/board/api";
import type { BoardCard } from "@/features/board/model";
import { repositoryBoardKey } from "@/features/board/query-keys";
import { fetchStatusLabels } from "@/features/issues/api";
import { statusLabelsQueryKey } from "@/features/issues/query-keys";
import { currentAccessSeq, recordAccessDenied, recordAccessUnverified, recordAccessVerified, useRepositoryVisibility, viewerKeyOf } from "@/features/repositories/access-store";
import { GraphQLUnavailableError, RepositoryAccessUnverifiedError } from "@/shared/api/errors";
import type { ViewerSession } from "@/shared/api/transport";

type BoardQueryOptions = {
  session: ViewerSession;
  repositoryId: string;
  // The opened issue and the staged cards are read with each fetch, so the Board includes them. Neither is in the key.
  openIssue: number | undefined;
  cards: BoardCard[];
  setCards: Dispatch<SetStateAction<BoardCard[]>>;
};

// Reads the Board and records what the read says about repository access; losing access also clears the cards. board is
// null while the read is pending or failed, and while access is denied or unverified.
export function useBoardQuery({ session, repositoryId, openIssue, cards, setCards }: BoardQueryOptions) {
  const viewerKey = viewerKeyOf(session);
  const queryClient = useQueryClient();
  const repoVisibility = useRepositoryVisibility(session, repositoryId);
  const query = useQuery({
    queryKey: repositoryBoardKey(viewerKey, repositoryId),
    queryFn: async ({ signal }) => {
      const startedAtSeq = currentAccessSeq();
      try {
        const result = await fetchRepositoryBoard(
          repositoryId,
          [...new Set([...(openIssue ? [openIssue] : []), ...cards.map(card => card.number)])].slice(0, 100),
          signal,
        );
        signal.throwIfAborted();
        if (result === null) {
          recordAccessDenied(queryClient, session, repositoryId);
          setCards([]);
        } else {
          recordAccessVerified(session, repositoryId, startedAtSeq);
        }
        return result;
      } catch (error) {
        if (!signal.aborted && error instanceof RepositoryAccessUnverifiedError) {
          recordAccessUnverified(queryClient, session, repositoryId, error.retryAt, { preserveSnapshot: Boolean(error.retryAt), startedAtSeq });
        }
        throw error;
      }
    },
    gcTime: 0,
    retry: (count, error) => !(error instanceof RepositoryAccessUnverifiedError || error instanceof GraphQLUnavailableError) && count < 1,
  });
  const accessBlocked = repoVisibility?.status === "denied" || repoVisibility?.status === "unverified";
  const board = query.isError || query.isPending || accessBlocked ? null : (query.data ?? null);
  // The status-labels setting as last fetched elsewhere; this observer never fetches it.
  const labelsSetting = useQuery({ queryKey: statusLabelsQueryKey(viewerKey, repositoryId), queryFn: ({ signal }) => fetchStatusLabels(repositoryId, signal), enabled: false, gcTime: 0 }).data?.enabled;
  return { query, board, labelsSetting };
}

// Rereads the Board when the status-labels setting, as last fetched elsewhere, differs from what the Board was read with.
export function useStatusLabelRefetch(labelsSetting: boolean | undefined, boardLabels: boolean | undefined, refetchBoard: () => unknown) {
  useEffect(() => {
    if (labelsSetting !== undefined && boardLabels !== undefined && labelsSetting !== boardLabels) void refetchBoard();
  }, [labelsSetting, boardLabels, refetchBoard]);
}

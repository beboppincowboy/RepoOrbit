import type { BoardStatus } from "@/features/issues/api";
import { guardedGraphql } from "@/shared/api/transport";

// status is null when the repository's status labels are off. multipleStatus flags an open issue carrying more than one
// status label. updatedAt is RFC 3339.
export type BoardIssue = { number: number; title: string; url: string; state: "open" | "closed"; updatedAt: string; authorLogin: string | null; labels: string[]; assignees: string[]; status: BoardStatus | null; multipleStatus: boolean };
// The open issues (500 most recently updated), closed issues (50 most recently closed) and the included numbers, each once.
// truncated is true when a cap left issues out.
export type RepositoryBoard = { statusLabels: boolean; truncated: boolean; issues: BoardIssue[] };

// Fetches the Board snapshot, adding the issues numbered in include (at most 100, for staged cards and an opened issue).
// Null means the repo is missing, unselected, or denied. Unverified access and temporary transport failures reject
// with their typed errors.
export async function fetchRepositoryBoard(id: string, include: number[], signal?: AbortSignal): Promise<RepositoryBoard | null> {
  const data = await guardedGraphql<{ repositoryBoard: RepositoryBoard | null }>(`query RepositoryBoard($id: ID!, $include: [Int!]) {
    repositoryBoard(id: $id, include: $include) {
      statusLabels truncated
      issues { number title url state updatedAt authorLogin labels assignees status multipleStatus }
    }
  }`, { id, include }, "Could not load the board", signal);
  return data.repositoryBoard;
}

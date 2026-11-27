// TanStack Query key of one viewer's Board read of a repository.
export const repositoryBoardKey = (viewerKey: string, repositoryId: string) => ["repositoryBoard", viewerKey, repositoryId] as const;

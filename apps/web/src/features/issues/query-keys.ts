// TanStack Query keys for issue reads and writes. Prefix invalidation depends on each key's element order, which
// differs between keys: workspaceIssues leads with the repository, the others with the viewer.
export const writeOperationsQueryKey = (viewerKey: string, repositoryId: string) => ["writeOperations", viewerKey, repositoryId] as const;

export const statusLabelsQueryKey = (viewerKey: string, repositoryId: string) => ["statusLabels", viewerKey, repositoryId] as const;

export const workspaceIssuesKey = (repositoryId: string, viewerKey: string) => ["workspaceIssues", repositoryId, viewerKey] as const;

// issueNumber is null while no single issue is being restored.
export const workspaceIssueKey = (viewerKey: string, repositoryId: string, issueNumber: number | null) =>
  ["workspaceIssue", viewerKey, repositoryId, issueNumber] as const;

export const issueCapabilitiesKey = (viewerKey: string, repositoryId: string) => ["issueCapabilities", viewerKey, repositoryId] as const;

export const assignableUsersKey = (viewerKey: string, repositoryId: string) => ["assignableUsers", viewerKey, repositoryId] as const;

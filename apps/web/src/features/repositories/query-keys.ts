import type { MetricWindow, ViewerSession } from "@/shared/api/transport";

// TanStack Query keys for repository reads. Prefix invalidation depends on each key's element order.

// The viewer's selected-repository reads. The paged detail and metadata reads extend this prefix.
export const selectedRepositoriesKey = (session: ViewerSession) => ["selectedRepositories", session.mode, session.user?.login] as const;

// Matches the viewer's cross-repository overview in every metric window.
export const crossRepositoryOverviewKey = (session: ViewerSession) => ["crossRepositoryOverview", session.mode, session.user?.login] as const;

// The signed-in user's owned and starred repository lists. Neither is scoped by viewer.
export const ownedRepositoriesKey = ["owned"] as const;
export const starredRepositoriesKey = ["starred"] as const;

// Matches the live overview of one repository in every metric window.
export const repositoryOverviewPrefix = (session: ViewerSession, repositoryId: string) =>
  ["repositoryOverview", session.mode, session.user?.login, repositoryId] as const;

// The live overview of one repository and metric window. The workspace route's placeholder guard reads positions 1 to 3.
export const repositoryOverviewKey = (session: ViewerSession, repositoryId: string, days: MetricWindow) =>
  [...repositoryOverviewPrefix(session, repositoryId), days] as const;

export const workspacePullRequestsKey = (repositoryId: string, viewerKey: string) => ["workspacePullRequests", repositoryId, viewerKey] as const;

// Matches saved overviews for one viewer across repositories and metric windows. The cache is in memory.
export const repositorySnapshotsKey = (session: ViewerSession) => ["repositorySnapshot", session.mode, session.user?.login] as const;

// Matches every saved metric window for a repository. Use this prefix to remove snapshots on access loss.
export const repositorySnapshotPrefix = (session: ViewerSession, repositoryId: string) =>
  [...repositorySnapshotsKey(session), repositoryId] as const;

// Identifies the last successful overview for exactly one viewer, repository, and metric window.
export const repositorySnapshotKey = (session: ViewerSession, repositoryId: string, days: MetricWindow) =>
  [...repositorySnapshotPrefix(session, repositoryId), days] as const;

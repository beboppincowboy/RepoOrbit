import type { MetricWindow, ViewerSession } from "@/shared/api/transport";

// Matches saved overviews for one viewer across repositories and metric windows. The cache is in memory.
export const repositorySnapshotsKey = (session: ViewerSession) => ["repositorySnapshot", session.mode, session.user?.login] as const;

// Matches every saved metric window for a repository. Use this prefix to remove snapshots on access loss.
export const repositorySnapshotPrefix = (session: ViewerSession, repositoryId: string) =>
  [...repositorySnapshotsKey(session), repositoryId] as const;

// Identifies the last successful overview for exactly one viewer, repository, and metric window.
export const repositorySnapshotKey = (session: ViewerSession, repositoryId: string, days: MetricWindow) =>
  [...repositorySnapshotPrefix(session, repositoryId), days] as const;

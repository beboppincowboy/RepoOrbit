import { removeStorage, storageKeys } from "@/shared/utils/storage";

const DRAFT_PREFIX = "repoorbit:issue-draft:";

// Take the per-repository withBoardLock first when needed. Work here is synchronous and never waits for it or the network.
// Run unlocked if Web Locks is unavailable or the request fails before work starts. Once work starts, propagate errors.
export async function withDraftStorageLock<T>(work: () => T): Promise<T> {
  const locks = globalThis.navigator?.locks;
  if (!locks) return work();
  let started = false;
  try {
    return await locks.request("repoorbit:issue-draft-storage", () => {
      started = true;
      return work();
    });
  } catch (error) {
    if (started) throw error;
    return work();
  }
}

function repositoryDraftPrefix(viewerKey: string, repositoryId: string): string {
  return `${DRAFT_PREFIX}${viewerKey}:${repositoryId}:`;
}

export function boardDraftPrefix(viewerKey: string, repositoryId: string): string {
  return `${repositoryDraftPrefix(viewerKey, repositoryId)}board:`;
}

// target: "create" is the new-issue draft, a number is the editor's update draft for that issue, and { board } is that
// issue's staged Board card.
export function issueDraftStorageKey(viewerKey: string, repositoryId: string, target: "create" | number | { board: number }): string {
  if (target === "create") return `${repositoryDraftPrefix(viewerKey, repositoryId)}create`;
  if (typeof target === "number") return `${repositoryDraftPrefix(viewerKey, repositoryId)}update:${target}`;
  return `${boardDraftPrefix(viewerKey, repositoryId)}${target.board}`;
}

export function clearRepositoryIssueDrafts(viewerKey: string, repositoryId: string): Promise<void> {
  return withDraftStorageLock(() => {
    for (const key of storageKeys(repositoryDraftPrefix(viewerKey, repositoryId))) removeStorage(key);
  });
}

export function clearAllIssueDrafts(): Promise<void> {
  return withDraftStorageLock(() => {
    for (const key of storageKeys(DRAFT_PREFIX)) removeStorage(key);
  });
}

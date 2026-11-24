// Browser-local issue drafts (U6b) and staged Board cards (U6d), one localStorage entry each, all under one prefix
// per viewer so logout and confirmed access loss can wipe them. Every helper no-ops without window or storage access.
const DRAFT_PREFIX = "repoorbit:issue-draft:";

function repositoryDraftPrefix(viewerKey: string, repositoryId: string): string {
  return `${DRAFT_PREFIX}${viewerKey}:${repositoryId}:`;
}

// Every staged Board card key of one viewer and repository starts with this.
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

function storageKeys(prefix: string): string[] {
  if (typeof window === "undefined") return [];
  try {
    return Object.keys(window.localStorage).filter(key => key.startsWith(prefix));
  } catch {
    return [];
  }
}

// Removes every editor draft and staged Board card of one viewer and repository.
export function clearRepositoryIssueDrafts(viewerKey: string, repositoryId: string): void {
  for (const key of storageKeys(repositoryDraftPrefix(viewerKey, repositoryId))) removeStorage(key);
}

// The parsed object stored under key, or null when it is missing, not JSON or not an object.
export function readStorage<T>(key: string): T | null {
  if (typeof window === "undefined") return null;
  try {
    const raw = window.localStorage.getItem(key);
    if (!raw) return null;
    const parsed = JSON.parse(raw) as T;
    return parsed && typeof parsed === "object" ? parsed : null;
  } catch {
    return null;
  }
}

// Every object readStorage finds under a key starting with prefix.
export function readStorageByPrefix<T>(prefix: string): T[] {
  return storageKeys(prefix).map(key => readStorage<T>(key)).filter(value => value !== null);
}

export function writeStorage(key: string, value: unknown): void {
  if (typeof window === "undefined") return;
  try {
    window.localStorage.setItem(key, JSON.stringify(value));
  } catch { /* ignore quota/private-mode errors */ }
}

export function removeStorage(key: string): void {
  if (typeof window === "undefined") return;
  try {
    window.localStorage.removeItem(key);
  } catch { /* ignore storage errors */ }
}

const DRAFT_PREFIX = "repoorbit:issue-draft:";
const CLEARED_KEY = "repoorbit:issue-draft-cleared";

const repositoryClearedKey = (viewerKey: string, repositoryId: string) => `${CLEARED_KEY}:${viewerKey}:${repositoryId}`;

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

function storageKeys(prefix: string): string[] {
  if (typeof window === "undefined") return [];
  try {
    return Object.keys(window.localStorage).filter(key => key.startsWith(prefix));
  } catch {
    return [];
  }
}

export function clearRepositoryIssueDrafts(viewerKey: string, repositoryId: string): void {
  markDraftsCleared(repositoryClearedKey(viewerKey, repositoryId));
  for (const key of storageKeys(repositoryDraftPrefix(viewerKey, repositoryId))) removeStorage(key);
}

export function clearAllIssueDrafts(): void {
  markDraftsCleared(CLEARED_KEY);
  for (const key of storageKeys(DRAFT_PREFIX)) removeStorage(key);
}

function markDraftsCleared(key: string): void {
  if (typeof window !== "undefined") writeStorage(key, { token: crypto.getRandomValues(new Uint32Array(4)).join("-") });
}

// The cleanup baseline is captured at call time, so callers call this before waiting for a lock.
export function watchIssueDraftCleanup(viewerKey: string, repositoryId: string): () => boolean {
  const keys = [CLEARED_KEY, repositoryClearedKey(viewerKey, repositoryId)];
  const token = (key: string) => readStorage<{ token: string }>(key)?.token;
  const baseline = keys.map(token);
  return () => keys.some((key, index) => token(key) !== baseline[index]);
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

export function readStorageByPrefix<T>(prefix: string): T[] {
  return storageKeys(prefix).map(key => readStorage<T>(key)).filter(value => value !== null);
}

export function writeStorage(key: string, value: unknown, opts: { verify?: boolean } = {}): boolean {
  if (typeof window === "undefined") return false;
  try {
    const serialized = JSON.stringify(value);
    if (serialized === undefined) return false;
    window.localStorage.setItem(key, serialized);
    return opts.verify ? window.localStorage.getItem(key) === serialized : true;
  } catch {
    return false;
  }
}

export function removeStorage(key: string): void {
  if (typeof window === "undefined") return;
  try {
    window.localStorage.removeItem(key);
  } catch { /* ignore storage errors */ }
}

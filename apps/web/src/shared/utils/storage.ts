export function storageKeys(prefix: string): string[] {
  if (typeof window === "undefined") return [];
  try {
    return Object.keys(window.localStorage).filter(key => key.startsWith(prefix));
  } catch {
    return [];
  }
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

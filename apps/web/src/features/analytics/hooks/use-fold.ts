import { useMemo, useSyncExternalStore } from "react";

const foldMemory = new Map<string, string>();
const foldEvent = "repoorbit:overview-fold";
const subscribeFold = (notify: () => void) => { window.addEventListener(foldEvent, notify); return () => window.removeEventListener(foldEvent, notify); };
export function useFold(key: string) {
  const raw = useSyncExternalStore(subscribeFold, () => {
    try { return localStorage.getItem(key) ?? foldMemory.get(key) ?? "{}"; } catch { return foldMemory.get(key) ?? "{}"; }
  }, () => "{}");
  const fold = useMemo<Record<string, boolean>>(() => { try { return JSON.parse(raw); } catch { return {}; } }, [raw]);
  const toggle = (id: string) => {
    const next = JSON.stringify({ ...fold, [id]: !fold[id] });
    foldMemory.set(key, next);
    try { localStorage.setItem(key, next); } catch { /* storage unavailable: memory keeps it for this visit */ }
    window.dispatchEvent(new Event(foldEvent));
  };
  return [fold, toggle] as const;
}

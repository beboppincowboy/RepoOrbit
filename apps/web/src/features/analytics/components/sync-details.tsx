import { useState } from "react";
import { card } from "@/features/analytics/components/workspace-parts";
import { clock } from "@/features/analytics/format";
import { describeActivity, describeMetadata, RefreshActivityButton, SyncStatus, type SyncInput } from "@/features/repositories/sync-status";

export function SyncDetails({ sync, repositoryId, onRefresh }: { sync: SyncInput; repositoryId: string; onRefresh: () => void }) {
  const [open, setOpen] = useState(false);
  const metadata = sync.metadataStatus ? describeMetadata(sync.metadataStatus) : [];
  const activity = sync.activity ? [...(sync.activity.synchronized ? [] : [sync.activity.message]), ...describeActivity(sync.activity)] : [];
  const lines = (label: string, items: string[]) => items.length > 0 && <ul aria-label={label} className="m-0 list-none space-y-1 p-0 text-[13px] text-muted-foreground">{items.map(line => <li key={line}>{line}</li>)}</ul>;
  return <div className="relative" onKeyDown={e => { if (e.key === "Escape") setOpen(false); }}>
    <button type="button" aria-expanded={open} aria-controls="sync-details" aria-label="Sync details" onClick={() => setOpen(!open)} className="flex items-center gap-1 rounded-full">
      <SyncStatus compact {...sync} /><span aria-hidden="true" className={`text-muted-foreground transition-transform ${open ? "rotate-90" : ""}`}>›</span>
    </button>
    {open && <div id="sync-details" role="region" aria-label="Sync status" className={`${card} absolute top-full left-0 z-20 mt-2 flex max-h-[60vh] w-[min(420px,80vw)] flex-col gap-2 overflow-auto p-3 shadow-[0_18px_44px_rgba(15,23,42,.16)]`}>
      {lines("Metadata status", metadata)}
      {lines("Activity status", activity)}
      {sync.retryAt && <p className="m-0 text-[13px] text-warning">GitHub rate limited the access check; next check after {clock.format(new Date(sync.retryAt))}.</p>}
      {sync.activity && <RefreshActivityButton repositoryId={repositoryId} onAccepted={onRefresh} />}
    </div>}
  </div>;
}

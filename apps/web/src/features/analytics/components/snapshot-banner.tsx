import { clock } from "@/features/analytics/format";
import { Button } from "@/shared/ui/button";

type SnapshotBannerProps = {
  savedAt: number;
  reason: string | null;
  retryAt: string | null;
  rateLimited: boolean;
  retrying: boolean;
  onRetry: () => void;
};

export function SnapshotBanner({ savedAt, reason, retryAt, rateLimited, retrying, onRetry }: SnapshotBannerProps) {
  return <div role="status" className="flex flex-none flex-wrap items-center gap-2 rounded-[10px] border border-warning bg-warning-soft px-3 py-2.5 text-[13px]">
    <strong>Showing saved data from {new Date(savedAt).toLocaleString()}.</strong>
    <span>{reason} {retryAt && `Retry after ${clock.format(new Date(retryAt))}.`}</span>
    {!rateLimited && <Button variant="outline" size="sm" onClick={onRetry} disabled={retrying}>Retry</Button>}
  </div>;
}

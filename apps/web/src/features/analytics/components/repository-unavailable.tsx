import { card, Ico, icons } from "@/features/analytics/components/workspace-parts";
import { clock } from "@/features/analytics/format";
import { LoadingCircle, SyncStatus } from "@/features/repositories/sync-status";
import { Button } from "@/shared/ui/button";

type RepositoryUnavailableProps = {
  /** GitHub access is withheld, so the repository name and data stay hidden. */
  unavailable: boolean;
  repositoryName: string | null | undefined;
  retryAt: string | null;
  rateLimited: boolean;
  loading: boolean;
  retrying: boolean;
  onRetry: () => void;
};

// The workspace when there is no overview to show: loading, failed, or access not verified.
export function RepositoryUnavailable({ unavailable, repositoryName, retryAt, rateLimited, loading, retrying, onRetry }: RepositoryUnavailableProps) {
  return <div className="flex h-full min-h-0 flex-col gap-3">
    <section className={`${card} flex flex-none flex-wrap items-center gap-3 p-4`}>
      <Ico>{icons.repo}</Ico>
      <h1 className="text-[19px] leading-[1.2]">{unavailable ? "Repository" : repositoryName || "Repository"}</h1>
      {unavailable && <SyncStatus compact syncState="awaiting-sync" withheld={1} retryAt={retryAt} />}
    </section>
    <section aria-label="Repository overview" className={`${card} p-4`}>
      {loading && !unavailable ? <p role="status" className="flex items-center gap-2"><LoadingCircle />Loading repository data…</p> : <>
        <p role="alert" className="m-0">{unavailable
          ? rateLimited && retryAt
            ? `GitHub limited access checks. RepoOrbit will retry after ${clock.format(new Date(retryAt))}.`
            : "Repository data is unavailable because GitHub access could not be verified."
          : "Repository analytics are unavailable."}</p>
        {!rateLimited && <Button className="mt-3" onClick={onRetry} disabled={retrying}>Retry</Button>}
      </>}
    </section>
  </div>;
}

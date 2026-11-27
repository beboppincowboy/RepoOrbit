import { useQuery } from "@tanstack/react-query";
import { SideDrawer } from "@/shared/ui/side-drawer";
import { fetchWorkflowRun } from "@/features/analytics/api";
import { formatDurationSeconds, githubHref, workflowStatusLabel, workflowStatusTone } from "@/shared/utils/delivery";

type RunDetailDrawerProps = {
  repositoryId: string;
  viewerKey: string;
  runId: string;
  github: string;
  onClose: () => void;
};

export function RunDetailDrawer({ repositoryId, viewerKey, runId, github, onClose }: RunDetailDrawerProps) {
  const query = useQuery({
    queryKey: ["workflowRun", viewerKey, repositoryId, runId],
    queryFn: ({ signal }) => fetchWorkflowRun(repositoryId, runId, signal),
    gcTime: 0,
  });

  const detail = query.data;
  const run = detail?.run;

  return (
    <SideDrawer
      ariaLabel="Workflow run detail"
      closeLabel="Close run detail"
      widthClass="w-[min(440px,calc(100vw-24px))]"
      eyebrow={run ? `Run #${run.runNumber} · ${run.headBranch} · ${run.headSha.slice(0, 7)}` : `Run #${runId}`}
      title={run?.workflowName ?? "Workflow run"}
      onClose={onClose}
    >
      {query.isPending && <p role="status" className="m-0 text-muted-foreground">Loading run details…</p>}
      {query.isError && <p role="alert" className="m-0 text-destructive">{query.error.message}</p>}
      {query.isSuccess && !detail && <p className="m-0 text-muted-foreground">Workflow run not found.</p>}
      {detail && run && (
        <>
          <div className="flex flex-wrap items-center gap-2">
            <span className={`inline-flex items-center rounded-full border px-2 py-0.5 text-xs font-medium ${workflowStatusTone(run.status, run.conclusion)}`}>
              {workflowStatusLabel(run.status, run.conclusion)}
            </span>
            <span className="text-xs text-muted-foreground">Duration: {formatDurationSeconds(run.durationSeconds)}</span>
            <span className="text-xs text-muted-foreground">Queue: {formatDurationSeconds(run.queueSeconds)}</span>
          </div>
          <a
            href={githubHref(run.htmlUrl, `${github}/actions/runs/${run.id}`)}
            target="_blank"
            rel="noreferrer"
            className="self-start text-primary underline"
          >
            Open run on GitHub ↗
          </a>
          <section aria-label="Jobs" className="flex flex-col gap-2">
            <h3 className="m-0 text-xs font-medium uppercase tracking-wide text-muted-foreground">Jobs ({detail.jobs.length})</h3>
            {detail.jobs.length === 0 ? (
              <p className="m-0 text-muted-foreground">No jobs recorded for this run.</p>
            ) : (
              <ul className="m-0 flex list-none flex-col divide-y divide-border rounded-lg border border-border p-0">
                {detail.jobs.map(job => (
                  <li key={job.id} className="flex flex-col gap-1.5 px-3 py-2.5">
                    <div className="flex items-center justify-between gap-2">
                      <span className="font-medium">{job.name}</span>
                      <span className={`inline-flex items-center rounded-full border px-2 py-0.5 text-[11px] font-medium ${workflowStatusTone(job.status, job.conclusion)}`}>
                        {workflowStatusLabel(job.status, job.conclusion)}
                      </span>
                    </div>
                    <div className="flex flex-wrap items-center justify-between gap-2 text-xs text-muted-foreground">
                      <span>Duration {formatDurationSeconds(job.durationSeconds)} · Queue {formatDurationSeconds(job.queueSeconds)}</span>
                      <a
                        href={githubHref(job.htmlUrl, `${github}/actions/runs/${run.id}`)}
                        target="_blank"
                        rel="noreferrer"
                        className="text-primary underline"
                      >
                        View logs on GitHub ↗
                      </a>
                    </div>
                  </li>
                ))}
              </ul>
            )}
          </section>
        </>
      )}
    </SideDrawer>
  );
}

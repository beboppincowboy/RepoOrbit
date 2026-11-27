import { useEffect, useRef } from "react";
import { useQuery } from "@tanstack/react-query";
import { Button } from "@/components/ui/button";
import { fetchWorkflowRun } from "@/lib/api";

const githubHref = (url: string, fallback: string) => url.startsWith("https://github.com/") ? url : fallback;

function formatDuration(seconds: number | null) {
  if (seconds === null) return "—";
  if (seconds < 60) return `${seconds}s`;
  const mins = Math.floor(seconds / 60);
  const rem = seconds % 60;
  return rem ? `${mins}m ${rem}s` : `${mins}m`;
}

function statusTone(status: string, conclusion: string) {
  if (status !== "completed") return "border-border bg-muted text-muted-foreground";
  if (conclusion === "success") return "border-success/30 bg-success/10 text-success";
  if (conclusion === "failure") return "border-destructive/30 bg-destructive/10 text-destructive";
  return "border-border bg-muted text-muted-foreground";
}

type RunDetailDrawerProps = {
  repositoryId: string;
  viewerKey: string;
  runId: string;
  github: string;
  onClose: () => void;
};

// Read-only Workflow run detail drawer: displays run timing, synchronized jobs, and direct GitHub log links.
// Contains no re-run or cancel controls (ADR 0004).
export function RunDetailDrawer({ repositoryId, viewerKey, runId, github, onClose }: RunDetailDrawerProps) {
  const ref = useRef<HTMLDivElement>(null);
  useEffect(() => ref.current?.focus(), []);
  useEffect(() => {
    const onKey = (event: KeyboardEvent) => { if (event.key === "Escape") onClose(); };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [onClose]);

  const query = useQuery({
    queryKey: ["workflowRun", viewerKey, repositoryId, runId],
    queryFn: ({ signal }) => fetchWorkflowRun(repositoryId, runId, signal),
    gcTime: 0,
  });

  const detail = query.data;
  const run = detail?.run;

  return (
    <div
      ref={ref}
      role="dialog"
      aria-label="Workflow run detail"
      tabIndex={-1}
      className="fixed inset-y-3 right-3 z-40 flex w-[min(440px,calc(100vw-24px))] flex-col overflow-hidden rounded-xl border border-border bg-card shadow-[0_18px_44px_rgba(15,23,42,.22)] outline-none"
    >
      <div className="flex flex-none items-start justify-between gap-2 border-b border-border p-4">
        <div className="min-w-0">
          <div className="font-mono text-xs text-muted-foreground">
            {run ? `Run #${run.runNumber} · ${run.headBranch} · ${run.headSha.slice(0, 7)}` : `Run #${runId}`}
          </div>
          <h2 className="m-0 break-words text-base">{run?.workflowName ?? "Workflow run"}</h2>
        </div>
        <Button variant="outline" size="sm" onClick={onClose} aria-label="Close run detail">Close</Button>
      </div>
      <div className="flex min-h-0 flex-1 flex-col gap-3 overflow-y-auto p-4 text-[13px]">
        {query.isPending && <p role="status" className="m-0 text-muted-foreground">Loading run details…</p>}
        {query.isError && <p role="alert" className="m-0 text-destructive">{query.error.message}</p>}
        {query.isSuccess && !detail && <p className="m-0 text-muted-foreground">Workflow run not found.</p>}
        {detail && run && (
          <>
            <div className="flex flex-wrap items-center gap-2">
              <span className={`inline-flex items-center rounded-full border px-2 py-0.5 text-xs font-medium ${statusTone(run.status, run.conclusion)}`}>
                {run.status === "completed" ? (run.conclusion || "completed") : run.status.replace("_", " ")}
              </span>
              <span className="text-xs text-muted-foreground">Duration: {formatDuration(run.durationSeconds)}</span>
              <span className="text-xs text-muted-foreground">Queue: {formatDuration(run.queueSeconds)}</span>
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
                        <span className={`inline-flex items-center rounded-full border px-2 py-0.5 text-[11px] font-medium ${statusTone(job.status, job.conclusion)}`}>
                          {job.status === "completed" ? (job.conclusion || "completed") : job.status.replace("_", " ")}
                        </span>
                      </div>
                      <div className="flex flex-wrap items-center justify-between gap-2 text-xs text-muted-foreground">
                        <span>Duration {formatDuration(job.durationSeconds)} · Queue {formatDuration(job.queueSeconds)}</span>
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
      </div>
    </div>
  );
}

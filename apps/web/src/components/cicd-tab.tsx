import { useMemo } from "react";
import { useQuery } from "@tanstack/react-query";
import { LineChart, type ChartSeries } from "@/components/line-chart";
import { Button } from "@/shared/ui/button";
import { fetchRepositoryActions, type RepositoryActions } from "@/features/analytics/api";
import type { MetricWindow } from "@/shared/api/transport";
import { deploymentUnavailableNote, formatDurationSeconds, githubHref, utcTimestampFormatter, workflowStatusLabel, workflowStatusTone } from "@/shared/utils/delivery";

const card = "rounded-xl border border-border bg-card min-w-0 min-h-0";

type CicdTabProps = {
  repositoryId: string;
  viewerKey: string;
  days: MetricWindow;
  github: string;
  onSelectRun: (runId: string) => void;
};

export function CicdTab({ repositoryId, viewerKey, days, github, onSelectRun }: CicdTabProps) {
  const query = useQuery({
    queryKey: ["repositoryActions", viewerKey, repositoryId, days],
    queryFn: ({ signal }) => fetchRepositoryActions(repositoryId, days, signal),
    gcTime: 0,
  });

  const data = query.data;
  const chartDays = useMemo(() => (data?.daily ?? []).map(d => d.day), [data]);
  const chartSeries = useMemo<ChartSeries[]>(() => [
    { name: "Succeeded", unit: "succeeded", color: "--primary", values: (data?.daily ?? []).map(d => d.succeeded) },
    { name: "Failed", unit: "failed", color: "--chart-2", values: (data?.daily ?? []).map(d => d.failed) },
  ], [data]);

  if (query.isPending) return <p role="status" className={`${card} p-4`}>Loading CI/CD metrics…</p>;
  if (query.isError) return (
    <div role="alert" className={`${card} flex items-center gap-3 p-4`}>
      <span>{query.error.message}</span>
      <Button variant="outline" size="sm" onClick={() => void query.refetch()}>Retry</Button>
    </div>
  );
  if (!data) return <p className={`${card} p-4 text-muted-foreground`}>CI/CD data is not available for this repository.</p>;

  const actionsDenied = !data.actionsCoverage.covered && data.actionsCoverage.reason === "permission_denied";

  return (
    <div className="flex flex-col gap-3">
      {actionsDenied && (
        <div role="status" className="rounded-[10px] border border-warning bg-warning-soft px-3.5 py-2.5 text-[13px]">
          <strong>CI/CD data unavailable: missing GitHub Actions read permission.</strong>{" "}
          Grant Actions read access to the RepoOrbit GitHub App installation to enable workflow metrics. Issues, pull requests, and Board remain available.
        </div>
      )}
      {!actionsDenied && !data.actionsCoverage.covered && (
        <div role="status" className="rounded-[10px] border border-border bg-card px-3.5 py-2.5 text-[13px] text-muted-foreground">
          {data.actionsCoverage.reason === "window_before_boundary"
            ? "CI/CD history does not yet cover the full selected window."
            : "Awaiting GitHub Actions synchronization."}
        </div>
      )}

      <section aria-label="CI/CD KPIs" className={`${card} grid grid-cols-[repeat(auto-fit,minmax(160px,1fr))]`}>
        <KpiCard
          label="Build success rate"
          value={data.buildSuccessRate.coverage.covered && data.buildSuccessRate.rate !== null ? `${(data.buildSuccessRate.rate * 100).toFixed(1)}%` : "—"}
          sub={data.buildSuccessRate.coverage.covered && data.buildSuccessRate.denominator !== null
            ? `${data.buildSuccessRate.numerator ?? 0} of ${data.buildSuccessRate.denominator} completed`
            : coverageNote(data.buildSuccessRate.coverage.reason)}
        />
        <KpiCard
          label="Failed & running jobs"
          value={data.failedJobs.coverage.covered ? `${data.failedJobs.value ?? 0} failed` : "—"}
          sub={data.runningJobs.coverage.covered ? `${data.runningJobs.value ?? 0} running` : coverageNote(data.runningJobs.coverage.reason)}
        />
        <KpiCard
          label="Duration p50 / p95"
          value={data.durationP50.coverage.covered ? `${formatDurationSeconds(data.durationP50.seconds)} / ${formatDurationSeconds(data.durationP95.seconds)}` : "—"}
          sub={data.durationP50.coverage.covered ? `${data.durationP50.sampleSize ?? 0} completed runs` : coverageNote(data.durationP50.coverage.reason)}
        />
        <KpiCard
          label="Queue wait p50"
          value={data.queueWaitP50.coverage.covered ? formatDurationSeconds(data.queueWaitP50.seconds) : "—"}
          sub={data.queueWaitP50.coverage.covered ? `${data.queueWaitP50.sampleSize ?? 0} started runs` : coverageNote(data.queueWaitP50.coverage.reason)}
        />
      </section>

      <div className="grid grid-cols-[minmax(0,1.2fr)_minmax(0,1fr)] gap-3 max-[900px]:grid-cols-1">
        <section aria-label="Runs over time" className={`${card} flex flex-col gap-2 p-3.5`}>
          <div className="flex items-center justify-between gap-2">
            <h2 className="m-0 text-sm">Runs over time</h2>
            <span className="text-xs text-muted-foreground">last {days} days</span>
          </div>
          <div className="h-52">
            <LineChart days={chartDays} series={chartSeries} />
          </div>
        </section>

        <section aria-label="Failures by workflow" className={`${card} flex flex-col overflow-hidden`}>
          <header className="border-b border-border px-3.5 py-2.5">
            <h2 className="m-0 text-sm">Failures by workflow</h2>
          </header>
          {data.failuresByWorkflow.length === 0 ? (
            <p className="m-0 p-4 text-[13px] text-muted-foreground">No failed workflows in this window.</p>
          ) : (
            <ul className="m-0 list-none divide-y divide-border p-0 text-[13px]">
              {data.failuresByWorkflow.map(wf => (
                <li key={wf.workflowId} className="flex items-center justify-between gap-2 px-3.5 py-2.5">
                  <button
                    type="button"
                    onClick={() => onSelectRun(wf.latestRunId)}
                    className="min-w-0 flex-1 text-left font-medium hover:underline"
                  >
                    {wf.workflowName}
                  </button>
                  <span className="shrink-0 text-xs text-muted-foreground">{wf.failedRuns} / {wf.totalRuns} failed</span>
                </li>
              ))}
            </ul>
          )}
        </section>
      </div>

      <div className="grid grid-cols-[minmax(0,1.2fr)_minmax(0,1fr)] gap-3 max-[900px]:grid-cols-1">
        <section aria-label="Recent workflow runs" className={`${card} flex flex-col overflow-hidden`}>
          <header className="border-b border-border px-3.5 py-2.5">
            <h2 className="m-0 text-sm">Recent workflow runs</h2>
          </header>
          {data.recentRuns.length === 0 ? (
            <p className="m-0 p-4 text-[13px] text-muted-foreground">No workflow runs in this window.</p>
          ) : (
            <ul className="m-0 list-none divide-y divide-border p-0 text-[13px]">
              {data.recentRuns.map(run => (
                <li key={run.id}>
                  <button
                    type="button"
                    onClick={() => onSelectRun(run.id)}
                    className="flex w-full items-center justify-between gap-3 px-3.5 py-2.5 text-left hover:bg-muted"
                  >
                    <div className="min-w-0 flex-1">
                      <div className="truncate font-medium">{run.workflowName} <span className="font-mono text-xs text-muted-foreground">#{run.runNumber}</span></div>
                      <div className="truncate text-xs text-muted-foreground">
                        {run.headBranch} · {run.headSha.slice(0, 7)} · {utcTimestampFormatter.format(new Date(run.createdAt))}
                      </div>
                    </div>
                    <span className={`inline-flex shrink-0 items-center rounded-full border px-2 py-0.5 text-[11px] font-medium ${workflowStatusTone(run.status, run.conclusion)}`}>
                      {workflowStatusLabel(run.status, run.conclusion)}
                    </span>
                  </button>
                </li>
              ))}
            </ul>
          )}
        </section>

        <section aria-label="Deployments" className={`${card} flex flex-col overflow-hidden`}>
          <header className="border-b border-border px-3.5 py-2.5">
            <h2 className="m-0 text-sm">Deployments</h2>
          </header>
          {data.deploymentUnavailable || data.deployments.length === 0 ? (
            <div className="p-4 text-[13px] text-muted-foreground">
              <p className="m-0 font-medium text-foreground">Deployment data unavailable</p>
              <p className="mt-1 mb-0 text-xs">
                {deploymentUnavailableNote(data.deploymentsCoverage)}
              </p>
            </div>
          ) : (
            <ul className="m-0 list-none divide-y divide-border p-0 text-[13px]">
              {data.deployments.map((dep: RepositoryActions["deployments"][number]) => (
                <li key={dep.id} className="flex items-center justify-between gap-3 px-3.5 py-2.5">
                  <div className="min-w-0 flex-1">
                    <div className="font-medium">{dep.environment} <span className="font-mono text-xs text-muted-foreground">{dep.ref} · {dep.sha.slice(0, 7)}</span></div>
                    <div className="truncate text-xs text-muted-foreground">
                      {dep.description || "GitHub Deployment"} · {utcTimestampFormatter.format(new Date(dep.createdAt))}
                    </div>
                  </div>
                  <div className="flex shrink-0 items-center gap-2">
                    <span className={`inline-flex items-center rounded-full border px-2 py-0.5 text-[11px] font-medium ${workflowStatusTone("completed", dep.state)}`}>
                      {dep.state}
                    </span>
                    <a
                      href={githubHref(dep.htmlUrl, `${github}/deployments`)}
                      target="_blank"
                      rel="noreferrer"
                      className="text-xs text-primary underline"
                    >
                      GitHub ↗
                    </a>
                  </div>
                </li>
              ))}
            </ul>
          )}
        </section>
      </div>
    </div>
  );
}

function KpiCard({ label, value, sub }: { label: string; value: string; sub: string }) {
  return (
    <div className="flex flex-col gap-0.5 border-l border-border px-4 py-3 first:border-l-0">
      <b className="num text-xl leading-tight font-semibold tracking-[-.02em]">{value}</b>
      <div className="text-[12.5px] font-medium">{label}</div>
      <div className="text-[11.5px] text-muted-foreground">{sub}</div>
    </div>
  );
}

function coverageNote(reason: string | null) {
  if (reason === "permission_denied") return "Permission denied";
  if (reason === "window_before_boundary") return "Window predates history";
  return "Awaiting data";
}

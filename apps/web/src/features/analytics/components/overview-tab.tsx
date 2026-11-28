import { useMemo, type ReactNode } from "react";
import type { RepositoryOverview } from "@/features/analytics/api";
import { coverageReason } from "@/features/analytics/components/coverage-reason";
import { LineChart, type ChartSeries } from "@/features/analytics/components/line-chart";
import { actor, entryHref, entryIcon, entryLabel } from "@/features/analytics/components/timeline";
import { card, Ico, lrow, muted } from "@/features/analytics/components/workspace-parts";
import { ago, duration } from "@/features/analytics/format";
import type { MetricWindow } from "@/shared/api/transport";

function Kpi({ label, value, coverage, detail, tone = muted }: { label: string; value: ReactNode; coverage: RepositoryOverview["openIssues"]["coverage"]; detail: ReactNode; tone?: string }) {
  const [reason, reasonTone] = coverageReason(coverage);
  return <div role="group" aria-label={label} className={`${card} px-4 py-3`}>
    <small className={`flex justify-between text-xs ${muted}`}>{label}</small>
    <b className="num block text-[26px] font-semibold leading-[1.25] tracking-[-.02em]">{coverage.covered && value !== null ? value : "—"}</b>
    <span className={`text-xs ${coverage.covered ? tone : reasonTone}`}>{coverage.covered ? detail : reason}</span>
  </div>;
}

export function OverviewTab({ overview: o, days, recent, github }: { overview: RepositoryOverview; days: MetricWindow; recent: ReactNode; github: string }) {
  const chart = useMemo(() => ({
    days: o.daily.map(d => d.day),
    series: [
      { name: "Issues opened", unit: "opened", color: "--primary", values: o.daily.map(d => d.issuesOpened) },
      { name: "PRs merged", unit: "merged", color: "--chart-2", values: o.daily.map(d => d.pullRequestsMerged) },
    ] satisfies ChartSeries[],
  }), [o.daily]);
  return <>
    <section aria-label="Repository metrics" className="grid flex-none grid-cols-[repeat(auto-fit,minmax(160px,1fr))] gap-3">
      <Kpi label="Median first response" value={duration(o.firstResponseTime.seconds)} coverage={o.firstResponseTime.coverage} detail="Issue responsiveness" tone="text-success-foreground" />
      <Kpi label="Median merge time" value={duration(o.mergeTime.seconds)} coverage={o.mergeTime.coverage} detail="Merged PRs" />
      <Kpi label="Open issues" value={o.openIssues.value} coverage={o.openIssues.coverage} detail={o.staleOpenIssues.value === null ? "" : `${o.staleOpenIssues.value} stale`} />
      <Kpi label="Merged PRs" value={o.pullRequestsMerged.value} coverage={o.pullRequestsMerged.coverage} detail={o.openPullRequests?.value != null ? `${o.openPullRequests.value} open now · last ${days} days` : `last ${days} days`} />
    </section>
    <div className="flex min-h-0 flex-1 gap-3 max-[900px]:flex-col">
      <section className={`${card} flex flex-[999_1_520px] flex-col gap-2 p-4 max-[900px]:min-h-[280px]`}>
        <div className="flex flex-wrap justify-between gap-2"><h2 className="text-sm">Opened vs merged</h2>
          <div className={`flex flex-wrap gap-3.5 text-xs ${muted}`}>{chart.series.map(s => <span key={s.name}>
            <i className="mr-[5px] inline-block size-2 rounded-full" style={{ background: `var(${s.color})` }} />{s.name}{s.values.every(v => v === null) && " (not imported yet)"}
          </span>)}</div>
        </div>
        <LineChart days={chart.days} series={chart.series} />
      </section>
      <section className={`${card} flex flex-[1_1_300px] flex-col`}>
        <div className="flex justify-between p-4 pb-1.5"><h2 className="text-sm">Recent activity</h2>{recent}</div>
        <div className="min-h-0 flex-1 overflow-auto">
          {o.timeline.length ? o.timeline.slice(0, 5).map(e => <div key={`${e.kind}-${e.sourceId}`} className={lrow}>
            <Ico>{entryIcon(e)}</Ico>
            <span className="min-w-0 flex-1"><a href={entryHref(e, github)} className="block truncate font-medium hover:underline">{entryLabel(e)}</a><span className={`text-xs ${muted}`}>{actor(e)}</span></span>
            <span className={`whitespace-nowrap text-xs ${muted}`}>{ago(e.occurredAt)}</span>
          </div>) : <p className={`p-4 ${muted}`}>No activity imported yet.</p>}
        </div>
      </section>
    </div>
  </>;
}

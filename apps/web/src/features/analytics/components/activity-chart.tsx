import { useMemo } from "react";
import type { CrossRepositoryOverview } from "@/features/analytics/api";
import { LineChart, type ChartSeries } from "@/features/analytics/components/line-chart";

export function ActivityChart({ daily }: { daily: CrossRepositoryOverview["daily"] }) {
  const days = useMemo(() => daily.map(d => d.day), [daily]);
  const series = useMemo<ChartSeries[]>(() => [
    { name: "PRs merged", unit: "PRs merged", color: "--primary", values: daily.map(d => d.pullRequestsMerged) },
    { name: "Issues opened", unit: "issues opened", color: "--chart-2", values: daily.map(d => d.issuesOpened) },
  ], [daily]);
  return <div className="flex h-full min-h-0 flex-col gap-1.5 px-3.5 pb-3">
    <span className="flex flex-wrap justify-end gap-3.5 text-xs text-muted-foreground">
      <span><i className="mr-1.5 inline-block size-2 rounded-full bg-primary" />PRs merged</span>
      <span><i className="mr-1.5 inline-block size-2 rounded-full bg-chart-2" />Issues opened</span>
    </span>
    <LineChart days={days} series={series} />
  </div>;
}

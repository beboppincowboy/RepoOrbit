import type { ReactNode } from "react";
import type { CrossRepositoryOverview } from "@/features/analytics/api";

const coverageLabels: Record<string, string> = {
  not_synchronized: "Awaiting data",
  window_before_boundary: "Window predates imported history",
  historical_gap: "Issue event history is incomplete",
  children_incomplete: "Related activity is still importing",
  access_changed: "Refreshing after access change",
  permission_denied: "Missing Actions read permission",
};

function StatTile({ icon, display, label, detail, warn }: { icon: ReactNode; display: ReactNode; label: string; detail: string; warn?: boolean }) {
  return <div className="flex flex-col gap-0.5 border-l border-border px-4 py-3 first:border-l-0">
    <span className={warn ? "text-warning" : "text-muted-foreground"}>{icon}</span>
    <b className="num text-2xl leading-tight font-semibold tracking-[-.02em]">{display}</b>
    <div className="text-[12.5px] font-medium">{label}</div>
    <div className="text-[11.5px] text-muted-foreground">{detail}</div>
  </div>;
}

function coverageFallback(reason: string | null) {
  return coverageLabels[reason ?? ""] ?? "Coverage is incomplete";
}

export function Stat({ icon, metric, label, sub, warn }: { icon: ReactNode; metric: CrossRepositoryOverview["openIssues"]; label: string; sub: string; warn?: boolean }) {
  const known = metric.value !== null;
  return <StatTile icon={icon} display={known ? metric.value : "—"} label={label} detail={known ? sub : coverageFallback(metric.coverage.reason)} warn={warn} />;
}

export function RateStat({ icon, metric, label, sub }: { icon: ReactNode; metric: CrossRepositoryOverview["buildSuccessRate"]; label: string; sub: string }) {
  const covered = metric.coverage.covered;
  const display = covered && metric.rate !== null ? `${(metric.rate * 100).toFixed(1)}%` : "—";
  const detail = covered ? (metric.denominator ? `${metric.numerator ?? 0} of ${metric.denominator} completed` : sub) : coverageFallback(metric.coverage.reason);
  return <StatTile icon={icon} display={display} label={label} detail={detail} />;
}

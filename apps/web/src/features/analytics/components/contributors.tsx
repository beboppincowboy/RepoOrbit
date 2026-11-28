import type { RepositoryOverview } from "@/features/analytics/api";
import { coverageReason } from "@/features/analytics/components/coverage-reason";
import { card, muted } from "@/features/analytics/components/workspace-parts";
import type { MetricWindow } from "@/shared/api/transport";

export function Contributors({ overview: o, days }: { overview: RepositoryOverview; days: MetricWindow }) {
  const people = o.contributors.filter(p => !p.isBot).sort((x, y) => x.login.localeCompare(y.login));
  const bots = o.contributors.filter(p => p.isBot);
  const max = Math.max(1, ...o.contributors.map(p => p.pullRequestsOpened + p.reviewsSubmitted + p.commentsCreated)) * 1.1;
  const bar = (value: number, color: string) => <span style={{ width: `${value / max * 100}%`, background: `var(${color})` }} />;
  const botText = bots.length ? bots.map(b => [`${b.login} ${b.pullRequestsOpened} PRs`, b.reviewsSubmitted ? `${b.reviewsSubmitted} rev` : "", b.commentsCreated ? `${b.commentsCreated} cmt` : ""].filter(Boolean).join(" · ")).join(", ")
    : o.activeBotContributors.value ? `${o.activeBotContributors.value} active` : "none in this window";
  return <section className={`${card} flex flex-1 flex-col gap-3 p-4`}>
    <div><h2 className="text-sm">Participation · last {days} days</h2><p className={`mt-0.5 mb-0 text-xs ${muted}`}>Alphabetical. Participation, not a productivity ranking.</p></div>
    {!o.contributorsCoverage.covered ? <p className={muted}>— · {coverageReason(o.contributorsCoverage)[0]}</p> : <div className="flex min-h-0 flex-1 flex-col gap-2.5 overflow-auto">
      {people.length ? <ul aria-label="Contributors" className="m-0 flex list-none flex-col gap-2.5 p-0">
        {people.map(p => <li key={p.login} className="grid grid-cols-[28px_110px_1fr_170px] items-center gap-2.5 max-[600px]:grid-cols-[28px_1fr]">
          <span className="inline-grid size-6 place-items-center rounded-full bg-primary-soft text-[10px] font-semibold text-primary" aria-hidden="true">{p.login.slice(0, 2).toUpperCase()}</span>
          <span className="truncate">{p.login}</span>
          <span className="flex h-2.5 overflow-hidden rounded-[5px] bg-muted max-[600px]:col-span-2" aria-hidden="true">{bar(p.pullRequestsOpened, "--primary")}{bar(p.reviewsSubmitted, "--chart-2")}{bar(p.commentsCreated, "--border-strong")}</span>
          <span className={`font-mono text-xs ${muted} max-[600px]:col-span-2`}>{p.pullRequestsOpened} PRs · {p.reviewsSubmitted} rev · {p.commentsCreated} cmt</span>
        </li>)}
      </ul> : <p className={muted}>No contributor activity in this window.</p>}
      <div className={`border-t border-dashed border-border pt-2.5 text-xs ${muted}`}>Bots counted separately: {botText}</div>
    </div>}
  </section>;
}

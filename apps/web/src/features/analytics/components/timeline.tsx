import type { RepositoryOverview } from "@/features/analytics/api";
import { Ico, icons, lrow, muted } from "@/features/analytics/components/workspace-parts";
import { day } from "@/features/analytics/format";
import { githubHref, utcTimestampFormatter as stamp } from "@/shared/utils/delivery";

type Entry = RepositoryOverview["timeline"][number];

export const entryIcon = (e: Entry) => e.kind === "pull_request_merged" ? icons.pr : e.kind === "release_published" ? icons.tag : icons.issue;
export const entryLabel = (e: Entry) => e.kind === "pull_request_merged" ? `PR #${e.number} merged`
  : e.kind === "release_published" ? `${e.title || "Release"} released`
  : `Issue #${e.number} ${e.kind.replace(/^issue_/, "")}`;
export const entryHref = (e: Entry, github: string) => githubHref(e.url, e.number === null ? `${github}/releases` : `${github}/${e.kind === "pull_request_merged" ? "pull" : "issues"}/${e.number}`);
export const actor = (e: Entry) => e.actor ? `${e.actor.login}${e.actor.isBot ? " · bot" : ""}` : "ghost";

export function Timeline({ overview: o, github }: { overview: RepositoryOverview; github: string }) {
  const gapAt = o.eventsCoveredFrom === null ? o.timeline.length
    : Date.parse(o.eventsCoveredFrom) > Date.parse(o.windowStart) ? o.timeline.findIndex(e => Date.parse(e.occurredAt) < Date.parse(o.eventsCoveredFrom!)) : null;
  const gap = <li key="gap" className="hatch border-b border-border p-2.5 text-center text-xs text-muted-foreground">
    {o.eventsCoveredFrom ? `History gap · issue events before ${day.format(new Date(o.eventsCoveredFrom))} were never recorded` : "History gap · issue events are not recorded yet"}
  </li>;
  const rows = o.timeline.map(e => <li key={`${e.kind}-${e.sourceId}`} className="border-b border-border">
    <a href={entryHref(e, github)} className={`${lrow} border-b-0 hover:bg-muted`}>
      <Ico>{entryIcon(e)}</Ico>
      <span className="min-w-0 flex-1"><span className="block font-medium">{entryLabel(e)}{e.kind !== "release_published" && e.title && ` · ${e.title}`}</span><span className={`text-xs ${muted}`}>{actor(e)}</span></span>
      <span className={`font-mono text-xs ${muted}`}>{stamp.format(new Date(e.occurredAt)).replace(",", "")}</span>{icons.ext}
    </a>
  </li>);
  if (gapAt !== null) rows.splice(gapAt === -1 ? rows.length : gapAt, 0, gap);
  if (!o.timeline.length && gapAt === null) return <p className={`p-4 ${muted}`}>No activity in this window.</p>;
  return <ul aria-label="Activity" className="m-0 list-none p-0">{rows}</ul>;
}

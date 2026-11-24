import { useEffect, useRef } from "react";
import { Button } from "@/components/ui/button";
import { BOARD_STATUSES, stageAssignees, type BoardCard, type BoardTarget } from "@/lib/board";
import type { BoardIssue, BoardStatus } from "@/lib/api";

// Each Board column's name.
export const STATUS_NAMES: Record<BoardStatus, string> = { backlog: "Backlog", todo: "Todo", in_progress: "In Progress", in_review: "In Review", done: "Done" };

const mini = "inline-flex items-center gap-1 whitespace-nowrap rounded-full border px-[7px] text-[11px]";
const option = "inline-flex items-center gap-1.5 rounded-full border border-border px-2.5 py-0.5 text-xs has-checked:border-primary has-checked:bg-primary-soft has-checked:font-semibold has-checked:text-primary has-disabled:opacity-50";
// Links come from the API; only GitHub URLs are rendered as links.
const githubHref = (url: string, fallback: string) => url.startsWith("https://github.com/") ? url : fallback;
const same = (a: string, b: string) => a.toLowerCase() === b.toLowerCase();
const has = (list: string[], value: string) => list.some(item => same(item, value));

type TaskDrawerProps = {
  issueNumber: number;
  // Undefined when the latest Board read did not return the issue.
  issue: BoardIssue | undefined;
  card: BoardCard | undefined;
  github: string;
  // Logins GitHub accepts as assignees, offered next to the current ones.
  assignable: string[];
  // Why the staging controls are disabled; null when they work.
  blocked: string | null;
  // What the card's staged change does beyond the move, from deriveChange.
  sideEffects: string[];
  onStage: (target: BoardTarget) => void;
  onDiscard: () => void;
  onClose: () => void;
};

// The Board's side panel for one issue: its details, plus a status menu, an assignee menu and Close or Reopen, each of
// which only stages a change on the card. It takes focus when it opens, and Escape closes it.
export function TaskDrawer({ issueNumber, issue, card, github, assignable, blocked, sideEffects, onStage, onDiscard, onClose }: TaskDrawerProps) {
  const ref = useRef<HTMLDivElement>(null);
  useEffect(() => ref.current?.focus(), []);
  useEffect(() => {
    const onKey = (event: KeyboardEvent) => { if (event.key === "Escape") onClose(); };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [onClose]);

  // A card holds the user's target over its snapshot; an unstaged issue's target is where it is now.
  const target: BoardTarget | null = card?.target ?? (issue?.status ? { status: issue.status, assigneesAdd: [], assigneesRemove: [] } : null);
  const base = card?.snapshot.assignees ?? issue?.assignees ?? [];
  const assigned = target ? [...base.filter(login => !has(target.assigneesRemove, login)), ...target.assigneesAdd.filter(login => !has(base, login))] : base;
  const people = [...base, ...assignable, ...assigned].filter((login, i, all) => all.findIndex(other => same(other, login)) === i);
  const disabled = blocked !== null;

  function toggleAssignee(login: string, on: boolean) {
    const next = on ? [...assigned, login] : assigned.filter(other => !same(other, login));
    onStage(stageAssignees(target!, next.filter(login => !has(base, login)), base.filter(login => !has(next, login))));
  }

  return (
    <div
      ref={ref}
      role="dialog"
      aria-label={`Issue #${issueNumber}`}
      tabIndex={-1}
      className="fixed inset-y-3 right-3 z-40 flex w-[min(400px,calc(100vw-24px))] flex-col overflow-hidden rounded-xl border border-border bg-card shadow-[0_18px_44px_rgba(15,23,42,.22)] outline-none"
    >
      <div className="flex flex-none items-start justify-between gap-2 border-b border-border p-4">
        <div className="min-w-0">
          <div className="font-mono text-xs text-muted-foreground">#{issueNumber}{issue && ` · ${issue.state}`}</div>
          <h2 className="m-0 break-words text-base">{issue?.title ?? "Issue not on the Board"}</h2>
        </div>
        <Button variant="outline" size="sm" onClick={onClose} aria-label="Close task drawer">Close</Button>
      </div>
      <div className="flex min-h-0 flex-1 flex-col gap-3 overflow-y-auto p-4 text-[13px]">
        {!issue ? <p className="m-0 text-muted-foreground">The Board's latest read did not include this issue.</p> : <>
          <a href={githubHref(issue.url, `${github}/issues/${issueNumber}`)} target="_blank" rel="noreferrer" className="self-start text-primary underline">Open on GitHub ↗</a>
          <div className="flex flex-wrap gap-1">
            {issue.labels.length ? issue.labels.map(name => <span key={name} className={`${mini} border-border bg-muted text-soft`}>{name}</span>) : <span className="text-xs text-muted-foreground">No labels</span>}
          </div>
          {blocked && <p role="status" className="m-0 rounded-[10px] border border-warning bg-warning-soft px-3 py-2">{blocked}</p>}
          {target ? <>
            <fieldset className="m-0 flex flex-col gap-1.5 border-0 p-0">
              <legend className="mb-1.5 text-xs font-medium text-soft">Status</legend>
              <div className="flex flex-wrap gap-1.5">
                {BOARD_STATUSES.map(status => (
                  <label key={status} className={option}>
                    <input type="radio" name={`board-status-${issueNumber}`} className="accent-primary" checked={target.status === status} disabled={disabled} onChange={() => onStage({ ...target, status })} />
                    {STATUS_NAMES[status]}
                  </label>
                ))}
              </div>
            </fieldset>
            <fieldset className="m-0 flex flex-col gap-1 border-0 p-0">
              <legend className="mb-1.5 text-xs font-medium text-soft">Assignees</legend>
              {people.length ? people.map(login => (
                <label key={login} className="flex items-center gap-2 has-disabled:opacity-50">
                  <input type="checkbox" className="accent-primary" checked={has(assigned, login)} disabled={disabled} onChange={e => toggleAssignee(login, e.target.checked)} />
                  {login}
                </label>
              )) : <span className="text-xs text-muted-foreground">No assignable users.</span>}
            </fieldset>
            <Button variant="outline" size="sm" className="self-start" disabled={disabled} onClick={() => onStage({ ...target, status: target.status === "done" ? "backlog" : "done" })}>
              {target.status === "done" ? "Reopen issue" : "Close issue"}
            </Button>
          </> : <p className="m-0 text-xs text-muted-foreground">Assignees: {issue.assignees.length ? issue.assignees.map(a => `@${a}`).join(", ") : "none"}</p>}
        </>}
        {card && (
          <section aria-label="Staged change" className="flex flex-col gap-1.5 rounded-[10px] border border-primary-soft bg-primary-soft px-3 py-2">
            <strong>{card.target.status === card.snapshot.status ? "Staged assignee change" : `Staged: ${STATUS_NAMES[card.snapshot.status]} → ${STATUS_NAMES[card.target.status]}`}</strong>
            {sideEffects.length > 0 && <ul className="m-0 list-disc pl-4">{sideEffects.map(line => <li key={line}>{line}</li>)}</ul>}
            <span className="text-xs text-muted-foreground">Nothing is sent to GitHub until you save.</span>
            <Button variant="outline" size="sm" className="self-start" disabled={card.phase === "pending" || card.phase === "unknown"} onClick={onDiscard}>Discard</Button>
          </section>
        )}
      </div>
    </div>
  );
}

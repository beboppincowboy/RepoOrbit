import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { SideDrawer } from "@/shared/ui/side-drawer";
import { Button } from "@/shared/ui/button";
import { mini } from "@/features/board/components/board-styles";
import { stageAssignees, targetOf, type BoardCard, type BoardTarget } from "@/features/board/model";
import { canDiscardBoardCard, recoveryAction } from "@/features/board/recovery";
import { confirmDisabled } from "@/features/board/review";
import type { BoardIssue } from "@/features/board/api";
import { fetchIssueDelivery, type DeliveryCheck, type IssueDelivery } from "@/features/issues/api";
import { hasIgnoreCase } from "@/features/issues/model";
import { BOARD_STATUSES, STATUS_NAMES } from "@/features/issues/status";
import { deploymentUnavailableNote, githubHref, isFailedConclusion, isGitHubUrl, reviewLabels, workflowStatusLabel } from "@/shared/utils/delivery";

const option = "inline-flex items-center gap-1.5 rounded-full border border-border px-2.5 py-0.5 text-xs has-checked:border-primary has-checked:bg-primary-soft has-checked:font-semibold has-checked:text-primary has-disabled:opacity-50";
const same = (a: string, b: string) => a.toLowerCase() === b.toLowerCase();

const isFailed = (check: DeliveryCheck) => check.status === "completed" && isFailedConclusion(check.conclusion);
const checkTone = (check: DeliveryCheck) => isFailed(check) ? "border-destructive/30 bg-destructive/10 text-destructive"
  : check.status === "completed" && check.conclusion === "success" ? "border-success/30 bg-success/10 text-success"
  : "border-border bg-muted text-muted-foreground";

// Each recovery action's button, idle and while it runs.
const recoveryNames = { recheck: ["Recheck", "Rechecking…"], "resend": ["Check recorded status", "Checking…"] } as const;

type TaskDrawerProps = {
  issueNumber: number;
  repositoryId: string;
  viewerKey: string;
  onOpenRun?: (runId: string) => void;
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
  // Why an unknown card's recovery action is disabled; null when the Board is not blocked.
  recoveryBlocked: string | null;
  // Whether a save or a recovery is running.
  busy: boolean;
  // Runs the unknown card's recovery action and resolves the failure to show, or null.
  onRecover: () => Promise<string | null>;
  onStage: (target: BoardTarget) => void;
  onDiscard: () => void;
  onClose: () => void;
};

// The Board's side panel for one issue: its details, plus a status menu, an assignee menu and Close or Reopen, each of
// which only stages a change on the card. An unknown card also gets its recovery action here. It takes focus when it
// opens, and Escape closes it.
export function TaskDrawer({ issueNumber, repositoryId, viewerKey, onOpenRun, issue, card, github, assignable, blocked, sideEffects, recoveryBlocked, busy, onRecover, onStage, onDiscard, onClose }: TaskDrawerProps) {
  // A card holds the user's target over its snapshot; an unstaged issue's target is where it is now.
  const target: BoardTarget | null = card?.target ?? (issue?.status ? targetOf(issue, undefined) : null);
  const base = card?.snapshot.assignees ?? issue?.assignees ?? [];
  const assigned = target ? [...base.filter(login => !hasIgnoreCase(target.assigneesRemove, login)), ...target.assigneesAdd.filter(login => !hasIgnoreCase(base, login))] : base;
  const people = [...base, ...assignable, ...assigned].filter((login, i, all) => all.findIndex(other => same(other, login)) === i);
  const disabled = blocked !== null;
  const delivery = useQuery({
    queryKey: ["issueDelivery", viewerKey, repositoryId, issueNumber],
    queryFn: ({ signal }) => fetchIssueDelivery(repositoryId, issueNumber, signal),
    gcTime: 0,
    retry: false,
  });

  const [recovering, setRecovering] = useState(false);
  const [recoveryError, setRecoveryError] = useState<string | null>(null);
  const action = card ? recoveryAction(card) : "none";
  const canDiscard = card ? canDiscardBoardCard(card) : false;

  async function recover() {
    setRecovering(true);
    setRecoveryError(null);
    try {
      setRecoveryError(await onRecover());
    } finally {
      setRecovering(false);
    }
  }

  function toggleAssignee(login: string, on: boolean) {
    const next = on ? [...assigned, login] : assigned.filter(other => !same(other, login));
    onStage(stageAssignees(target!, next.filter(login => !hasIgnoreCase(base, login)), base.filter(login => !hasIgnoreCase(next, login))));
  }

  return (
    <SideDrawer
      ariaLabel={`Issue #${issueNumber}`}
      closeLabel="Close task drawer"
      eyebrow={<>#{issueNumber}{issue && ` · ${issue.state}`}</>}
      title={issue?.title ?? "Issue not on the Board"}
      onClose={onClose}
    >
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
                <input type="checkbox" className="accent-primary" checked={hasIgnoreCase(assigned, login)} disabled={disabled} onChange={e => toggleAssignee(login, e.target.checked)} />
                {login}
              </label>
            )) : <span className="text-xs text-muted-foreground">No assignable users.</span>}
          </fieldset>
          <Button variant="outline" size="sm" className="self-start" disabled={disabled} onClick={() => onStage({ ...target, status: target.status === "done" ? "backlog" : "done" })}>
            {target.status === "done" ? "Reopen issue" : "Close issue"}
          </Button>
        </> : <p className="m-0 text-xs text-muted-foreground">Assignees: {issue.assignees.length ? issue.assignees.map(a => `@${a}`).join(", ") : "none"}</p>}
      </>}
      <DeliverySection query={delivery} github={github} onOpenRun={onOpenRun} />
      {card && (
        <section aria-label="Staged change" className="flex flex-col gap-1.5 rounded-[10px] border border-primary-soft bg-primary-soft px-3 py-2">
          <strong>{card.target.status === card.snapshot.status ? "Staged assignee change" : `Staged: ${STATUS_NAMES[card.snapshot.status]} → ${STATUS_NAMES[card.target.status]}`}</strong>
          {sideEffects.length > 0 && <ul className="m-0 list-disc pl-4">{sideEffects.map(line => <li key={line}>{line}</li>)}</ul>}
          {card.phase === "unknown" ? <>
            <span className="text-xs text-muted-foreground">{action === "recheck"
              ? "This change may already be on GitHub. Recheck reads the issue again to confirm it."
              : action === "resend"
                ? "This change may already be on GitHub. Checking resends the same request under the same key, so GitHub is not asked to apply it twice."
                : canDiscard
                  ? "This change may already be on GitHub. Check GitHub first, then discard."
                  : "This change may already be on GitHub, and it can't be checked from here. The card stays locked."}</span>
            {action !== "none" && <>
              {recoveryBlocked && <p role="status" className="m-0">{recoveryBlocked}</p>}
              <Button size="sm" className="self-start" disabled={confirmDisabled({ blocked: recoveryBlocked, saving: busy })} aria-busy={recovering} onClick={() => void recover()}>
                {recoveryNames[action][recovering ? 1 : 0]}
              </Button>
            </>}
            <Link to="/workspace/$repositoryId" params={{ repositoryId }} search={prev => ({ days: prev.days ?? 30, tab: "issues" })} className="self-start text-primary underline">Write history</Link>
          </> : card.phase === "changed" || card.phase === "failed" ? <>
            <span className="text-xs text-muted-foreground">{card.phase === "changed" ? "This issue changed on GitHub. Stage your change again to save it, or discard it." : "The save failed. Stage your change again to retry, or discard it."}</span>
            <Button variant="outline" size="sm" className="self-start" disabled={disabled || !issue?.status} onClick={() => onStage(card.target)}>Stage again</Button>
          </> : <span className="text-xs text-muted-foreground">Nothing is sent to GitHub until you save.</span>}
          {recoveryError && card.phase !== "staged" && <p role="alert" className="m-0 rounded-[10px] border border-destructive bg-destructive-soft px-3 py-2">{recoveryError}</p>}
          <Button variant="outline" size="sm" className="self-start" disabled={!canDiscard} onClick={onDiscard}>Discard</Button>
        </section>
      )}
    </SideDrawer>
  );
}

function DeliverySection({ query, github, onOpenRun }: { query: { isPending: boolean; isError: boolean; data: IssueDelivery | null | undefined }; github: string; onOpenRun?: (runId: string) => void }) {
  const data = query.data;
  const denied = data && !data.actionsCoverage.covered && data.actionsCoverage.reason === "permission_denied";
  return (
    <section aria-label="Delivery lifecycle" className="flex flex-col gap-2 rounded-[10px] border border-border px-3 py-2.5">
      <h3 className="m-0 text-xs font-medium uppercase tracking-wide text-muted-foreground">Delivery lifecycle</h3>
      {query.isPending && <p role="status" className="m-0 text-xs text-muted-foreground">Loading delivery lifecycle…</p>}
      {query.isError && <p role="alert" className="m-0 text-xs text-muted-foreground">Delivery lifecycle is unavailable right now.</p>}
      {!query.isPending && !query.isError && !data && <p className="m-0 text-xs text-muted-foreground">Delivery lifecycle is not available for this issue.</p>}
      {data && <>
        {denied && <p role="status" className="m-0 rounded-lg border border-warning bg-warning-soft px-2.5 py-1.5 text-xs">CI/CD data unavailable: missing GitHub Actions read permission.</p>}
        {data.pullRequests.length === 0
          ? <p className="m-0 text-xs text-muted-foreground">No confirmed GitHub-linked pull requests.</p>
          : <ul className="m-0 flex list-none flex-col gap-2.5 p-0">
            {data.pullRequests.map(pr => {
              const review = pr.reviewState ? reviewLabels[pr.reviewState] : null;
              return <li key={pr.number} className="flex flex-col gap-1.5">
                <div className="flex flex-wrap items-center gap-1.5">
                  <a href={githubHref(pr.url, `${github}/pull/${pr.number}`)} target="_blank" rel="noreferrer" className="font-medium text-primary underline">PR #{pr.number}</a>
                  <span className="min-w-0 break-words">{pr.title}</span>
                  {review ? <span className={`${mini} ${review[1]}`}>{review[0]}</span>
                    : <span className={`${mini} border-dashed border-border-strong text-muted-foreground`}>Review history importing</span>}
                </div>
                {!denied && (pr.checks.length === 0
                  ? <p className="m-0 text-xs text-muted-foreground">No CI checks recorded for this pull request.</p>
                  : <ul aria-label={`Checks for PR #${pr.number}`} className="m-0 flex list-none flex-col gap-1.5 p-0">
                    {pr.checks.map(check => <li key={check.runId} className="flex flex-col gap-1 rounded-lg bg-muted px-2.5 py-1.5">
                      <div className="flex items-center justify-between gap-2">
                        <span className="font-medium">{check.workflowName}</span>
                        <span className={`${mini} ${checkTone(check)}`}>{workflowStatusLabel(check.status, check.conclusion)}</span>
                      </div>
                      {isFailed(check) && <div className="flex flex-wrap items-center gap-x-3 gap-y-1 text-xs">
                        {check.failingJobName && <span>Failing job: <strong>{check.failingJobName}</strong></span>}
                        {check.failingJobUrl && isGitHubUrl(check.failingJobUrl) && <a href={check.failingJobUrl} target="_blank" rel="noreferrer" className="text-primary underline">View logs on GitHub ↗</a>}
                        <a href={githubHref(check.htmlUrl, `${github}/actions/runs/${check.runId}`)} target="_blank" rel="noreferrer" className="text-primary underline">View run on GitHub ↗</a>
                        {onOpenRun && <Button variant="outline" size="sm" onClick={() => onOpenRun(check.runId)}>Open run detail</Button>}
                      </div>}
                    </li>)}
                  </ul>)}
                {pr.deployments.length > 0 && <ul aria-label={`Deployments for PR #${pr.number}`} className="m-0 flex list-none flex-col gap-1 p-0 text-xs">
                  {pr.deployments.map(dep => <li key={dep.id} className="flex flex-wrap items-center gap-2">
                    <span className="font-medium">{dep.environment}</span>
                    <span className={`${mini} border-border bg-muted text-muted-foreground`}>{dep.state}</span>
                    <a href={githubHref(dep.htmlUrl, `${github}/deployments`)} target="_blank" rel="noreferrer" className="text-primary underline">GitHub ↗</a>
                  </li>)}
                </ul>}
              </li>;
            })}
          </ul>}
        {data.deploymentUnavailable
          ? <p className="m-0 text-xs text-muted-foreground"><strong className="font-medium text-foreground">Deployment data unavailable</strong> — {deploymentUnavailableNote(data.deploymentsCoverage)}</p>
          : data.pullRequests.every(pr => pr.deployments.length === 0) && <p className="m-0 text-xs text-muted-foreground">No deployments recorded for these pull requests.</p>}
      </>}
    </section>
  );
}

import { useState, type FormEvent } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Button } from "@/shared/ui/button";
import {
  fetchWriteOperations,
  recheckWriteOperation,
  type CleanupDraft,
  type IssueCreateDraft,
  type IssueUpdateDraft,
  type StatusLabelsDraft,
  type WriteOperation,
  type WriteOperationIssue,
} from "@/features/issues/api";
import { IssueWriteError } from "@/features/issues/write-errors";

const stamp = new Intl.DateTimeFormat("en-US", { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit", hourCycle: "h23", timeZone: "UTC" });
const clock = new Intl.DateTimeFormat("en-GB", { hour: "2-digit", minute: "2-digit" });

const stateBadge: Record<WriteOperation["state"], [string, string]> = {
  succeeded: ["Confirmed", "border-success-soft bg-success-soft text-success-foreground"],
  unknown: ["Unknown", "border-warning bg-warning-soft text-warning"],
  pending: ["Pending", "border-run bg-run-soft text-run"],
  failed: ["Failed", "border-destructive bg-destructive-soft text-destructive"],
};

function safeHref(url?: string | null): string | null {
  if (!url) return null;
  return /^https?:\/\//i.test(url) ? url : null;
}

// Operations come from two id sequences (user writes and the app's cleanups), so rows are keyed by both.
function rowKey(op: WriteOperation): string {
  return `${op.kind === "cleanup" ? "app" : "user"}-${op.id}`;
}

function title(op: WriteOperation): string {
  switch (op.kind) {
    case "create": return "Create issue";
    case "update": return `Update issue #${op.issueNumber}`;
    case "status_labels": return "Turn on status labels";
    case "cleanup": return `Remove status label from closed issue #${op.issueNumber}`;
  }
}

function summarizeDraft(op: WriteOperation): string {
  if (!op.draft) return "Draft removed (redacted)";
  if (op.kind === "status_labels") {
    const created = op.outcome?.created ?? [];
    const labels = (op.draft as StatusLabelsDraft).labels ?? [];
    if (op.state !== "succeeded") return `Labels: ${labels.join(", ")}`;
    // A label not in created may have existed already or been created by a request whose answer was lost.
    const others = labels.filter(l => !created.includes(l));
    return [created.length ? `created: ${created.join(", ")}` : "", others.length ? `confirmed present: ${others.join(", ")}` : ""].filter(Boolean).join(" · ");
  }
  if (op.kind === "cleanup") return `-label: ${(op.draft as CleanupDraft).label} (the issue is closed, so it is in Done)`;
  if (op.kind === "create") {
    const d = op.draft as IssueCreateDraft;
    const parts = [`Create "${d.title}"`];
    if (d.labels?.length) parts.push(`labels: ${d.labels.join(", ")}`);
    if (d.assignees?.length) parts.push(`assignees: ${d.assignees.map(a => `@${a}`).join(", ")}`);
    return parts.join(" · ");
  }
  const d = op.draft as IssueUpdateDraft;
  const parts: string[] = [];
  if (d.labels?.add?.length) parts.push(`+labels: ${d.labels.add.join(", ")}`);
  if (d.labels?.remove?.length) parts.push(`-labels: ${d.labels.remove.join(", ")}`);
  if (d.assignees?.add?.length) parts.push(`+assignees: ${d.assignees.add.map(a => `@${a}`).join(", ")}`);
  if (d.assignees?.remove?.length) parts.push(`-assignees: ${d.assignees.remove.map(a => `@${a}`).join(", ")}`);
  if (d.state) parts.push(`state → ${d.state}`);
  return parts.length ? parts.join(" · ") : "Update issue";
}

export function writeOperationsQueryKey(viewerKey: string, repositoryId: string) {
  return ["writeOperations", viewerKey, repositoryId] as const;
}

export function statusLabelsQueryKey(viewerKey: string, repositoryId: string) {
  return ["statusLabels", viewerKey, repositoryId] as const;
}

export function WriteHistory({
  repositoryId,
  viewerKey,
  demo,
  frozen,
  onResolved,
  onEditIssue,
}: {
  repositoryId: string;
  viewerKey: string;
  demo: boolean;
  frozen: boolean;
  onResolved: (op: WriteOperation) => void;
  onEditIssue: (issueNumber: number, confirmed?: WriteOperationIssue) => void;
}) {
  const queryClient = useQueryClient();
  const queryKey = writeOperationsQueryKey(viewerKey, repositoryId);
  const query = useQuery({
    queryKey,
    queryFn: ({ signal }) => fetchWriteOperations(repositoryId, signal),
    enabled: !demo && !frozen,
    gcTime: 0,
  });

  return (
    <section aria-label="Write history" className="flex min-h-0 flex-col rounded-xl border border-border bg-card">
      <div className="flex flex-none items-center justify-between gap-2 border-b border-border px-4 py-3">
        <div>
          <h2 className="m-0 text-sm">Write history</h2>
          <p className="m-0 text-xs text-muted-foreground">Your audited GitHub issue writes here, and the GitHub App&rsquo;s status label cleanup.</p>
        </div>
        {!demo && !frozen && (
          <Button variant="outline" size="sm" onClick={() => void query.refetch()} disabled={query.isFetching}>
            Refresh
          </Button>
        )}
      </div>

      <div className="min-h-0 flex-1 overflow-y-auto">
        {demo ? (
          <p className="m-0 p-4 text-[13px] text-muted-foreground">
            Demo mode — remote GitHub writes are disabled, so no write operations are recorded.
          </p>
        ) : frozen ? (
          <p className="m-0 p-4 text-[13px] text-muted-foreground">
            Write history needs a live connection to GitHub.
          </p>
        ) : query.isPending ? (
          <p role="status" className="m-0 p-4 text-[13px] text-muted-foreground">Loading write history…</p>
        ) : query.isError ? (
          <div role="alert" className="p-4 text-[13px]">
            <p className="m-0">
              {query.error instanceof IssueWriteError && query.error.retryAfter
                ? `${query.error.message} Retry after ${clock.format(new Date(query.error.retryAfter))}.`
                : query.error.message}
            </p>
            <Button variant="outline" size="sm" className="mt-2.5" onClick={() => void query.refetch()} disabled={query.isFetching}>
              Retry
            </Button>
          </div>
        ) : query.data.length === 0 ? (
          <p className="m-0 p-4 text-[13px] text-muted-foreground">No issue write operations recorded for your account in this repository.</p>
        ) : (
          <ul aria-label="Write operations" className="m-0 list-none p-0">
            {query.data.map(op => (
              <WriteHistoryRow
                key={rowKey(op)}
                op={op}
                onUpdated={updated => {
                  queryClient.setQueryData<WriteOperation[]>(queryKey, prev => prev?.map(item => (rowKey(item) === rowKey(updated) ? updated : item)) ?? [updated]);
                  if (updated.kind === "status_labels") void queryClient.invalidateQueries({ queryKey: statusLabelsQueryKey(viewerKey, repositoryId) });
                  onResolved(updated);
                }}
                onEditIssue={onEditIssue}
              />
            ))}
          </ul>
        )}
      </div>
    </section>
  );
}

function WriteHistoryRow({
  op,
  onUpdated,
  onEditIssue,
}: {
  op: WriteOperation;
  onUpdated: (op: WriteOperation) => void;
  onEditIssue: (issueNumber: number, confirmed?: WriteOperationIssue) => void;
}) {
  const [issueNumberInput, setIssueNumberInput] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const [label, tone] = stateBadge[op.state];
  const issueHref = safeHref(op.outcome?.issue?.url);
  const searchHref = safeHref(op.searchUrl);

  async function runRecheck(event?: FormEvent) {
    event?.preventDefault();
    if (busy) return;
    setError(null);
    let issueNum: number | undefined;
    if (op.kind === "create") {
      issueNum = Number.parseInt(issueNumberInput.trim(), 10);
      if (!Number.isInteger(issueNum) || issueNum <= 0) {
        setError("Enter a valid GitHub issue number to recheck.");
        return;
      }
    }
    setBusy(true);
    try {
      const updated = await recheckWriteOperation(op.id, issueNum);
      setIssueNumberInput("");
      onUpdated(updated);
    } catch (err: unknown) {
      if (err instanceof IssueWriteError && err.kind === "recheck_unresolved") {
        if (err.operation) onUpdated(err.operation);
        setError(
          op.kind === "create"
            ? `Issue #${issueNum} did not match this draft. Operation stays unknown.`
            : "Changes are not yet confirmed on GitHub. Operation stays unknown.",
        );
      } else {
        setError(err instanceof Error ? err.message : "Could not recheck operation.");
      }
    } finally {
      setBusy(false);
    }
  }

  return (
    <li className="flex flex-col gap-2 border-b border-border px-4 py-3 text-[13px] last:border-b-0">
      <div className="flex flex-wrap items-center gap-2">
        <span className={`inline-flex items-center rounded-full border px-2 py-0.5 text-[11px] font-medium ${tone}`}>{label}</span>
        <span className="font-medium">
          {title(op)}
          {op.kind === "create" && op.issueNumber && (
            <>
              {" · "}
              {issueHref
                ? <a href={issueHref} target="_blank" rel="noreferrer" className="font-mono text-primary underline">#{op.issueNumber} ↗</a>
                : <span className="font-mono">#{op.issueNumber}</span>}
            </>
          )}
          {op.kind === "update" && issueHref && (
            <>{" "}<a href={issueHref} target="_blank" rel="noreferrer" className="text-xs font-normal text-primary underline">GitHub ↗</a></>
          )}
        </span>
        <span className="ml-auto font-mono text-xs text-muted-foreground">
          {op.kind === "cleanup" ? "GitHub App" : `@${op.actor}`} · {stamp.format(new Date(op.createdAt)).replace(",", "")} UTC
        </span>
      </div>

      <div className="text-xs text-soft">{summarizeDraft(op)}</div>

      {op.outcome?.issue && (
        <div className="text-xs text-muted-foreground">
          Confirmed: <span className="text-foreground">{op.outcome.issue.state}</span>
          {op.outcome.issue.labels && ` · labels: ${op.outcome.issue.labels.length ? op.outcome.issue.labels.join(", ") : "none"}`}
          {op.outcome.issue.assignees && ` · assignees: ${op.outcome.issue.assignees.length ? op.outcome.issue.assignees.map(a => `@${a}`).join(", ") : "unassigned"}`}
        </div>
      )}

      {op.outcome?.unapplied && op.outcome.unapplied.length > 0 && (
        <div className="text-xs text-warning">
          Unapplied: {op.outcome.unapplied.join(", ")}
        </div>
      )}

      {op.outcome?.redacted && typeof op.outcome.unappliedCount === "number" && op.outcome.unappliedCount > 0 && (
        <div className="text-xs text-warning">
          {op.outcome.unappliedCount} requested {op.outcome.unappliedCount === 1 ? "change was" : "changes were"} not applied (details redacted).
        </div>
      )}

      {op.outcome?.error && (
        <div className="text-xs text-destructive">{op.outcome.error}</div>
      )}

      {op.state === "unknown" && op.kind === "cleanup" && (
        <div className="text-xs text-warning">No reliable answer yet; the app retries the removal on its own.</div>
      )}

      {op.state === "unknown" && op.kind !== "cleanup" && (
        <div className="flex flex-col gap-2 rounded-lg border border-warning bg-warning-soft p-2.5 text-xs">
          <div className="flex flex-wrap items-center justify-between gap-2">
            <span>Outcome uncertain — verify on GitHub and recheck instead of resending.</span>
            {searchHref && (
              <a href={searchHref} target="_blank" rel="noreferrer" className="text-primary underline">Search GitHub ↗</a>
            )}
          </div>
          {op.kind === "create" ? (
            <form onSubmit={e => void runRecheck(e)} className="flex flex-wrap items-center gap-2">
              <input
                type="number"
                min={1}
                step={1}
                aria-label={`Issue number for operation #${op.id}`}
                placeholder="GitHub issue #"
                value={issueNumberInput}
                onChange={e => { setIssueNumberInput(e.target.value); setError(null); }}
                className="w-36 rounded-lg border border-border bg-card px-2 py-1 text-xs"
              />
              <Button type="submit" variant="outline" size="sm" disabled={busy}>
                {busy ? "Rechecking…" : "Recheck"}
              </Button>
            </form>
          ) : (
            <div>
              <Button type="button" variant="outline" size="sm" onClick={() => void runRecheck()} disabled={busy}>
                {busy ? "Rechecking…" : "Recheck"}
              </Button>
            </div>
          )}
          {error && <div role="alert" className="text-destructive">{error}</div>}
        </div>
      )}

      {op.state === "failed" && op.kind === "create" && op.issueNumber && (
        <div>
          <Button variant="outline" size="sm" onClick={() => onEditIssue(op.issueNumber!, op.outcome?.issue)}>
            Edit issue #{op.issueNumber}
          </Button>
        </div>
      )}
    </li>
  );
}

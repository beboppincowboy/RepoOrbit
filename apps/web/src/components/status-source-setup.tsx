import { useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Button } from "@/shared/ui/button";
import { STATUS_NAMES } from "@/components/task-drawer";
import { statusLabelsQueryKey, writeOperationsQueryKey } from "@/components/write-history";
import { STATUS_LABELS } from "@/lib/board";
import { canReplayWithSameKey, disableStatusLabels, enableStatusLabels, fetchStatusLabels, generateIdempotencyKey, mayHaveBeenRecorded, type BoardStatus, type WriteOperation } from "@/lib/api";

const columns: Record<string, string> = Object.fromEntries(
  (Object.entries(STATUS_LABELS) as [BoardStatus, string | undefined][])
    .filter((entry): entry is [BoardStatus, string] => Boolean(entry[1]))
    .map(([status, label]) => [label, STATUS_NAMES[status]]),
);

type Step = "idle" | "review" | "submitting" | "confirm-off";

export function StatusSourceSetup({ repositoryId, viewerKey, demo, frozen }: { repositoryId: string; viewerKey: string; demo: boolean; frozen: boolean }) {
  const queryClient = useQueryClient();
  const queryKey = statusLabelsQueryKey(viewerKey, repositoryId);
  const query = useQuery({ queryKey, queryFn: ({ signal }) => fetchStatusLabels(repositoryId, signal), enabled: !demo && !frozen, gcTime: 0 });
  const [step, setStep] = useState<Step>("idle");
  const [result, setResult] = useState<WriteOperation | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [retryKey, setRetryKey] = useState<string | null>(null);
  const [retryAttemptedAt, setRetryAttemptedAt] = useState<string | null>(null);

  function settle() {
    void queryClient.invalidateQueries({ queryKey });
    void queryClient.invalidateQueries({ queryKey: writeOperationsQueryKey(viewerKey, repositoryId) });
  }

  async function turnOn() {
    const activeRetryKey = retryKey && canReplayWithSameKey(retryAttemptedAt) ? retryKey : null;
    const key = activeRetryKey ?? generateIdempotencyKey();
    const attemptStamp = activeRetryKey ? retryAttemptedAt! : new Date().toISOString();
    setStep("submitting");
    setError(null);
    try {
      const op = await enableStatusLabels(repositoryId, key);
      setRetryKey(null);
      setRetryAttemptedAt(null);
      setResult(op);
      setStep("idle");
      settle();
    } catch (err: unknown) {
      const uncertain = mayHaveBeenRecorded(err, { isReplay: Boolean(activeRetryKey) });
      setRetryKey(uncertain ? key : null);
      setRetryAttemptedAt(uncertain ? attemptStamp : null);
      setError(err instanceof Error ? err.message : "Could not turn status labels on.");
      setStep("review");
    }
  }

  async function turnOff() {
    setStep("submitting");
    setError(null);
    try {
      await disableStatusLabels(repositoryId);
      setResult(null);
      setStep("idle");
      settle();
    } catch (err: unknown) {
      setError(err instanceof Error ? err.message : "Could not turn status labels off.");
      setStep("confirm-off");
    }
  }

  const state = query.data;

  function renderStatusBody() {
    if (demo) {
      return <p className="m-0 text-xs text-muted-foreground">Demo mode — GitHub writes are disabled, so status labels cannot be turned on.</p>;
    }
    if (frozen) {
      return <p className="m-0 text-xs text-muted-foreground">Changing status labels needs a live connection to GitHub.</p>;
    }
    if (query.isPending) {
      return <p role="status" className="m-0 text-xs text-muted-foreground">Checking status labels…</p>;
    }
    if (query.isError) {
      return (
        <p role="alert" className="m-0 text-xs">
          {query.error.message} <Button variant="outline" size="sm" onClick={() => void query.refetch()}>Retry</Button>
        </p>
      );
    }
    const status = query.data;
    if (!status.canChange) {
      return (
        <p className="m-0 text-xs text-muted-foreground">
          {status.enabled ? "Issues are placed in columns by their status label." : "Without status labels the Board shows every issue under “No status source”."}
          {" "}Only Admin or Maintain users can change this.
        </p>
      );
    }
    if (status.enabled) {
      if (step === "confirm-off" || step === "submitting") {
        return (
          <div className="flex flex-col gap-2 rounded-lg border border-border p-2.5 text-xs">
            <span>Turn status labels off for everyone? The labels stay on GitHub, and the Board shows &ldquo;No status source&rdquo;.</span>
            <div className="flex gap-2">
              <Button variant="destructive" size="sm" onClick={() => void turnOff()} disabled={step === "submitting"}>
                {step === "submitting" ? "Turning off…" : "Turn off"}
              </Button>
              <Button variant="outline" size="sm" onClick={() => { setStep("idle"); setError(null); }} disabled={step === "submitting"}>Cancel</Button>
            </div>
          </div>
        );
      }
      return (
        <div className="flex flex-wrap items-center justify-between gap-2 text-xs">
          <span className="text-muted-foreground">Closed issues lose their status label through the GitHub App.</span>
          <Button variant="outline" size="sm" onClick={() => { setStep("confirm-off"); setResult(null); }}>Turn off</Button>
        </div>
      );
    }
    if (step === "idle") {
      return (
        <div className="flex flex-wrap items-center justify-between gap-2 text-xs">
          <span className="text-muted-foreground">Off: the Board shows every issue under &ldquo;No status source&rdquo;.</span>
          <Button size="sm" onClick={() => { setStep("review"); setResult(null); }}>Turn on…</Button>
        </div>
      );
    }
    return (
      <div className="flex flex-col gap-2 rounded-lg border border-border p-2.5 text-xs">
        <span className="font-medium">Review</span>
        <ul aria-label="Status labels to set up" className="m-0 flex list-none flex-col gap-1 p-0">
          {(status.labels ?? []).map(l => (
            <li key={l.name} className="flex flex-wrap items-center gap-2">
              <code className="font-mono">{l.name}</code>
              <span className="text-muted-foreground">→ {columns[l.name] ?? l.name}</span>
              <span className={l.exists ? "text-muted-foreground" : "text-foreground"}>{l.exists ? "exists, reused" : "will be created"}</span>
            </li>
          ))}
        </ul>
        <span className="text-muted-foreground">This is a setting every viewer of the repository shares. Your GitHub account creates the missing labels.</span>
        <div className="flex gap-2">
          <Button size="sm" onClick={() => void turnOn()} disabled={step === "submitting"}>
            {step === "submitting" ? "Submitting…" : retryKey ? "Retry submit" : "Submit"}
          </Button>
          <Button variant="outline" size="sm" onClick={() => { setStep("idle"); setError(null); setRetryKey(null); setRetryAttemptedAt(null); }} disabled={step === "submitting"}>Cancel</Button>
        </div>
      </div>
    );
  }

  return (
    <section aria-label="Status labels" className="flex flex-col gap-2 rounded-xl border border-border bg-card px-4 py-3 text-[13px]">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <div>
          <h2 className="m-0 text-sm">Status labels</h2>
          <p className="m-0 text-xs text-muted-foreground">Board columns come only from GitHub labels: Backlog, Todo, In Progress, In Review, Done.</p>
        </div>
        {state && (
          <span className={`inline-flex items-center rounded-full border px-2 py-0.5 text-[11px] font-medium ${state.enabled ? "border-success-soft bg-success-soft text-success-foreground" : "border-border text-muted-foreground"}`}>
            {state.enabled ? "On" : "Off"}
          </span>
        )}
      </div>

      {renderStatusBody()}

      {result && !(state?.enabled && result.state !== "succeeded") && (
        <div role="status" className={`text-xs ${result.state === "succeeded" ? "text-success-foreground" : result.state === "unknown" ? "text-warning" : "text-destructive"}`}>
          {result.state === "succeeded"
            ? `Confirmed: status labels are on${result.outcome?.created?.length ? `; created ${result.outcome.created.join(", ")}` : "; all three labels are on GitHub"}.`
            : result.state === "unknown"
              ? "Outcome unknown — recheck it in Write history instead of resubmitting."
              : `Not turned on: ${result.outcome?.error ?? result.outcome?.unapplied?.join(", ") ?? "GitHub did not confirm the labels"}.`}
        </div>
      )}
      {error && <div role="alert" className="text-xs text-destructive">{error}</div>}
    </section>
  );
}

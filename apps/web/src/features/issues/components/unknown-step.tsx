import type { FormEvent } from "react";
import type { WriteOperation } from "@/features/issues/api";
import { fieldInput } from "@/features/issues/components/editor-styles";
import { canReplayWithSameKey } from "@/features/issues/write-errors";
import { Button } from "@/shared/ui/button";

type UnknownStepProps = {
  lastError: string | null;
  duplicateOperationId: number | null;
  lastOperation: WriteOperation | null;
  inspectDuplicateOperation: () => Promise<void>;
  rechecking: boolean;
  searchHref: string | null;
  handleRecheck: (event?: FormEvent) => Promise<void>;
  recheckNumber: string;
  setRecheckNumber: (value: string) => void;
  setRecheckError: (error: string | null) => void;
  keepKeyOnNextSubmit: boolean;
  idempotencyKey: string | null;
  attemptedAt: string | null;
  submitWrite: (reuseExistingKey: boolean) => Promise<void>;
  recheckError: string | null;
};

export function UnknownStep({
  lastError, duplicateOperationId, lastOperation, inspectDuplicateOperation, rechecking,
  searchHref, handleRecheck, recheckNumber, setRecheckNumber, setRecheckError,
  keepKeyOnNextSubmit, idempotencyKey, attemptedAt, submitWrite, recheckError,
}: UnknownStepProps) {
  return (
    <div className="flex flex-col gap-3">
      <div role="status" className="rounded-[10px] border border-warning bg-warning-soft px-3 py-2.5 text-[13px]">
        <strong>Outcome unknown.</strong>{" "}
        {lastError || "No reliable answer arrived from GitHub. The request may have been applied."}{" "}
        Your draft is preserved. RepoOrbit will not resend automatically.
      </div>

      {duplicateOperationId && !lastOperation && (
        <div className="flex flex-col gap-2 rounded-[10px] border border-border bg-muted p-3 text-[13px]">
          <p className="m-0">An earlier create with this title and body is already recorded (operation #{duplicateOperationId}). Inspect and recheck it instead of creating a duplicate.</p>
          <div>
            <Button variant="outline" size="sm" onClick={() => void inspectDuplicateOperation()} disabled={rechecking}>
              {rechecking ? "Loading operation…" : `Inspect operation #${duplicateOperationId}`}
            </Button>
          </div>
        </div>
      )}

      {lastOperation && (
        <div className="flex flex-col gap-2.5 rounded-[10px] border border-border bg-muted p-3 text-[13px]">
          <div className="flex flex-wrap items-center justify-between gap-2">
            <span className="font-mono text-xs text-muted-foreground">Operation #{lastOperation.id} · {lastOperation.state}</span>
            {searchHref && (
              <a href={searchHref} target="_blank" rel="noreferrer" className="text-xs text-primary underline">Search GitHub for this issue ↗</a>
            )}
          </div>

          {lastOperation.kind === "create" ? (
            <form onSubmit={e => void handleRecheck(e)} className="flex flex-col gap-2">
              <label htmlFor="recheck-issue-number" className="text-xs font-medium text-soft">
                Found the created issue on GitHub? Enter its issue number to verify and link it:
              </label>
              <div className="flex gap-2">
                <input
                  id="recheck-issue-number"
                  type="number"
                  min={1}
                  step={1}
                  value={recheckNumber}
                  onChange={e => { setRecheckNumber(e.target.value); setRecheckError(null); }}
                  placeholder="e.g. 42"
                  className={fieldInput}
                />
                <Button type="submit" disabled={rechecking}>
                  {rechecking ? "Rechecking…" : "Recheck"}
                </Button>
              </div>
            </form>
          ) : (
            <div className="flex items-center justify-between gap-2">
              <span className="text-xs text-muted-foreground">Re-read issue #{lastOperation.issueNumber} from GitHub to check whether the changes landed.</span>
              <Button type="button" onClick={() => void handleRecheck()} disabled={rechecking}>
                {rechecking ? "Rechecking…" : "Recheck"}
              </Button>
            </div>
          )}
        </div>
      )}

      {!lastOperation && !duplicateOperationId && keepKeyOnNextSubmit && idempotencyKey && (
        canReplayWithSameKey(attemptedAt) ? (
          <div className="flex flex-col gap-2 rounded-[10px] border border-border bg-muted p-3 text-[13px]">
            <p className="m-0 text-xs text-muted-foreground">
              The server may have already recorded this request under your idempotency key. Retrying with the same key lets the server return any operation it already recorded for this request.
            </p>
            <div>
              <Button variant="outline" size="sm" onClick={() => void submitWrite(true)}>Check recorded status with same key</Button>
            </div>
          </div>
        ) : (
          <div className="flex flex-col gap-1.5 rounded-[10px] border border-warning bg-warning-soft p-3 text-[13px]">
            <div className="font-medium">Manual reconciliation required</div>
            <p className="m-0 text-xs text-muted-foreground">
              This unresolved attempt is outside the 30-day idempotency retention window or has no verified attempt time, so same-key recovery is disabled to avoid creating a duplicate issue. Check GitHub and Write history manually before discarding or re-entering this draft.
            </p>
          </div>
        )
      )}

      {recheckError && (
        <div role="alert" className="rounded-[10px] border border-destructive bg-destructive-soft px-3 py-2 text-[13px]">
          {recheckError}
        </div>
      )}
    </div>
  );
}

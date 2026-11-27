import type { WriteOperation, WriteOperationIssue } from "@/features/issues/api";
import { smallBtn } from "@/features/issues/components/editor-styles";
import { Button } from "@/shared/ui/button";

type FailedStepProps = {
  lastError: string | null;
  lastOperation: WriteOperation | null;
  confirmedIssue: WriteOperationIssue | null;
  confirmedIssueHref: string | null;
  onSwitchToEditIssue: ((issueNumber: number, confirmed?: WriteOperationIssue) => void) | undefined;
};

export function FailedStep({
  lastError, lastOperation, confirmedIssue, confirmedIssueHref, onSwitchToEditIssue,
}: FailedStepProps) {
  return (
    <div className="flex flex-col gap-3">
      <div role="alert" className="rounded-[10px] border border-destructive bg-destructive-soft px-3 py-2.5 text-[13px]">
        <strong>Write did not fully succeed.</strong> {lastError || "GitHub rejected or only partially applied the request."}
      </div>

      {lastOperation?.outcome?.unapplied && lastOperation.outcome.unapplied.length > 0 && (
        <div className="rounded-[10px] border border-warning bg-warning-soft px-3 py-2 text-[13px]">
          <div className="font-medium">Unapplied changes:</div>
          <ul className="m-0 mt-1 list-disc pl-5 text-xs">
            {lastOperation.outcome.unapplied.map(item => <li key={item}>{item}</li>)}
          </ul>
        </div>
      )}

      {confirmedIssue ? (
        <div className="flex flex-col gap-1.5 rounded-[10px] border border-border bg-muted p-3 text-[13px]">
          <div className="font-medium">Issue #{confirmedIssue.number} exists on GitHub</div>
          <p className="m-0 text-xs text-muted-foreground">
            Follow up by updating issue #{confirmedIssue.number} rather than creating a second issue.
          </p>
          <div className="mt-1 flex flex-wrap gap-2">
            {confirmedIssueHref && <a href={confirmedIssueHref} target="_blank" rel="noreferrer" className={smallBtn}>Open on GitHub ↗</a>}
            {onSwitchToEditIssue && (
              <Button variant="outline" size="sm" onClick={() => onSwitchToEditIssue(confirmedIssue.number, confirmedIssue)}>
                Edit issue #{confirmedIssue.number}
              </Button>
            )}
          </div>
        </div>
      ) : (
        <p className="m-0 text-xs text-muted-foreground">Your draft is preserved. Return to Edit to adjust and try again, or close and come back later.</p>
      )}
    </div>
  );
}

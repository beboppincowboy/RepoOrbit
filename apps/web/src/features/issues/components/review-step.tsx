import type { IssueFields } from "@/features/issues/model";

type ReviewStepProps = {
  fields: IssueFields;
  isCreate: boolean;
  fullName: string;
  issueNumber: number;
  viewerLogin: string | null;
};

export function ReviewStep({
  fields, isCreate, fullName, issueNumber, viewerLogin,
}: ReviewStepProps) {
  const {
    title, body, createLabels, createAssignees, addLabels, removeLabels, addAssignees,
    removeAssignees, targetState,
  } = fields;
  return (
    <div className="flex flex-col gap-3">
      <p className="m-0 text-[13px]">Exactly what will be sent to GitHub:</p>
      <div role="region" aria-label="Reviewed changes" className="rounded-[10px] border border-border bg-muted p-3 font-mono text-xs leading-relaxed">
        {isCreate ? (
          <>
            <div className="text-success-foreground">+ create issue in {fullName}: &ldquo;{title.trim()}&rdquo;</div>
            {body.trim() && <div className="mt-1 whitespace-pre-wrap text-soft">+ body: {body.trim().length > 240 ? `${body.trim().slice(0, 240)}…` : body.trim()}</div>}
            {createLabels.map(l => <div key={`l-${l}`} className="text-success-foreground">+ label: {l}</div>)}
            {createAssignees.map(a => <div key={`a-${a}`} className="text-success-foreground">+ assignee: @{a}</div>)}
          </>
        ) : (
          <>
            <div className="text-foreground">~ update issue #{issueNumber} in {fullName}</div>
            {addLabels.map(l => <div key={`al-${l}`} className="text-success-foreground">+ add label: {l}</div>)}
            {removeLabels.map(l => <div key={`rl-${l}`} className="text-destructive">- remove label: {l}</div>)}
            {addAssignees.map(a => <div key={`aa-${a}`} className="text-success-foreground">+ add assignee: @{a}</div>)}
            {removeAssignees.map(a => <div key={`ra-${a}`} className="text-destructive">- remove assignee: @{a}</div>)}
            {targetState && <div className="text-primary">~ state: {targetState}</div>}
          </>
        )}
      </div>
      <p className="m-0 text-xs text-muted-foreground">
        Sent using your GitHub account{viewerLogin ? ` (@${viewerLogin})` : ""}. Nothing else on the issue changes.
      </p>
    </div>
  );
}

import { useId } from "react";
import type { IssueCapabilities, WriteOperation, WriteOperationIssue } from "@/features/issues/api";
import { EditStep } from "@/features/issues/components/edit-step";
import { smallBtn } from "@/features/issues/components/editor-styles";
import { FailedStep } from "@/features/issues/components/failed-step";
import { ReviewStep } from "@/features/issues/components/review-step";
import { UnknownStep } from "@/features/issues/components/unknown-step";
import { useIssueForm } from "@/features/issues/hooks/use-issue-form";
import { safeHref, type EditableIssue, type Step } from "@/features/issues/model";
import { Button } from "@/shared/ui/button";

type IssueEditorProps = {
  mode: "create" | "update";
  issue?: EditableIssue;
  viewerKey: string;
  viewerLogin: string | null;
  repositoryId: string;
  fullName: string;
  demo: boolean;
  archived: boolean;
  capabilities: IssueCapabilities | null;
  capabilitiesLoading: boolean;
  capabilitiesError: string | null;
  onRetryCapabilities: () => void;
  assigneeSuggestions: string[];
  onClose: () => void;
  onOperationRecorded: (op: WriteOperation) => void;
  onSwitchToEditIssue?: (issueNumber: number, confirmed?: WriteOperationIssue) => void;
};

const STEPS = ["Edit", "Review", "Submit", "Confirmed result"] as const;
const STEP_INDEX: Record<Step, number> = { edit: 0, review: 1, submitting: 2, confirmed: 3, unknown: 3, failed: 3 };

export function IssueEditor(props: IssueEditorProps) {
  const {
    issue,
    viewerLogin,
    fullName,
    demo,
    capabilitiesLoading,
    capabilitiesError,
    onRetryCapabilities,
    assigneeSuggestions,
    onClose,
    onSwitchToEditIssue,
  } = props;

  const headingId = useId();
  const suggestId = useId();
  const {
    isCreate, issueNumber, fields, canCreate, canCreateMetadata, canManage, canAuthorToggleState, canEditAnything,
    permissionNotice, validationError, setValidationError, labelInput, setLabelInput, assigneeInput, setAssigneeInput,
    setTitle, setBody, setCreateLabels, setCreateAssignees, setAddLabels, setAddAssignees, setTargetState,
    handleAddLabel, handleAddAssignee, onChipInputKeyDown, toggleRemoveCurrentLabel, toggleRemoveCurrentAssignee,
    discardDraft, goToReview,
    step, setStep, idempotencyKey, attemptedAt, keepKeyOnNextSubmit, lastError, lastOperation, duplicateOperationId,
    recheckNumber, setRecheckNumber, rechecking, recheckError, setRecheckError, hasAnyDraft, submitWrite,
    inspectDuplicateOperation, handleRecheck,
  } = useIssueForm(props);

  const currentLabels = issue?.labels ?? [];
  const currentAssignees = issue?.assignees ?? [];
  const activeIdx = STEP_INDEX[step];

  const confirmedIssue = lastOperation?.outcome?.issue ?? null;
  const confirmedIssueHref = safeHref(confirmedIssue?.url);
  const searchHref = safeHref(lastOperation?.searchUrl);

  return (
    <div
      role="dialog"
      aria-modal="true"
      aria-labelledby={headingId}
      className="fixed inset-0 z-50 grid place-items-center bg-scrim p-4"
      onClick={e => { if (e.target === e.currentTarget && step !== "submitting") onClose(); }}
    >
      <div className="flex max-h-[88vh] w-[min(560px,100%)] flex-col overflow-hidden rounded-xl border border-border bg-card shadow-[0_18px_44px_rgba(15,23,42,.22)]">
        <div className="flex flex-none flex-col gap-2 border-b border-border p-4">
          <div className="flex items-start justify-between gap-2">
            <div className="min-w-0">
              <div className="font-mono text-xs text-muted-foreground">{fullName}{!isCreate && ` · #${issueNumber}`}</div>
              <h2 id={headingId} className="m-0 text-base">{isCreate ? "New issue" : `Edit issue #${issueNumber}`}</h2>
            </div>
            <Button variant="outline" size="sm" onClick={onClose} disabled={step === "submitting"} aria-label="Close issue editor">Close</Button>
          </div>
          <ol aria-label="Write progress" className="m-0 flex flex-wrap gap-1.5 p-0 text-xs">
            {STEPS.map((label, idx) => {
              const done = idx < activeIdx;
              const current = idx === activeIdx;
              return (
                <li
                  key={label}
                  aria-current={current ? "step" : undefined}
                  className={`inline-flex items-center gap-1 rounded-full border px-2.5 py-0.5 ${
                    current
                      ? "border-primary bg-primary-soft font-semibold text-primary"
                      : done
                        ? "border-success-soft bg-success-soft text-success-foreground"
                        : "border-border bg-muted text-muted-foreground"
                  }`}
                >
                  {done && <span aria-hidden="true">✓</span>}
                  {label}
                </li>
              );
            })}
          </ol>
        </div>

        <div className="flex min-h-0 flex-1 flex-col gap-3 overflow-y-auto p-4">
          {demo && (
            <div role="status" className="rounded-[10px] border border-demo bg-demo-soft px-3 py-2 text-[13px] text-demo">
              <strong>Demo mode</strong> — remote GitHub writes are disabled. Connect GitHub credentials to create or edit issues.
            </div>
          )}

          {!demo && capabilitiesLoading && (
            <p role="status" className="m-0 text-[13px] text-muted-foreground">Checking your current GitHub permissions…</p>
          )}

          {!demo && permissionNotice && (
            <div role="status" className="flex flex-wrap items-center justify-between gap-2 rounded-[10px] border border-warning bg-warning-soft px-3 py-2 text-[13px]">
              <span>{permissionNotice}</span>
              {capabilitiesError && (
                <Button variant="outline" size="sm" onClick={onRetryCapabilities}>Retry</Button>
              )}
            </div>
          )}

          {step === "edit" && (
            <EditStep
              fields={fields}
              issue={issue}
              isCreate={isCreate}
              demo={demo}
              canCreate={canCreate}
              canCreateMetadata={canCreateMetadata}
              canManage={canManage}
              canAuthorToggleState={canAuthorToggleState}
              lastError={lastError}
              validationError={validationError}
              labelInput={labelInput}
              assigneeInput={assigneeInput}
              suggestId={suggestId}
              assigneeSuggestions={assigneeSuggestions}
              currentLabels={currentLabels}
              currentAssignees={currentAssignees}
              setTitle={setTitle}
              setBody={setBody}
              setCreateLabels={setCreateLabels}
              setCreateAssignees={setCreateAssignees}
              setAddLabels={setAddLabels}
              setAddAssignees={setAddAssignees}
              setTargetState={setTargetState}
              setValidationError={setValidationError}
              setLabelInput={setLabelInput}
              setAssigneeInput={setAssigneeInput}
              handleAddLabel={handleAddLabel}
              handleAddAssignee={handleAddAssignee}
              onChipInputKeyDown={onChipInputKeyDown}
              toggleRemoveCurrentLabel={toggleRemoveCurrentLabel}
              toggleRemoveCurrentAssignee={toggleRemoveCurrentAssignee}
            />
          )}

          {step === "review" && (
            <ReviewStep
              fields={fields}
              isCreate={isCreate}
              fullName={fullName}
              issueNumber={issueNumber}
              viewerLogin={viewerLogin}
            />
          )}

          {step === "submitting" && (
            <div role="status" className="flex items-center gap-2.5 rounded-[10px] border border-run bg-run-soft px-3 py-3 text-[13px] text-run">
              <span className="size-2 rounded-full bg-current sync-pulse" aria-hidden="true" />
              <span>Submitting to GitHub… Do not resubmit while the request is in flight.</span>
            </div>
          )}

          {step === "confirmed" && (
            <div className="flex flex-col gap-3">
              <div role="status" className="rounded-[10px] border border-success bg-success-soft px-3 py-2.5 text-[13px]">
                <strong>Confirmed on GitHub.</strong>{" "}
                {confirmedIssue
                  ? `Issue #${confirmedIssue.number} (${confirmedIssue.state}) matches all requested changes.`
                  : "All requested changes were confirmed on GitHub."}
              </div>
              {confirmedIssue && (
                <div className="flex flex-col gap-1.5 rounded-[10px] border border-border bg-muted p-3 text-[13px]">
                  <div className="font-medium">#{confirmedIssue.number}{confirmedIssue.title ? ` · ${confirmedIssue.title}` : ""}</div>
                  <div className="text-xs text-muted-foreground">
                    State: <span className="text-foreground">{confirmedIssue.state}</span>
                    {confirmedIssue.labels && ` · Labels: ${confirmedIssue.labels.length ? confirmedIssue.labels.join(", ") : "none"}`}
                    {confirmedIssue.assignees && ` · Assignees: ${confirmedIssue.assignees.length ? confirmedIssue.assignees.map(a => `@${a}`).join(", ") : "unassigned"}`}
                  </div>
                  {confirmedIssueHref && (
                    <a href={confirmedIssueHref} target="_blank" rel="noreferrer" className={`${smallBtn} mt-1 self-start`}>Open on GitHub ↗</a>
                  )}
                </div>
              )}
            </div>
          )}

          {step === "unknown" && (
            <UnknownStep
              lastError={lastError}
              duplicateOperationId={duplicateOperationId}
              lastOperation={lastOperation}
              inspectDuplicateOperation={inspectDuplicateOperation}
              rechecking={rechecking}
              searchHref={searchHref}
              handleRecheck={handleRecheck}
              recheckNumber={recheckNumber}
              setRecheckNumber={setRecheckNumber}
              setRecheckError={setRecheckError}
              keepKeyOnNextSubmit={keepKeyOnNextSubmit}
              idempotencyKey={idempotencyKey}
              attemptedAt={attemptedAt}
              submitWrite={submitWrite}
              recheckError={recheckError}
            />
          )}

          {step === "failed" && (
            <FailedStep
              lastError={lastError}
              lastOperation={lastOperation}
              confirmedIssue={confirmedIssue}
              confirmedIssueHref={confirmedIssueHref}
              onSwitchToEditIssue={onSwitchToEditIssue}
            />
          )}
        </div>

        <div className="flex flex-none flex-wrap items-center justify-between gap-2 border-t border-border p-3.5">
          <div>
            {!demo && hasAnyDraft && step !== "submitting" && step !== "confirmed" && !(step === "unknown" && lastOperation) && (
              <Button variant="ghost" size="sm" onClick={discardDraft}>Discard draft</Button>
            )}
          </div>
          <div className="flex flex-wrap gap-2">
            {step === "edit" && (
              <>
                <Button variant="outline" onClick={onClose}>Cancel</Button>
                <Button onClick={goToReview} disabled={demo || !canEditAnything || capabilitiesLoading}>Review</Button>
              </>
            )}
            {step === "review" && (
              <>
                <Button variant="outline" onClick={() => setStep("edit")}>Back to edit</Button>
                <Button variant="outline" onClick={onClose}>Cancel</Button>
                <Button onClick={() => void submitWrite(false)} disabled={demo || !canEditAnything}>Submit to GitHub</Button>
              </>
            )}
            {step === "confirmed" && (
              <Button onClick={onClose}>Done</Button>
            )}
            {step === "unknown" && (
              <Button variant="outline" onClick={onClose}>Keep draft and close</Button>
            )}
            {step === "failed" && (
              <>
                {!(isCreate && confirmedIssue) && (
                  <Button variant="outline" onClick={() => setStep("edit")}>Back to edit</Button>
                )}
                <Button onClick={onClose}>{isCreate && confirmedIssue ? "Close" : "Keep draft and close"}</Button>
              </>
            )}
          </div>
        </div>
      </div>
    </div>
  );
}

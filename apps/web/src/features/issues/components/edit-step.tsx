import type { KeyboardEvent } from "react";
import { chipClass, fieldInput } from "@/features/issues/components/editor-styles";
import { MAX_TITLE, hasIgnoreCase, removeIgnoreCase, safeHref, type EditableIssue, type IssueFields, type IssueStateChange } from "@/features/issues/model";
import { Button } from "@/shared/ui/button";

type EditStepProps = {
  fields: IssueFields;
  issue: EditableIssue | undefined;
  isCreate: boolean;
  demo: boolean;
  canCreate: boolean;
  canCreateMetadata: boolean;
  canManage: boolean;
  canAuthorToggleState: boolean;
  lastError: string | null;
  validationError: string | null;
  labelInput: string;
  assigneeInput: string;
  suggestId: string;
  assigneeSuggestions: string[];
  currentLabels: string[];
  currentAssignees: string[];
  setTitle: (value: string) => void;
  setBody: (value: string) => void;
  setCreateLabels: (update: (previous: string[]) => string[]) => void;
  setCreateAssignees: (update: (previous: string[]) => string[]) => void;
  setAddLabels: (update: (previous: string[]) => string[]) => void;
  setAddAssignees: (update: (previous: string[]) => string[]) => void;
  setTargetState: (state: IssueStateChange) => void;
  setValidationError: (error: string | null) => void;
  setLabelInput: (value: string) => void;
  setAssigneeInput: (value: string) => void;
  handleAddLabel: () => void;
  handleAddAssignee: () => void;
  onChipInputKeyDown: (kind: "label" | "assignee") => (event: KeyboardEvent<HTMLInputElement>) => void;
  toggleRemoveCurrentLabel: (value: string) => void;
  toggleRemoveCurrentAssignee: (value: string) => void;
};

export function EditStep({
  fields, issue, isCreate, demo, canCreate, canCreateMetadata, canManage, canAuthorToggleState,
  lastError, validationError, labelInput, assigneeInput, suggestId, assigneeSuggestions,
  currentLabels, currentAssignees, setTitle, setBody, setCreateLabels, setCreateAssignees,
  setAddLabels, setAddAssignees, setTargetState, setValidationError, setLabelInput,
  setAssigneeInput, handleAddLabel, handleAddAssignee, onChipInputKeyDown,
  toggleRemoveCurrentLabel, toggleRemoveCurrentAssignee,
}: EditStepProps) {
  const {
    title, body, createLabels, createAssignees, addLabels, removeLabels, addAssignees,
    removeAssignees, targetState,
  } = fields;
  return (
    <>
      {lastError && (
        <div role="alert" className="rounded-[10px] border border-destructive bg-destructive-soft px-3 py-2 text-[13px]">
          <strong>Previous attempt did not succeed:</strong> {lastError} Your draft was kept below.
        </div>
      )}

      {validationError && (
        <div role="alert" className="rounded-[10px] border border-destructive bg-destructive-soft px-3 py-2 text-[13px]">
          {validationError}
        </div>
      )}

      {isCreate ? (
        <div className="flex flex-col gap-3">
          <div className="flex flex-col gap-1">
            <label htmlFor="issue-title" className="text-xs font-medium text-soft">Title</label>
            <input
              id="issue-title"
              type="text"
              value={title}
              maxLength={MAX_TITLE}
              placeholder="Short summary"
              disabled={demo || !canCreate}
              onChange={e => { setTitle(e.target.value); setValidationError(null); }}
              className={fieldInput}
            />
          </div>

          <div className="flex flex-col gap-1">
            <label htmlFor="issue-body" className="text-xs font-medium text-soft">Description</label>
            <textarea
              id="issue-body"
              rows={4}
              value={body}
              placeholder="Context, reproduction steps, or acceptance notes (optional)"
              disabled={demo || !canCreate}
              onChange={e => { setBody(e.target.value); setValidationError(null); }}
              className={fieldInput}
            />
          </div>

          <div className="flex flex-col gap-1">
            <label htmlFor="issue-labels" className="text-xs font-medium text-soft">Labels (optional)</label>
            <div className="flex gap-2">
              <input
                id="issue-labels"
                type="text"
                value={labelInput}
                placeholder="bug, documentation"
                disabled={demo || !canCreateMetadata}
                onChange={e => { setLabelInput(e.target.value); setValidationError(null); }}
                onKeyDown={onChipInputKeyDown("label")}
                className={fieldInput}
              />
              <Button type="button" variant="outline" size="sm" onClick={handleAddLabel} disabled={demo || !canCreateMetadata || !labelInput.trim()}>Add label</Button>
            </div>
            {!demo && canCreate && !canCreateMetadata && (
              <span className="text-xs text-muted-foreground">Disabled: adding labels on a new issue requires Push access on GitHub.</span>
            )}
            {createLabels.length > 0 && (
              <div className="mt-1 flex flex-wrap gap-1.5">
                {createLabels.map(label => (
                  <span key={label} className={chipClass}>
                    {label}
                    <button
                      type="button"
                      aria-label={`Remove label ${label}`}
                      disabled={demo || !canCreateMetadata}
                      onClick={() => setCreateLabels(prev => removeIgnoreCase(prev, label))}
                      className="ml-0.5 text-muted-foreground hover:text-foreground"
                    >×</button>
                  </span>
                ))}
              </div>
            )}
          </div>

          <div className="flex flex-col gap-1">
            <label htmlFor="issue-assignees" className="text-xs font-medium text-soft">Assignees (optional)</label>
            <div className="flex gap-2">
              <input
                id="issue-assignees"
                type="text"
                list={suggestId}
                value={assigneeInput}
                placeholder="GitHub username"
                disabled={demo || !canCreateMetadata}
                onChange={e => { setAssigneeInput(e.target.value); setValidationError(null); }}
                onKeyDown={onChipInputKeyDown("assignee")}
                className={fieldInput}
              />
              <Button type="button" variant="outline" size="sm" onClick={handleAddAssignee} disabled={demo || !canCreateMetadata || !assigneeInput.trim()}>Add assignee</Button>
            </div>
            <datalist id={suggestId}>
              {assigneeSuggestions.map(login => <option key={login} value={login} />)}
            </datalist>
            <span className="text-xs text-muted-foreground">
              {!demo && canCreate && !canCreateMetadata
                ? "Disabled: adding assignees on a new issue requires Push access on GitHub."
                : "RepoOrbit checks GitHub assignability before creating the issue."}
            </span>
            {createAssignees.length > 0 && (
              <div className="mt-1 flex flex-wrap gap-1.5">
                {createAssignees.map(login => (
                  <span key={login} className={chipClass}>
                    @{login}
                    <button
                      type="button"
                      aria-label={`Remove assignee ${login}`}
                      disabled={demo || !canCreateMetadata}
                      onClick={() => setCreateAssignees(prev => removeIgnoreCase(prev, login))}
                      className="ml-0.5 text-muted-foreground hover:text-foreground"
                    >×</button>
                  </span>
                ))}
              </div>
            )}
          </div>
        </div>
      ) : (
        <div className="flex flex-col gap-3">
          {issue?.title && (
            <div className="rounded-lg border border-border bg-muted px-3 py-2 text-[13px]">
              <div className="font-medium">{issue.title}</div>
              <div className="mt-0.5 flex flex-wrap items-center gap-2 text-xs text-muted-foreground">
                <span>Current state: <strong className="text-foreground">{issue.state ?? "unknown"}</strong></span>
                {issue.authorLogin && <span>· Author: @{issue.authorLogin}</span>}
                {safeHref(issue.url) && <a href={safeHref(issue.url)!} target="_blank" rel="noreferrer" className="text-primary underline">Open on GitHub ↗</a>}
              </div>
            </div>
          )}

          <div className="flex flex-col gap-1">
            <label htmlFor="issue-state" className="text-xs font-medium text-soft">Open / closed state</label>
            <select
              id="issue-state"
              value={targetState}
              disabled={demo || (!canManage && !canAuthorToggleState)}
              onChange={e => { setTargetState(e.target.value as IssueStateChange); setValidationError(null); }}
              className={fieldInput}
            >
              <option value="">Keep current{issue?.state ? ` (${issue.state})` : ""}</option>
              <option value="open">Open issue</option>
              <option value="closed">Close issue</option>
            </select>
          </div>

          <div className="flex flex-col gap-1.5">
            <label htmlFor="edit-add-labels" className="text-xs font-medium text-soft">Labels (add or remove)</label>
            {currentLabels.length > 0 && (
              <div className="flex flex-wrap gap-1.5">
                {currentLabels.map(name => {
                  const removing = hasIgnoreCase(removeLabels, name);
                  return (
                    <button
                      key={name}
                      type="button"
                      disabled={demo || !canManage}
                      aria-pressed={removing}
                      onClick={() => toggleRemoveCurrentLabel(name)}
                      className={`inline-flex items-center gap-1 rounded-full border px-2.5 py-0.5 text-xs transition-colors disabled:cursor-not-allowed disabled:opacity-50 ${
                        removing
                          ? "border-destructive bg-destructive-soft text-destructive line-through"
                          : "border-border bg-muted text-soft hover:border-border-strong"
                      }`}
                    >
                      {name}
                      <span aria-hidden="true">{removing ? "↺" : "×"}</span>
                    </button>
                  );
                })}
              </div>
            )}
            <div className="flex gap-2">
              <input
                id="edit-add-labels"
                type="text"
                value={labelInput}
                placeholder="Add label name"
                disabled={demo || !canManage}
                onChange={e => { setLabelInput(e.target.value); setValidationError(null); }}
                onKeyDown={onChipInputKeyDown("label")}
                className={fieldInput}
              />
              <Button type="button" variant="outline" size="sm" onClick={handleAddLabel} disabled={demo || !canManage || !labelInput.trim()}>Add label</Button>
            </div>
            {addLabels.length > 0 && (
              <div className="flex flex-wrap gap-1.5">
                {addLabels.map(name => (
                  <span key={name} className="inline-flex items-center gap-1 rounded-full border border-success bg-success-soft px-2.5 py-0.5 text-xs text-success-foreground">
                    + {name}
                    <button type="button" aria-label={`Cancel adding label ${name}`} onClick={() => setAddLabels(prev => removeIgnoreCase(prev, name))}>×</button>
                  </span>
                ))}
              </div>
            )}
          </div>

          <div className="flex flex-col gap-1.5">
            <label htmlFor="edit-add-assignees" className="text-xs font-medium text-soft">Assignees (add or remove)</label>
            {currentAssignees.length > 0 && (
              <div className="flex flex-wrap gap-1.5">
                {currentAssignees.map(login => {
                  const removing = hasIgnoreCase(removeAssignees, login);
                  return (
                    <button
                      key={login}
                      type="button"
                      disabled={demo || !canManage}
                      aria-pressed={removing}
                      onClick={() => toggleRemoveCurrentAssignee(login)}
                      className={`inline-flex items-center gap-1 rounded-full border px-2.5 py-0.5 text-xs transition-colors disabled:cursor-not-allowed disabled:opacity-50 ${
                        removing
                          ? "border-destructive bg-destructive-soft text-destructive line-through"
                          : "border-border bg-muted text-soft hover:border-border-strong"
                      }`}
                    >
                      @{login}
                      <span aria-hidden="true">{removing ? "↺" : "×"}</span>
                    </button>
                  );
                })}
              </div>
            )}
            <div className="flex gap-2">
              <input
                id="edit-add-assignees"
                type="text"
                list={suggestId}
                value={assigneeInput}
                placeholder="Add GitHub username"
                disabled={demo || !canManage}
                onChange={e => { setAssigneeInput(e.target.value); setValidationError(null); }}
                onKeyDown={onChipInputKeyDown("assignee")}
                className={fieldInput}
              />
              <Button type="button" variant="outline" size="sm" onClick={handleAddAssignee} disabled={demo || !canManage || !assigneeInput.trim()}>Add assignee</Button>
            </div>
            <datalist id={suggestId}>
              {assigneeSuggestions.map(login => <option key={login} value={login} />)}
            </datalist>
            {addAssignees.length > 0 && (
              <div className="flex flex-wrap gap-1.5">
                {addAssignees.map(login => (
                  <span key={login} className="inline-flex items-center gap-1 rounded-full border border-success bg-success-soft px-2.5 py-0.5 text-xs text-success-foreground">
                    + @{login}
                    <button type="button" aria-label={`Cancel adding assignee ${login}`} onClick={() => setAddAssignees(prev => removeIgnoreCase(prev, login))}>×</button>
                  </span>
                ))}
              </div>
            )}
            <span className="text-xs text-muted-foreground">Only added or removed values are sent; concurrent changes by teammates are preserved.</span>
          </div>
        </div>
      )}
    </>
  );
}

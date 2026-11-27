import { useCallback, useEffect, useRef, useState, type FormEvent } from "react";
import {
  fetchWriteOperation,
  recheckWriteOperation,
  submitIssueCreate,
  submitIssueUpdate,
  type IssueCreateDraft,
  type IssueUpdateDraft,
  type WriteOperation,
} from "@/features/issues/api";
import { saveEditorDraft, type DraftWriteState } from "@/features/issues/editor-storage";
import { buildIssueDraft, type IssueFields, type Step } from "@/features/issues/model";
import {
  IssueWriteError,
  canReplayWithSameKey,
  classifyWriteFailure,
  generateIdempotencyKey,
} from "@/features/issues/write-errors";
import { removeStorage } from "@/shared/utils/storage";

type IssueWriteOptions = {
  isCreate: boolean;
  issueNumber: number;
  repositoryId: string;
  storageKey: string;
  demo: boolean;
  canEditAnything: boolean;
  fields: IssueFields;
  initial: DraftWriteState;
  setValidationError: (error: string | null) => void;
  onOperationRecorded: (op: WriteOperation) => void;
};

// Owns the write progress of one editor: saves or removes the draft as it changes, and submits, inspects and rechecks
// the GitHub write.
export function useIssueWrite({
  isCreate, issueNumber, repositoryId, storageKey, demo, canEditAnything, fields, initial,
  setValidationError, onOperationRecorded,
}: IssueWriteOptions) {
  const {
    title, body, createLabels, createAssignees, addLabels, removeLabels, addAssignees,
    removeAssignees, targetState,
  } = fields;

  const [step, setStep] = useState<Step>(initial.step);
  const [idempotencyKey, setIdempotencyKey] = useState<string | null>(initial.idempotencyKey);
  const [attemptedAt, setAttemptedAt] = useState<string | null>(initial.attemptedAt);
  const [submittedDraft, setSubmittedDraft] = useState<IssueCreateDraft | IssueUpdateDraft | null>(initial.submittedDraft);
  const [keepKeyOnNextSubmit, setKeepKeyOnNextSubmit] = useState<boolean>(initial.keepKeyOnNextSubmit);
  const [lastError, setLastError] = useState<string | null>(initial.lastError);
  const [lastOperation, setLastOperation] = useState<WriteOperation | null>(initial.lastOperation);
  const [duplicateOperationId, setDuplicateOperationId] = useState<number | null>(initial.duplicateOperationId);

  const [recheckNumber, setRecheckNumber] = useState("");
  const [rechecking, setRechecking] = useState(false);
  const [recheckError, setRecheckError] = useState<string | null>(null);

  const submittingRef = useRef(false);

  const hasAnyDraft = isCreate
    ? Boolean(title.trim() || body.trim() || createLabels.length || createAssignees.length || lastError || lastOperation || duplicateOperationId)
    : Boolean(addLabels.length || removeLabels.length || addAssignees.length || removeAssignees.length || targetState || lastError || lastOperation);

  // The autosave effect depends on this identity changing with every field it saves.
  const saveDraft = useCallback((status: DraftWriteState, opts?: { verify?: boolean }): boolean => saveEditorDraft(
    storageKey,
    isCreate ? "create" : "update",
    issueNumber,
    { title, body, createLabels, createAssignees, addLabels, removeLabels, addAssignees, removeAssignees, targetState },
    status,
    opts,
  ), [
    isCreate, storageKey, title, body, createLabels, createAssignees,
    issueNumber, addLabels, removeLabels, addAssignees, removeAssignees, targetState,
  ]);

  useEffect(() => {
    if (demo || step === "confirmed" || step === "submitting") return;
    if (!hasAnyDraft || (isCreate && step === "failed" && lastOperation?.issueNumber)) {
      removeStorage(storageKey);
      return;
    }
    saveDraft({ step, idempotencyKey, attemptedAt, submittedDraft, keepKeyOnNextSubmit, lastError, lastOperation, duplicateOperationId });
  }, [
    demo, isCreate, hasAnyDraft, storageKey, step, idempotencyKey, attemptedAt, submittedDraft,
    keepKeyOnNextSubmit, lastError, lastOperation, duplicateOperationId, saveDraft,
  ]);

  function resetWrite() {
    setLastError(null);
    setLastOperation(null);
    setDuplicateOperationId(null);
    setIdempotencyKey(null);
    setAttemptedAt(null);
    setSubmittedDraft(null);
    setKeepKeyOnNextSubmit(false);
    setRecheckNumber("");
    setRecheckError(null);
    setStep("edit");
  }

  async function submitWrite(reuseExistingKey = false) {
    if (submittingRef.current || demo) return;
    const existingKey = (reuseExistingKey || keepKeyOnNextSubmit) ? idempotencyKey : null;
    if (reuseExistingKey) {
      if (!existingKey || !canReplayWithSameKey(attemptedAt)) return;
    } else if (!canEditAnything) {
      return;
    }

    const key = existingKey ?? generateIdempotencyKey();
    const attemptStamp = existingKey ? (attemptedAt ?? new Date().toISOString()) : new Date().toISOString();
    const payload = existingKey && submittedDraft ? submittedDraft : buildIssueDraft(isCreate, fields);

    const persisted = saveDraft({
      step: "unknown",
      idempotencyKey: key,
      attemptedAt: attemptStamp,
      submittedDraft: payload,
      keepKeyOnNextSubmit: true,
      lastError: null,
      lastOperation: null,
      duplicateOperationId: null,
    }, { verify: !existingKey });
    if (!existingKey && !persisted) {
      setValidationError("Could not save the recovery key in browser storage, so nothing was sent to GitHub. Check browser storage space or site permissions, then try again.");
      setStep("edit");
      return;
    }

    submittingRef.current = true;
    setStep("submitting");
    setValidationError(null);
    setLastError(null);
    setRecheckError(null);
    setDuplicateOperationId(null);
    setLastOperation(null);
    setIdempotencyKey(key);
    setAttemptedAt(attemptStamp);
    setSubmittedDraft(payload);
    setKeepKeyOnNextSubmit(false);

    try {
      const op = isCreate
        ? await submitIssueCreate(repositoryId, key, payload as IssueCreateDraft)
        : await submitIssueUpdate(repositoryId, issueNumber, key, payload as IssueUpdateDraft);

      setLastOperation(op);
      onOperationRecorded(op);

      if (op.state === "succeeded") {
        removeStorage(storageKey);
        setStep("confirmed");
      } else if (op.state === "unknown" || op.state === "pending") {
        setLastError(op.outcome?.error ?? null);
        setStep("unknown");
      } else {
        if (isCreate && op.issueNumber) {
          removeStorage(storageKey);
        }
        setAttemptedAt(null);
        setSubmittedDraft(null);
        setLastError(op.outcome?.error || "GitHub did not apply every requested change.");
        setStep("failed");
      }
    } catch (error: unknown) {
      const classified = classifyWriteFailure(error, { isReplay: Boolean(existingKey) });
      if (classified.outcome === "duplicate_create") {
        setDuplicateOperationId(classified.operationId);
        setLastError(classified.message);
        setStep("unknown");
      } else if (classified.outcome === "possibly_recorded") {
        setKeepKeyOnNextSubmit(true);
        setLastError(classified.message);
        setStep("unknown");
      } else {
        setKeepKeyOnNextSubmit(false);
        setAttemptedAt(null);
        setSubmittedDraft(null);
        setLastError(classified.message);
        setStep("failed");
      }
    } finally {
      submittingRef.current = false;
    }
  }

  async function inspectDuplicateOperation() {
    if (!duplicateOperationId) return;
    setRechecking(true);
    setRecheckError(null);
    try {
      const op = await fetchWriteOperation(duplicateOperationId);
      setLastOperation(op);
      setDuplicateOperationId(null);
      if (op.state === "succeeded") {
        removeStorage(storageKey);
        setStep("confirmed");
      } else if (op.state === "failed") {
        setLastError(op.outcome?.error ?? "The earlier create failed.");
        setStep("failed");
      } else {
        setLastError(op.outcome?.error ?? null);
        setStep("unknown");
      }
    } catch (error: unknown) {
      setRecheckError(error instanceof Error ? error.message : "Could not load the existing operation.");
    } finally {
      setRechecking(false);
    }
  }

  async function handleRecheck(event?: FormEvent) {
    event?.preventDefault();
    if (!lastOperation || rechecking) return;
    setRecheckError(null);
    let parsedNumber: number | undefined;
    if (lastOperation.kind === "create") {
      parsedNumber = Number.parseInt(recheckNumber.trim(), 10);
      if (!Number.isInteger(parsedNumber) || parsedNumber <= 0) {
        setRecheckError("Enter the positive GitHub issue number you found.");
        return;
      }
    }
    setRechecking(true);
    try {
      const updated = await recheckWriteOperation(lastOperation.id, parsedNumber);
      setLastOperation(updated);
      onOperationRecorded(updated);
      if (updated.state === "succeeded") {
        removeStorage(storageKey);
        setStep("confirmed");
      } else if (updated.state === "failed") {
        if (updated.kind === "create" && updated.issueNumber) {
          removeStorage(storageKey);
        }
        setLastError(updated.outcome?.error || "GitHub did not apply every requested change.");
        setStep("failed");
      } else {
        setStep("unknown");
      }
    } catch (error: unknown) {
      if (error instanceof IssueWriteError && error.kind === "recheck_unresolved") {
        if (error.operation) setLastOperation(error.operation);
        setRecheckError(
          lastOperation.kind === "create"
            ? `Issue #${parsedNumber} could not be matched to this draft (repository, author, title/body, or timestamp did not match). The operation stays unknown and your draft is preserved.`
            : "GitHub does not yet confirm every requested change. The operation stays unknown and your draft is preserved.",
        );
      } else {
        setRecheckError(error instanceof Error ? error.message : "Could not recheck this operation.");
      }
    } finally {
      setRechecking(false);
    }
  }

  return {
    step, setStep, idempotencyKey, attemptedAt, keepKeyOnNextSubmit, lastError, setLastError, lastOperation,
    duplicateOperationId, setDuplicateOperationId, recheckNumber, setRecheckNumber, rechecking, recheckError,
    setRecheckError, hasAnyDraft, resetWrite, submitWrite, inspectDuplicateOperation, handleRecheck,
  };
}

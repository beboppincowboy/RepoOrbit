import { useCallback, useEffect, useId, useMemo, useRef, useState, type FormEvent, type KeyboardEvent } from "react";
import { Button, buttonVariants } from "@/components/ui/button";
import {
  IssueWriteError,
  canReplayWithSameKey,
  classifyWriteFailure,
  fetchWriteOperation,
  generateIdempotencyKey,
  recheckWriteOperation,
  submitIssueCreate,
  submitIssueUpdate,
  type IssueCapabilities,
  type IssueCreateDraft,
  type IssueUpdateDraft,
  type WriteOperation,
  type WriteOperationIssue,
} from "@/lib/api";
import { issueDraftStorageKey, readStorage, removeStorage, writeStorage } from "@/lib/drafts";

const MAX_TITLE = 256;
const MAX_BODY = 65536;
const MAX_ITEMS = 10;
const MAX_LABEL = 50;
const LOGIN_PATTERN = /^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})$/;

export type EditableIssue = {
  number: number;
  title?: string;
  state?: string;
  url?: string;
  authorLogin?: string | null;
  labels?: string[];
  assignees?: string[];
};

type Step = "edit" | "review" | "submitting" | "confirmed" | "unknown" | "failed";

type StoredCreateDraft = {
  version: 1;
  title: string;
  body: string;
  labels: string[];
  assignees: string[];
  idempotencyKey: string | null;
  attemptedAt?: string | null;
  submittedDraft?: IssueCreateDraft | null;
  keepKeyOnNextSubmit: boolean;
  lastError: string | null;
  lastOperation: WriteOperation | null;
  duplicateOperationId: number | null;
  step: Exclude<Step, "submitting" | "confirmed">;
};

type StoredUpdateDraft = {
  version: 1;
  issueNumber: number;
  addLabels: string[];
  removeLabels: string[];
  addAssignees: string[];
  removeAssignees: string[];
  state: "" | "open" | "closed";
  idempotencyKey: string | null;
  attemptedAt?: string | null;
  submittedDraft?: IssueUpdateDraft | null;
  keepKeyOnNextSubmit: boolean;
  lastError: string | null;
  lastOperation: WriteOperation | null;
  step: Exclude<Step, "submitting" | "confirmed">;
};

export function hasSavedCreateDraft(viewerKey: string, repositoryId: string): boolean {
  const saved = readStorage<StoredCreateDraft>(issueDraftStorageKey(viewerKey, repositoryId, "create"));
  if (!saved) return false;
  return Boolean(
    saved.title?.trim()
    || saved.body?.trim()
    || saved.labels?.length
    || saved.assignees?.length
    || saved.lastError
    || saved.lastOperation
    || saved.duplicateOperationId,
  );
}

function safeHref(url?: string | null): string | null {
  if (!url) return null;
  return /^https?:\/\//i.test(url) ? url : null;
}

function hasIgnoreCase(list: string[], value: string): boolean {
  const lower = value.toLowerCase();
  return list.some(item => item.toLowerCase() === lower);
}

function removeIgnoreCase(list: string[], value: string): string[] {
  const lower = value.toLowerCase();
  return list.filter(item => item.toLowerCase() !== lower);
}

function validateLabel(raw: string): string | null {
  if (!raw || raw.trim() !== raw || raw.length > MAX_LABEL) {
    return `Labels must be 1 to ${MAX_LABEL} characters with no surrounding spaces.`;
  }
  return null;
}

function validateLogin(raw: string): string | null {
  if (!LOGIN_PATTERN.test(raw)) {
    return `"${raw}" is not a valid GitHub login.`;
  }
  return null;
}

function splitTokens(raw: string): string[] {
  return raw.split(",").map(part => part.trim()).filter(Boolean);
}

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

const fieldInput = "w-full rounded-lg border border-border bg-card px-2.5 py-1.5 text-[13px] text-foreground placeholder:text-muted-foreground disabled:cursor-not-allowed disabled:bg-muted disabled:opacity-65";
const chipClass = "inline-flex items-center gap-1 rounded-full border border-border bg-muted px-2.5 py-0.5 text-xs text-soft";
const smallBtn = buttonVariants({ variant: "outline", size: "sm" });

export function IssueEditor(props: IssueEditorProps) {
  const {
    mode,
    issue,
    viewerKey,
    viewerLogin,
    repositoryId,
    fullName,
    demo,
    archived,
    capabilities,
    capabilitiesLoading,
    capabilitiesError,
    onRetryCapabilities,
    assigneeSuggestions,
    onClose,
    onOperationRecorded,
    onSwitchToEditIssue,
  } = props;

  const headingId = useId();
  const suggestId = useId();
  const isCreate = mode === "create";
  const issueNumber = issue?.number ?? 0;
  const storageKey = issueDraftStorageKey(viewerKey, repositoryId, isCreate ? "create" : issueNumber);

  const initialCreate = useMemo<StoredCreateDraft | null>(() => {
    if (demo || !isCreate) return null;
    return readStorage<StoredCreateDraft>(storageKey);
  }, [demo, isCreate, storageKey]);

  const initialUpdate = useMemo<StoredUpdateDraft | null>(() => {
    if (demo || isCreate) return null;
    return readStorage<StoredUpdateDraft>(storageKey);
  }, [demo, isCreate, storageKey]);

  const [title, setTitle] = useState(() => initialCreate?.title ?? "");
  const [body, setBody] = useState(() => initialCreate?.body ?? "");
  const [createLabels, setCreateLabels] = useState<string[]>(() => initialCreate?.labels ?? []);
  const [createAssignees, setCreateAssignees] = useState<string[]>(() => initialCreate?.assignees ?? []);

  const [addLabels, setAddLabels] = useState<string[]>(() => initialUpdate?.addLabels ?? []);
  const [removeLabels, setRemoveLabels] = useState<string[]>(() => initialUpdate?.removeLabels ?? []);
  const [addAssignees, setAddAssignees] = useState<string[]>(() => initialUpdate?.addAssignees ?? []);
  const [removeAssignees, setRemoveAssignees] = useState<string[]>(() => initialUpdate?.removeAssignees ?? []);
  const [targetState, setTargetState] = useState<"" | "open" | "closed">(() => initialUpdate?.state ?? "");

  const [labelInput, setLabelInput] = useState("");
  const [assigneeInput, setAssigneeInput] = useState("");
  const [validationError, setValidationError] = useState<string | null>(null);

  const [step, setStep] = useState<Step>(() => (isCreate ? initialCreate?.step : initialUpdate?.step) ?? "edit");
  const [idempotencyKey, setIdempotencyKey] = useState<string | null>(() => (isCreate ? initialCreate?.idempotencyKey : initialUpdate?.idempotencyKey) ?? null);
  const [attemptedAt, setAttemptedAt] = useState<string | null>(() => (isCreate ? initialCreate?.attemptedAt : initialUpdate?.attemptedAt) ?? null);
  const [submittedDraft, setSubmittedDraft] = useState<IssueCreateDraft | IssueUpdateDraft | null>(
    () => (isCreate ? initialCreate?.submittedDraft : initialUpdate?.submittedDraft) ?? null,
  );
  const [keepKeyOnNextSubmit, setKeepKeyOnNextSubmit] = useState<boolean>(() => (isCreate ? initialCreate?.keepKeyOnNextSubmit : initialUpdate?.keepKeyOnNextSubmit) ?? false);
  const [lastError, setLastError] = useState<string | null>(() => (isCreate ? initialCreate?.lastError : initialUpdate?.lastError) ?? null);
  const [lastOperation, setLastOperation] = useState<WriteOperation | null>(() => (isCreate ? initialCreate?.lastOperation : initialUpdate?.lastOperation) ?? null);
  const [duplicateOperationId, setDuplicateOperationId] = useState<number | null>(() => initialCreate?.duplicateOperationId ?? null);

  const [recheckNumber, setRecheckNumber] = useState("");
  const [rechecking, setRechecking] = useState(false);
  const [recheckError, setRecheckError] = useState<string | null>(null);

  const submittingRef = useRef(false);

  const isAuthor = Boolean(viewerLogin && issue?.authorLogin && viewerLogin.toLowerCase() === issue.authorLogin.toLowerCase());
  const canCreate = !demo && !archived && Boolean(capabilities?.canCreateIssue);
  const canCreateMetadata = !demo && !archived && Boolean(capabilities?.canCreateIssueWithMetadata);
  const canManage = !demo && !archived && Boolean(capabilities?.canManageIssues);
  const canAuthorToggleState = !demo && !archived && !canManage && Boolean(capabilities?.canCreateIssue && isAuthor);
  const canEditAnything = isCreate ? canCreate : (canManage || canAuthorToggleState);

  const permissionNotice = useMemo<string | null>(() => {
    if (demo) return null;
    if (archived) return "This repository is archived on GitHub and does not accept issue writes.";
    if (capabilitiesLoading) return null;
    if (capabilitiesError) return capabilitiesError;
    if (!capabilities) return "GitHub permissions could not be confirmed for this repository.";
    if (isCreate) {
      if (!capabilities.canCreateIssue) return "Your current GitHub permission does not allow creating issues in this repository.";
      if (!capabilities.canCreateIssueWithMetadata) return "You can create an issue with a title and description. Adding labels or assignees on a new issue requires Push access on GitHub.";
      return null;
    }
    if (capabilities.canManageIssues) return null;
    if (capabilities.canCreateIssue && isAuthor) {
      return "With Read access you can close or reopen issues you authored. Changing labels or assignees requires Triage or higher permission on GitHub.";
    }
    return "Changing labels, assignees, or issue state requires Triage or higher permission on GitHub.";
  }, [demo, archived, capabilitiesLoading, capabilitiesError, capabilities, isCreate, isAuthor]);

  const hasAnyDraft = isCreate
    ? Boolean(title.trim() || body.trim() || createLabels.length || createAssignees.length || lastError || lastOperation || duplicateOperationId)
    : Boolean(addLabels.length || removeLabels.length || addAssignees.length || removeAssignees.length || targetState || lastError || lastOperation);

  const saveDraft = useCallback((status: {
    step: Exclude<Step, "submitting" | "confirmed">;
    idempotencyKey: string | null;
    attemptedAt: string | null;
    submittedDraft: IssueCreateDraft | IssueUpdateDraft | null;
    keepKeyOnNextSubmit: boolean;
    lastError: string | null;
    lastOperation: WriteOperation | null;
    duplicateOperationId: number | null;
  }, opts?: { verify?: boolean }): boolean => {
    const { duplicateOperationId, submittedDraft: draftPayload, ...writeState } = status;
    if (isCreate) {
      return writeStorage(storageKey, {
        version: 1, title, body, labels: createLabels, assignees: createAssignees,
        duplicateOperationId,
        submittedDraft: draftPayload as IssueCreateDraft | null,
        ...writeState,
      } satisfies StoredCreateDraft, opts);
    }
    return writeStorage(storageKey, {
      version: 1, issueNumber, addLabels, removeLabels, addAssignees, removeAssignees,
      state: targetState,
      submittedDraft: draftPayload as IssueUpdateDraft | null,
      ...writeState,
    } satisfies StoredUpdateDraft, opts);
  }, [
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

  useEffect(() => {
    function onKey(event: globalThis.KeyboardEvent) {
      if (event.key === "Escape" && step !== "submitting") onClose();
    }
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [step, onClose]);

  const currentLabels = issue?.labels ?? [];
  const currentAssignees = issue?.assignees ?? [];

  function addTokens(
    raw: string,
    existingAdd: string[],
    existingRemove: string[],
    validate: (token: string) => string | null,
    itemPlural: string,
  ): { nextAdd: string[]; nextRemove: string[]; error: string | null } {
    const tokens = splitTokens(raw);
    let nextAdd = [...existingAdd];
    let nextRemove = [...existingRemove];
    for (const token of tokens) {
      const err = validate(token);
      if (err) return { nextAdd, nextRemove, error: err };
      if (hasIgnoreCase(nextRemove, token)) {
        nextRemove = removeIgnoreCase(nextRemove, token);
        continue;
      }
      if (hasIgnoreCase(nextAdd, token)) continue;
      if (nextAdd.length >= MAX_ITEMS) {
        return { nextAdd, nextRemove, error: `At most ${MAX_ITEMS} ${itemPlural} can be added at once.` };
      }
      nextAdd = [...nextAdd, token];
    }
    return { nextAdd, nextRemove, error: null };
  }

  function handleAddChips(kind: "label" | "assignee") {
    const config = kind === "label"
      ? {
          input: labelInput,
          clearInput: () => setLabelInput(""),
          list: isCreate ? createLabels : addLabels,
          setList: isCreate ? setCreateLabels : setAddLabels,
          removeList: isCreate ? [] : removeLabels,
          setRemoveList: isCreate ? null : setRemoveLabels,
          validate: validateLabel,
          itemPlural: "labels",
        }
      : {
          input: assigneeInput,
          clearInput: () => setAssigneeInput(""),
          list: isCreate ? createAssignees : addAssignees,
          setList: isCreate ? setCreateAssignees : setAddAssignees,
          removeList: isCreate ? [] : removeAssignees,
          setRemoveList: isCreate ? null : setRemoveAssignees,
          validate: validateLogin,
          itemPlural: "assignees",
        };
    if (!config.input.trim()) return;
    const res = addTokens(config.input, config.list, config.removeList, config.validate, config.itemPlural);
    if (res.error) { setValidationError(res.error); return; }
    config.setList(res.nextAdd);
    config.setRemoveList?.(res.nextRemove);
    config.clearInput();
    setValidationError(null);
  }

  function handleAddLabel() {
    handleAddChips("label");
  }

  function handleAddAssignee() {
    handleAddChips("assignee");
  }

  function onChipInputKeyDown(kind: "label" | "assignee") {
    return (event: KeyboardEvent<HTMLInputElement>) => {
      if (event.key === "Enter") {
        event.preventDefault();
        handleAddChips(kind);
      }
    };
  }

  function toggleRemoval(
    value: string,
    removeList: string[],
    setAddList: (updater: (prev: string[]) => string[]) => void,
    setRemoveList: (next: string[] | ((prev: string[]) => string[])) => void,
    itemPlural: string,
  ) {
    setValidationError(null);
    if (hasIgnoreCase(removeList, value)) {
      setRemoveList(removeIgnoreCase(removeList, value));
      return;
    }
    if (removeList.length >= MAX_ITEMS) {
      setValidationError(`At most ${MAX_ITEMS} ${itemPlural} can be removed at once.`);
      return;
    }
    setAddList(prev => removeIgnoreCase(prev, value));
    setRemoveList(prev => [...prev, value]);
  }

  function toggleRemoveCurrentLabel(name: string) {
    toggleRemoval(name, removeLabels, setAddLabels, setRemoveLabels, "labels");
  }

  function toggleRemoveCurrentAssignee(login: string) {
    toggleRemoval(login, removeAssignees, setAddAssignees, setRemoveAssignees, "assignees");
  }

  function discardDraft() {
    removeStorage(storageKey);
    setTitle("");
    setBody("");
    setCreateLabels([]);
    setCreateAssignees([]);
    setAddLabels([]);
    setRemoveLabels([]);
    setAddAssignees([]);
    setRemoveAssignees([]);
    setTargetState("");
    setLabelInput("");
    setAssigneeInput("");
    setValidationError(null);
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

  function goToReview() {
    if (demo || !canEditAnything) return;
    setValidationError(null);

    if (isCreate) {
      let nextLabels = createLabels;
      let nextAssignees = createAssignees;
      if (canCreateMetadata && labelInput.trim()) {
        const res = addTokens(labelInput, nextLabels, [], validateLabel, "labels");
        if (res.error) { setValidationError(res.error); return; }
        nextLabels = res.nextAdd;
        setCreateLabels(nextLabels);
        setLabelInput("");
      }
      if (canCreateMetadata && assigneeInput.trim()) {
        const res = addTokens(assigneeInput, nextAssignees, [], validateLogin, "assignees");
        if (res.error) { setValidationError(res.error); return; }
        nextAssignees = res.nextAdd;
        setCreateAssignees(nextAssignees);
        setAssigneeInput("");
      }
      const trimmedTitle = title.trim();
      if (!trimmedTitle || trimmedTitle.length > MAX_TITLE) {
        setValidationError(`Title must be 1 to ${MAX_TITLE} characters.`);
        return;
      }
      if (body.length > MAX_BODY) {
        setValidationError(`Description must be at most ${MAX_BODY} characters.`);
        return;
      }
      if (!canCreateMetadata && (nextLabels.length > 0 || nextAssignees.length > 0)) {
        setValidationError("Adding labels or assignees on a new issue requires Push access on GitHub.");
        return;
      }
      setLastError(null);
      setDuplicateOperationId(null);
      setStep("review");
      return;
    }

    let nextAddLabels = addLabels;
    let nextRemoveLabels = removeLabels;
    let nextAddAssignees = addAssignees;
    let nextRemoveAssignees = removeAssignees;
    if (canManage && labelInput.trim()) {
      const res = addTokens(labelInput, nextAddLabels, nextRemoveLabels, validateLabel, "labels");
      if (res.error) { setValidationError(res.error); return; }
      nextAddLabels = res.nextAdd;
      nextRemoveLabels = res.nextRemove;
      setAddLabels(nextAddLabels);
      setRemoveLabels(nextRemoveLabels);
      setLabelInput("");
    }
    if (canManage && assigneeInput.trim()) {
      const res = addTokens(assigneeInput, nextAddAssignees, nextRemoveAssignees, validateLogin, "assignees");
      if (res.error) { setValidationError(res.error); return; }
      nextAddAssignees = res.nextAdd;
      nextRemoveAssignees = res.nextRemove;
      setAddAssignees(nextAddAssignees);
      setRemoveAssignees(nextRemoveAssignees);
      setAssigneeInput("");
    }
    const touchesMetadata = nextAddLabels.length + nextRemoveLabels.length + nextAddAssignees.length + nextRemoveAssignees.length > 0;
    if (!canManage && touchesMetadata) {
      setValidationError("Changing labels or assignees requires Triage or higher permission on GitHub.");
      return;
    }
    if (!touchesMetadata && !targetState) {
      setValidationError("Choose at least one label, assignee, or state change to review.");
      return;
    }
    setLastError(null);
    setStep("review");
  }

  function buildCurrentDraft(): IssueCreateDraft | IssueUpdateDraft {
    if (isCreate) {
      const draft: IssueCreateDraft = { title: title.trim() };
      if (body) draft.body = body;
      if (createLabels.length) draft.labels = createLabels;
      if (createAssignees.length) draft.assignees = createAssignees;
      return draft;
    }
    const draft: IssueUpdateDraft = {};
    if (addLabels.length || removeLabels.length) {
      draft.labels = {
        ...(addLabels.length ? { add: addLabels } : {}),
        ...(removeLabels.length ? { remove: removeLabels } : {}),
      };
    }
    if (addAssignees.length || removeAssignees.length) {
      draft.assignees = {
        ...(addAssignees.length ? { add: addAssignees } : {}),
        ...(removeAssignees.length ? { remove: removeAssignees } : {}),
      };
    }
    if (targetState) draft.state = targetState;
    return draft;
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
    const payload = existingKey && submittedDraft ? submittedDraft : buildCurrentDraft();

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

  const fields = {
    title, body, createLabels, createAssignees, addLabels, removeLabels, addAssignees,
    removeAssignees, targetState,
  };
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

type IssueFields = Pick<StoredCreateDraft, "title" | "body">
  & Pick<StoredUpdateDraft, "addLabels" | "removeLabels" | "addAssignees" | "removeAssignees">
  & { createLabels: string[]; createAssignees: string[]; targetState: StoredUpdateDraft["state"] };

type EditStepProps = {
  fields: IssueFields;
  issue: IssueEditorProps["issue"];
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
  setTargetState: (state: StoredUpdateDraft["state"]) => void;
  setValidationError: (error: string | null) => void;
  setLabelInput: (value: string) => void;
  setAssigneeInput: (value: string) => void;
  handleAddLabel: () => void;
  handleAddAssignee: () => void;
  onChipInputKeyDown: (kind: "label" | "assignee") => (event: KeyboardEvent<HTMLInputElement>) => void;
  toggleRemoveCurrentLabel: (value: string) => void;
  toggleRemoveCurrentAssignee: (value: string) => void;
};

function EditStep({
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
              onChange={e => { setTargetState(e.target.value as "" | "open" | "closed"); setValidationError(null); }}
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

type ReviewStepProps = {
  fields: IssueFields;
  isCreate: boolean;
  fullName: string;
  issueNumber: number;
  viewerLogin: string | null;
};

function ReviewStep({
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

function UnknownStep({
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

type FailedStepProps = {
  lastError: string | null;
  lastOperation: WriteOperation | null;
  confirmedIssue: WriteOperationIssue | null;
  confirmedIssueHref: string | null;
  onSwitchToEditIssue: IssueEditorProps["onSwitchToEditIssue"];
};

function FailedStep({
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

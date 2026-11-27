import type { IssueCreateDraft, IssueUpdateDraft, WriteOperation } from "@/features/issues/api";
import { issueDraftStorageKey } from "@/features/issues/drafts";
import type { IssueFields, IssueStateChange, Step } from "@/features/issues/model";
import { readStorage, writeStorage } from "@/shared/utils/storage";

// A submitting or confirmed editor is never saved.
type SavedStep = Exclude<Step, "submitting" | "confirmed">;

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
  step: SavedStep;
};

type StoredUpdateDraft = {
  version: 1;
  issueNumber: number;
  addLabels: string[];
  removeLabels: string[];
  addAssignees: string[];
  removeAssignees: string[];
  state: IssueStateChange;
  idempotencyKey: string | null;
  attemptedAt?: string | null;
  submittedDraft?: IssueUpdateDraft | null;
  keepKeyOnNextSubmit: boolean;
  lastError: string | null;
  lastOperation: WriteOperation | null;
  step: SavedStep;
};

// The write progress saved next to the fields. Update drafts do not store duplicateOperationId.
export type DraftWriteState = {
  step: SavedStep;
  idempotencyKey: string | null;
  attemptedAt: string | null;
  submittedDraft: IssueCreateDraft | IssueUpdateDraft | null;
  keepKeyOnNextSubmit: boolean;
  lastError: string | null;
  lastOperation: WriteOperation | null;
  duplicateOperationId: number | null;
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

// The saved draft under storageKey, with defaults for anything missing. A null storageKey reads nothing.
export function loadEditorDraft(storageKey: string | null, mode: "create" | "update"): { fields: IssueFields; write: DraftWriteState } {
  const isCreate = mode === "create";
  const initialCreate = storageKey && isCreate ? readStorage<StoredCreateDraft>(storageKey) : null;
  const initialUpdate = storageKey && !isCreate ? readStorage<StoredUpdateDraft>(storageKey) : null;
  const saved = isCreate ? initialCreate : initialUpdate;
  return {
    fields: {
      title: initialCreate?.title ?? "",
      body: initialCreate?.body ?? "",
      createLabels: initialCreate?.labels ?? [],
      createAssignees: initialCreate?.assignees ?? [],
      addLabels: initialUpdate?.addLabels ?? [],
      removeLabels: initialUpdate?.removeLabels ?? [],
      addAssignees: initialUpdate?.addAssignees ?? [],
      removeAssignees: initialUpdate?.removeAssignees ?? [],
      targetState: initialUpdate?.state ?? "",
    },
    write: {
      step: saved?.step ?? "edit",
      idempotencyKey: saved?.idempotencyKey ?? null,
      attemptedAt: saved?.attemptedAt ?? null,
      submittedDraft: saved?.submittedDraft ?? null,
      keepKeyOnNextSubmit: saved?.keepKeyOnNextSubmit ?? false,
      lastError: saved?.lastError ?? null,
      lastOperation: saved?.lastOperation ?? null,
      duplicateOperationId: initialCreate?.duplicateOperationId ?? null,
    },
  };
}

// False when the write fails, or, with verify, when reading it back does not match. issueNumber is stored for updates only.
export function saveEditorDraft(
  storageKey: string,
  mode: "create" | "update",
  issueNumber: number,
  fields: IssueFields,
  status: DraftWriteState,
  opts?: { verify?: boolean },
): boolean {
  const { duplicateOperationId, submittedDraft: draftPayload, ...writeState } = status;
  if (mode === "create") {
    return writeStorage(storageKey, {
      version: 1, title: fields.title, body: fields.body, labels: fields.createLabels, assignees: fields.createAssignees,
      duplicateOperationId,
      submittedDraft: draftPayload as IssueCreateDraft | null,
      ...writeState,
    } satisfies StoredCreateDraft, opts);
  }
  return writeStorage(storageKey, {
    version: 1, issueNumber, addLabels: fields.addLabels, removeLabels: fields.removeLabels,
    addAssignees: fields.addAssignees, removeAssignees: fields.removeAssignees,
    state: fields.targetState,
    submittedDraft: draftPayload as IssueUpdateDraft | null,
    ...writeState,
  } satisfies StoredUpdateDraft, opts);
}

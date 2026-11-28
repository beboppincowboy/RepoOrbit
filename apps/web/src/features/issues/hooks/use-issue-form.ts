import { useEffect, useMemo, useState, type KeyboardEvent } from "react";
import type { IssueCapabilities, WriteOperation } from "@/features/issues/api";
import { issueDraftStorageKey } from "@/features/issues/drafts";
import { loadEditorDraft } from "@/features/issues/editor-storage";
import { useIssueWrite } from "@/features/issues/hooks/use-issue-write";
import {
  MAX_ITEMS,
  addTokens,
  getPermissionNotice,
  hasIgnoreCase,
  removeIgnoreCase,
  reviewIssueChanges,
  validateLabel,
  validateLogin,
  type EditableIssue,
  type IssueStateChange,
} from "@/features/issues/model";
import { removeStorage } from "@/shared/utils/storage";

type IssueFormOptions = {
  mode: "create" | "update";
  issue?: EditableIssue;
  viewerKey: string;
  viewerLogin: string | null;
  repositoryId: string;
  demo: boolean;
  archived: boolean;
  capabilities: IssueCapabilities | null;
  capabilitiesLoading: boolean;
  capabilitiesError: string | null;
  onClose: () => void;
  onOperationRecorded: (op: WriteOperation) => void;
};

// The whole state of one issue editor: form fields, permissions, review and discard, the write from useIssueWrite, and
// closing on Escape. Reads the saved draft on mount only, so the caller must remount it for another issue.
export function useIssueForm({
  mode, issue, viewerKey, viewerLogin, repositoryId, demo, archived, capabilities, capabilitiesLoading,
  capabilitiesError, onClose, onOperationRecorded,
}: IssueFormOptions) {
  const isCreate = mode === "create";
  const issueNumber = issue?.number ?? 0;
  const storageKey = issueDraftStorageKey(viewerKey, repositoryId, isCreate ? "create" : issueNumber);

  const [saved] = useState(() => loadEditorDraft(demo ? null : storageKey, mode));

  const [title, setTitle] = useState(saved.fields.title);
  const [body, setBody] = useState(saved.fields.body);
  const [createLabels, setCreateLabels] = useState<string[]>(saved.fields.createLabels);
  const [createAssignees, setCreateAssignees] = useState<string[]>(saved.fields.createAssignees);

  const [addLabels, setAddLabels] = useState<string[]>(saved.fields.addLabels);
  const [removeLabels, setRemoveLabels] = useState<string[]>(saved.fields.removeLabels);
  const [addAssignees, setAddAssignees] = useState<string[]>(saved.fields.addAssignees);
  const [removeAssignees, setRemoveAssignees] = useState<string[]>(saved.fields.removeAssignees);
  const [targetState, setTargetState] = useState<IssueStateChange>(saved.fields.targetState);

  const [labelInput, setLabelInput] = useState("");
  const [assigneeInput, setAssigneeInput] = useState("");
  const [validationError, setValidationError] = useState<string | null>(null);

  const isAuthor = Boolean(viewerLogin && issue?.authorLogin && viewerLogin.toLowerCase() === issue.authorLogin.toLowerCase());
  const canCreate = !demo && !archived && Boolean(capabilities?.canCreateIssue);
  const canCreateMetadata = !demo && !archived && Boolean(capabilities?.canCreateIssueWithMetadata);
  const canManage = !demo && !archived && Boolean(capabilities?.canManageIssues);
  const canAuthorToggleState = !demo && !archived && !canManage && Boolean(capabilities?.canCreateIssue && isAuthor);
  const canEditAnything = isCreate ? canCreate : (canManage || canAuthorToggleState);

  const fields = {
    title, body, createLabels, createAssignees, addLabels, removeLabels, addAssignees,
    removeAssignees, targetState,
  };

  const { resetWrite, setLastError, setDuplicateOperationId, ...write } = useIssueWrite({
    isCreate, issueNumber, repositoryId, storageKey, demo, canEditAnything, fields, initial: saved.write,
    setValidationError, onOperationRecorded,
  });
  const { step, setStep } = write;

  const permissionNotice = useMemo<string | null>(
    () => getPermissionNotice({ demo, archived, capabilitiesLoading, capabilitiesError, capabilities, isCreate, isAuthor }),
    [demo, archived, capabilitiesLoading, capabilitiesError, capabilities, isCreate, isAuthor],
  );

  useEffect(() => {
    function onKey(event: globalThis.KeyboardEvent) {
      if (event.key === "Escape" && step !== "submitting") onClose();
    }
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [step, onClose]);

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
    resetWrite();
  }

  function goToReview() {
    if (demo || !canEditAnything) return;
    setValidationError(null);

    const review = reviewIssueChanges({ isCreate, fields, labelInput, assigneeInput, canCreateMetadata, canManage });
    if (review.labels) {
      if (isCreate) {
        setCreateLabels(review.labels.add);
      } else {
        setAddLabels(review.labels.add);
        setRemoveLabels(review.labels.remove);
      }
      setLabelInput("");
    }
    if (review.assignees) {
      if (isCreate) {
        setCreateAssignees(review.assignees.add);
      } else {
        setAddAssignees(review.assignees.add);
        setRemoveAssignees(review.assignees.remove);
      }
      setAssigneeInput("");
    }
    if (review.error) { setValidationError(review.error); return; }
    setLastError(null);
    if (isCreate) setDuplicateOperationId(null);
    setStep("review");
  }

  return {
    ...write,
    isCreate, issueNumber, fields, canCreate, canCreateMetadata, canManage, canAuthorToggleState, canEditAnything,
    permissionNotice, validationError, setValidationError, labelInput, setLabelInput, assigneeInput, setAssigneeInput,
    setTitle, setBody, setCreateLabels, setCreateAssignees, setAddLabels, setAddAssignees,
    setTargetState, handleAddLabel, handleAddAssignee, onChipInputKeyDown, toggleRemoveCurrentLabel,
    toggleRemoveCurrentAssignee, discardDraft, goToReview,
  };
}

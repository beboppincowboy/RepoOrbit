import type { IssueCapabilities, IssueCreateDraft, IssueUpdateDraft } from "@/features/issues/api";

export const MAX_TITLE = 256;
const MAX_BODY = 65536;
export const MAX_ITEMS = 10;
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

export type Step = "edit" | "review" | "submitting" | "confirmed" | "unknown" | "failed";

export type IssueStateChange = "" | "open" | "closed";

// Everything the editor form holds for either mode; the other mode's fields stay empty.
export type IssueFields = {
  title: string;
  body: string;
  createLabels: string[];
  createAssignees: string[];
  addLabels: string[];
  removeLabels: string[];
  addAssignees: string[];
  removeAssignees: string[];
  targetState: IssueStateChange;
};

export function safeHref(url?: string | null): string | null {
  if (!url) return null;
  return /^https?:\/\//i.test(url) ? url : null;
}

export function hasIgnoreCase(list: string[], value: string): boolean {
  const lower = value.toLowerCase();
  return list.some(item => item.toLowerCase() === lower);
}

export function removeIgnoreCase(list: string[], value: string): string[] {
  const lower = value.toLowerCase();
  return list.filter(item => item.toLowerCase() !== lower);
}

export function validateLabel(raw: string): string | null {
  if (!raw || raw.trim() !== raw || raw.length > MAX_LABEL) {
    return `Labels must be 1 to ${MAX_LABEL} characters with no surrounding spaces.`;
  }
  return null;
}

export function validateLogin(raw: string): string | null {
  if (!LOGIN_PATTERN.test(raw)) {
    return `"${raw}" is not a valid GitHub login.`;
  }
  return null;
}

export function splitTokens(raw: string): string[] {
  return raw.split(",").map(part => part.trim()).filter(Boolean);
}

export function addTokens(
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

// The payload that submitting the form as it stands would send.
export function buildIssueDraft(isCreate: boolean, fields: IssueFields): IssueCreateDraft | IssueUpdateDraft {
  const { title, body, createLabels, createAssignees, addLabels, removeLabels, addAssignees, removeAssignees, targetState } = fields;
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

export function getPermissionNotice({ demo, archived, capabilitiesLoading, capabilitiesError, capabilities, isCreate, isAuthor }: {
  demo: boolean;
  archived: boolean;
  capabilitiesLoading: boolean;
  capabilitiesError: string | null;
  capabilities: IssueCapabilities | null;
  isCreate: boolean;
  isAuthor: boolean;
}): string | null {
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
}

export type TokenLists = { add: string[]; remove: string[] };

// labels and assignees are non-null when that pending input was accepted: the caller must then store the lists and
// clear the input, even if a later check set error.
export type IssueReview = { labels: TokenLists | null; assignees: TokenLists | null; error: string | null };

export function reviewIssueChanges({ isCreate, fields, labelInput, assigneeInput, canCreateMetadata, canManage }: {
  isCreate: boolean;
  fields: IssueFields;
  labelInput: string;
  assigneeInput: string;
  canCreateMetadata: boolean;
  canManage: boolean;
}): IssueReview {
  const { title, body, createLabels, createAssignees, addLabels, removeLabels, addAssignees, removeAssignees, targetState } = fields;
  let labels: TokenLists | null = null;
  let assignees: TokenLists | null = null;
  const result = (error: string | null): IssueReview => ({ labels, assignees, error });

  if (isCreate) {
    let nextLabels = createLabels;
    let nextAssignees = createAssignees;
    if (canCreateMetadata && labelInput.trim()) {
      const res = addTokens(labelInput, nextLabels, [], validateLabel, "labels");
      if (res.error) return result(res.error);
      nextLabels = res.nextAdd;
      labels = { add: nextLabels, remove: [] };
    }
    if (canCreateMetadata && assigneeInput.trim()) {
      const res = addTokens(assigneeInput, nextAssignees, [], validateLogin, "assignees");
      if (res.error) return result(res.error);
      nextAssignees = res.nextAdd;
      assignees = { add: nextAssignees, remove: [] };
    }
    const trimmedTitle = title.trim();
    if (!trimmedTitle || trimmedTitle.length > MAX_TITLE) {
      return result(`Title must be 1 to ${MAX_TITLE} characters.`);
    }
    if (body.length > MAX_BODY) {
      return result(`Description must be at most ${MAX_BODY} characters.`);
    }
    if (!canCreateMetadata && (nextLabels.length > 0 || nextAssignees.length > 0)) {
      return result("Adding labels or assignees on a new issue requires Push access on GitHub.");
    }
    return result(null);
  }

  let nextAddLabels = addLabels;
  let nextRemoveLabels = removeLabels;
  let nextAddAssignees = addAssignees;
  let nextRemoveAssignees = removeAssignees;
  if (canManage && labelInput.trim()) {
    const res = addTokens(labelInput, nextAddLabels, nextRemoveLabels, validateLabel, "labels");
    if (res.error) return result(res.error);
    nextAddLabels = res.nextAdd;
    nextRemoveLabels = res.nextRemove;
    labels = { add: nextAddLabels, remove: nextRemoveLabels };
  }
  if (canManage && assigneeInput.trim()) {
    const res = addTokens(assigneeInput, nextAddAssignees, nextRemoveAssignees, validateLogin, "assignees");
    if (res.error) return result(res.error);
    nextAddAssignees = res.nextAdd;
    nextRemoveAssignees = res.nextRemove;
    assignees = { add: nextAddAssignees, remove: nextRemoveAssignees };
  }
  const touchesMetadata = nextAddLabels.length + nextRemoveLabels.length + nextAddAssignees.length + nextRemoveAssignees.length > 0;
  if (!canManage && touchesMetadata) {
    return result("Changing labels or assignees requires Triage or higher permission on GitHub.");
  }
  if (!touchesMetadata && !targetState) {
    return result("Choose at least one label, assignee, or state change to review.");
  }
  return result(null);
}

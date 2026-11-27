import { useMemo, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { fetchAssignableUsers, fetchIssueCapabilities, fetchWorkspaceIssue, type WriteOperationIssue } from "@/features/issues/api";
import { hasSavedCreateDraft } from "@/features/issues/editor-storage";
import { toEditableIssue, type EditableIssue } from "@/features/issues/model";
import { assignableUsersKey, issueCapabilitiesKey, workspaceIssueKey } from "@/features/issues/query-keys";

type WorkspaceEditorOptions = {
  viewerKey: string;
  repositoryId: string;
  demo: boolean;
  /** A saved snapshot is shown, so nothing is read live. */
  frozen: boolean;
  /** Missing until the repository overview has loaded. */
  overview: { contributors: { login: string; isBot: boolean }[] } | null | undefined;
  tab: string;
  /** The `editor` search value: a new issue, an issue number, or closed. */
  editor: "new" | number | undefined;
  onShowEditor: (editor: "new" | number) => void;
  onHideEditor: () => void;
};

// The workspace's issue editor: which issue it edits, the reads it and the Board need, and whether a create draft is saved.
export function useWorkspaceEditor({ viewerKey, repositoryId, demo, frozen, overview, tab, editor, onShowEditor, onHideEditor }: WorkspaceEditorOptions) {
  const [selectedEditIssue, setSelectedEditIssue] = useState<EditableIssue | null>(null);
  const [draftRevision, setDraftRevision] = useState(0);
  const editorOpen = editor !== undefined;
  const capabilitiesQuery = useQuery({
    queryKey: issueCapabilitiesKey(viewerKey, repositoryId),
    queryFn: ({ signal }) => fetchIssueCapabilities(repositoryId, signal),
    enabled: !demo && !frozen && Boolean(overview) && (editorOpen || tab === "issues" || tab === "board"),
    gcTime: 0,
    retry: false,
  });
  const restoredNumber = typeof editor === "number" && selectedEditIssue?.number !== editor ? editor : null;
  const restoredIssueQuery = useQuery({
    queryKey: workspaceIssueKey(viewerKey, repositoryId, restoredNumber),
    queryFn: ({ signal }) => fetchWorkspaceIssue(repositoryId, restoredNumber!, signal),
    enabled: !frozen && restoredNumber !== null,
    gcTime: 0,
    retry: false,
  });
  const assigneesQuery = useQuery({
    queryKey: assignableUsersKey(viewerKey, repositoryId),
    queryFn: ({ signal }) => fetchAssignableUsers(repositoryId, signal),
    enabled: !demo && !frozen && Boolean(overview) && (editorOpen || tab === "board"),
    staleTime: 5 * 60_000,
    refetchOnWindowFocus: false,
    retry: false,
  });
  const savedCreateDraft = useMemo(() => {
    void draftRevision;
    return !demo && hasSavedCreateDraft(viewerKey, repositoryId);
  }, [demo, viewerKey, repositoryId, draftRevision]);

  function openNewIssue() {
    setSelectedEditIssue(null);
    onShowEditor("new");
  }

  function openEditIssue(issue: EditableIssue) {
    setSelectedEditIssue(issue);
    onShowEditor(issue.number);
  }

  function closeEditor() {
    setSelectedEditIssue(null);
    refreshSavedDraft();
    onHideEditor();
  }

  function refreshSavedDraft() {
    setDraftRevision(v => v + 1);
  }

  function switchToEditIssue(issueNumber: number, confirmed?: WriteOperationIssue) {
    openEditIssue(toEditableIssue(issueNumber, confirmed));
  }

  const assigneeSuggestions = demo ? overview?.contributors.filter(c => !c.isBot).map(c => c.login) ?? [] : assigneesQuery.data ?? [];
  const editingIssue: EditableIssue | undefined = typeof editor === "number"
    ? (selectedEditIssue?.number === editor ? selectedEditIssue : restoredIssueQuery.data ?? { number: editor })
    : undefined;

  return {
    editorOpen, editingIssue, savedCreateDraft, capabilitiesQuery, assigneeSuggestions,
    openNewIssue, openEditIssue, switchToEditIssue, closeEditor, refreshSavedDraft,
  };
}

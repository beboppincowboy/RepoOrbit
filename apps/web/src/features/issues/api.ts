import { RepositoryAccessUnverifiedError } from "@/shared/api/errors";
import { coverageFields, graphqlQuery, guardedGraphql, restFetch, type Coverage, type ReviewState } from "@/shared/api/transport";
import { IssueWriteError, toIssueWriteError, type BoardSnapshot } from "./write-errors";

export type { BoardSnapshot };

export type BoardStatus = "backlog" | "todo" | "in_progress" | "in_review" | "done";

export type WorkspaceIssue = {
  id: string;
  repositoryId: string;
  number: number;
  title: string;
  state: string;
  url: string;
  authorLogin: string | null;
  updatedAt: string;
  labels: string[];
  assignees: string[];
};

export type IssueCapabilities = {
  canCreateIssue: boolean;
  canCreateIssueWithMetadata: boolean;
  canManageIssues: boolean;
};

export type IssueCreateDraft = {
  title: string;
  body?: string;
  labels?: string[];
  assignees?: string[];
};

export type IssueDelta = {
  add?: string[];
  remove?: string[];
};

export type IssueUpdateDraft = {
  labels?: IssueDelta;
  assignees?: IssueDelta;
  state?: "open" | "closed";
  // Board changes only: the state the change was staged over. When GitHub's Board status, assignee set or (while open)
  // status-label set differs, the update answers 409 changed_on_github and nothing is recorded or sent.
  expect?: { status: BoardStatus; assignees: string[]; statusLabels: string[] };
};

export type WriteOperationIssue = {
  number: number;
  url: string;
  title?: string;
  state: string;
  labels?: string[];
  assignees?: string[];
};

export type WriteOperationOutcome = {
  issue?: WriteOperationIssue;
  unapplied?: string[];
  created?: string[]; // status_labels only: the labels the operation created (the others already existed)
  unappliedCount?: number;
  error?: string;
  allStepsAnswered?: boolean;
  redacted?: boolean;
};

export type WriteOperation = {
  id: number;
  repositoryId: number;
  // "cleanup" is the GitHub App removing a status label from a closed issue (actor ""); its id is from a separate sequence.
  kind: "create" | "update" | "status_labels" | "cleanup";
  state: "pending" | "unknown" | "succeeded" | "failed";
  issueNumber: number | null;
  draft: IssueCreateDraft | IssueUpdateDraft | StatusLabelsDraft | CleanupDraft | null;
  outcome: WriteOperationOutcome | null;
  actor: string;
  searchUrl?: string;
  createdAt: string;
  updatedAt: string;
};

export type StatusLabelsDraft = { labels: string[] };
export type CleanupDraft = { label: string };

// A repository's status source: whether status labels are on, whether the viewer (Admin or Maintain) may change that,
// and, only for such a viewer while they are off, which of the three labels already exist on GitHub.
export type StatusLabelState = { enabled: boolean; canChange: boolean; labels?: { name: string; exists: boolean }[] };

export type DeploymentSummary = {
  id: string;
  environment: string;
  sha: string;
  ref: string;
  state: string;
  description: string;
  htmlUrl: string;
  createdAt: string;
  updatedAt: string;
};
export type DeliveryCheck = {
  runId: string;
  workflowName: string;
  status: string;
  conclusion: string;
  htmlUrl: string;
  updatedAt: string;
  failingJobName: string | null;
  failingJobUrl: string | null;
};
export type LinkedPullRequestDelivery = {
  number: number;
  title: string;
  state: string;
  url: string;
  reviewState: ReviewState | null;
  headSha: string;
  checks: DeliveryCheck[];
  deployments: DeploymentSummary[];
};
export type IssueDelivery = {
  issueNumber: number;
  actionsCoverage: Coverage;
  deploymentsCoverage: Coverage;
  deploymentUnavailable: boolean;
  pullRequests: LinkedPullRequestDelivery[];
};

export async function fetchIssueDelivery(repositoryId: string, issueNumber: number, signal: AbortSignal): Promise<IssueDelivery | null> {
  const data = await guardedGraphql<{ issueDelivery: IssueDelivery | null }>(`query IssueDelivery($id: ID!, $issueNumber: Int!) {
    issueDelivery(repositoryId: $id, issueNumber: $issueNumber) {
      issueNumber deploymentUnavailable
      actionsCoverage { ${coverageFields} }
      deploymentsCoverage { ${coverageFields} }
      pullRequests {
        number title state url reviewState headSha
        checks { runId workflowName status conclusion htmlUrl updatedAt failingJobName failingJobUrl }
        deployments { id environment sha ref state description htmlUrl createdAt updatedAt }
      }
    }
  }`, { id: repositoryId, issueNumber }, "Could not load delivery lifecycle", signal);
  return data.issueDelivery;
}

export async function fetchWorkspaceIssues(id: string, after: string | null, signal: AbortSignal) {
  const { data, errors } = await graphqlQuery<{
    issues: { nodes: WorkspaceIssue[]; withheld: number; retryAt: string | null; hasNextPage: boolean; endCursor: string | null };
  }>(`query WorkspaceIssues($id: ID!, $after: ID) {
    issues(repositoryIds: [$id], first: 20, after: $after) {
      nodes { id repositoryId number title state url authorLogin updatedAt labels assignees }
      withheld retryAt hasNextPage endCursor
    }
  }`, { id, after }, signal);
  if (!data || errors?.length) throw new Error("Could not load issues");
  if (data.issues.withheld) throw new RepositoryAccessUnverifiedError(data.issues.retryAt ?? null);
  return data.issues;
}

// Fetches one imported issue by number (null when it has not been imported), so a restored editor knows its author and state.
export async function fetchWorkspaceIssue(id: string, number: number, signal: AbortSignal): Promise<WorkspaceIssue | null> {
  const { data, errors } = await graphqlQuery<{ issues: { nodes: WorkspaceIssue[]; withheld: number; retryAt: string | null } }>(`query WorkspaceIssue($id: ID!, $number: Int!) {
    issues(repositoryIds: [$id], first: 1, number: $number) {
      nodes { id repositoryId number title state url authorLogin updatedAt labels assignees }
      withheld retryAt
    }
  }`, { id, number }, signal);
  if (!data || errors?.length) throw new Error("Could not load the issue");
  if (data.issues.withheld) throw new RepositoryAccessUnverifiedError(data.issues.retryAt ?? null);
  return data.issues.nodes[0] ?? null;
}

export async function fetchIssueCapabilities(id: string, signal?: AbortSignal): Promise<IssueCapabilities> {
  const response = await restFetch(`/api/repositories/${encodeURIComponent(id)}/issue-capabilities`, { signal });
  if (!response.ok) throw await toIssueWriteError(response);
  return response.json();
}

// Lists the users GitHub would accept as assignees (empty when the viewer cannot assign), for editor suggestions.
export async function fetchAssignableUsers(id: string, signal?: AbortSignal): Promise<string[]> {
  const response = await restFetch(`/api/repositories/${encodeURIComponent(id)}/assignees`, { signal });
  if (!response.ok) throw await toIssueWriteError(response);
  return ((await response.json()) as { assignees: string[] }).assignees;
}

export async function fetchWriteOperations(id: string, signal?: AbortSignal): Promise<WriteOperation[]> {
  const response = await restFetch(`/api/repositories/${encodeURIComponent(id)}/write-operations`, { signal });
  if (!response.ok) throw await toIssueWriteError(response);
  const body: { operations: WriteOperation[] } = await response.json();
  return body.operations ?? [];
}

export async function fetchWriteOperation(operationId: number, signal?: AbortSignal): Promise<WriteOperation> {
  const response = await restFetch(`/api/write-operations/${encodeURIComponent(String(operationId))}`, { signal });
  if (!response.ok) throw await toIssueWriteError(response);
  return response.json();
}

export async function submitIssueCreate(repositoryId: string, idempotencyKey: string, draft: IssueCreateDraft): Promise<WriteOperation> {
  return submitIssueWrite(`/api/repositories/${encodeURIComponent(repositoryId)}/issues`, idempotencyKey, draft);
}

export async function submitIssueUpdate(repositoryId: string, issueNumber: number, idempotencyKey: string, draft: IssueUpdateDraft, signal?: AbortSignal): Promise<WriteOperation> {
  return submitIssueWrite(`/api/repositories/${encodeURIComponent(repositoryId)}/issues/${encodeURIComponent(String(issueNumber))}/updates`, idempotencyKey, draft, signal);
}

export async function fetchStatusLabels(repositoryId: string, signal?: AbortSignal): Promise<StatusLabelState> {
  const response = await restFetch(`/api/repositories/${encodeURIComponent(repositoryId)}/status-labels`, { signal });
  if (!response.ok) throw await toIssueWriteError(response);
  return response.json();
}

// Turns status labels on: a reviewed write that creates the missing labels and reuses existing ones.
export async function enableStatusLabels(repositoryId: string, idempotencyKey: string): Promise<WriteOperation> {
  return submitIssueWrite(`/api/repositories/${encodeURIComponent(repositoryId)}/status-labels`, idempotencyKey, {});
}

// Turns status labels off. Nothing is written to GitHub; the labels stay.
export async function disableStatusLabels(repositoryId: string): Promise<void> {
  const response = await restFetch(`/api/repositories/${encodeURIComponent(repositoryId)}/status-labels`, { method: "DELETE" }).catch(() => {
    throw new IssueWriteError("Could not reach the server. Check the setting before trying again.", { status: 0, kind: "transport" });
  });
  if (!response.ok) throw await toIssueWriteError(response);
}

async function submitIssueWrite(path: string, idempotencyKey: string, draft: IssueCreateDraft | IssueUpdateDraft | Record<string, never>, signal?: AbortSignal): Promise<WriteOperation> {
  const response = await restFetch(path, { method: "POST", headers: { "Idempotency-Key": idempotencyKey }, json: draft, signal }).catch(() => {
    throw new IssueWriteError("No reliable answer from the server. Your draft is preserved; check Write history before retrying.", { status: 0, kind: "transport" });
  });
  if (!response.ok) throw await toIssueWriteError(response);
  return response.json();
}

export async function recheckWriteOperation(operationId: number, issueNumber?: number, signal?: AbortSignal): Promise<WriteOperation> {
  const payload = issueNumber !== undefined ? { issueNumber } : {};
  const response = await restFetch(`/api/write-operations/${encodeURIComponent(String(operationId))}/recheck`, { method: "POST", json: payload, signal }).catch(() => {
    throw new IssueWriteError("Could not reach the server to recheck this operation. Try again shortly.", { status: 0, kind: "transport" });
  });
  if (!response.ok) throw await toIssueWriteError(response);
  return response.json();
}

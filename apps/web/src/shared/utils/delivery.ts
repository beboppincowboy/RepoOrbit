import type { Coverage, ReviewState } from "@/shared/api/transport";

export const isGitHubUrl = (url: string) => url.startsWith("https://github.com/");
export const githubHref = (url: string, fallback: string) => isGitHubUrl(url) ? url : fallback;

export const utcTimestampFormatter = new Intl.DateTimeFormat("en-US", {
  month: "short",
  day: "numeric",
  hour: "2-digit",
  minute: "2-digit",
  hourCycle: "h23",
  timeZone: "UTC",
});

export function formatDurationSeconds(seconds: number | null) {
  if (seconds === null) return "—";
  if (seconds < 60) return `${seconds}s`;
  const mins = Math.floor(seconds / 60);
  const rem = seconds % 60;
  return rem ? `${mins}m ${rem}s` : `${mins}m`;
}

const FAILED_CONCLUSIONS = ["failure", "timed_out", "startup_failure", "error"];
export const isFailedConclusion = (conclusion: string) => FAILED_CONCLUSIONS.includes(conclusion);

export function deploymentUnavailableNote(coverage: Coverage) {
  if (coverage.reason === "permission_denied") return "Deployments read permission is not granted for this installation.";
  if (!coverage.covered) return "Awaiting GitHub Deployments synchronization.";
  return "No GitHub Deployment records exist for this repository. Passing workflows are not treated as Deployments.";
}

export function workflowStatusTone(status: string, conclusion: string) {
  if (status !== "completed") return "border-border bg-muted text-muted-foreground";
  if (conclusion === "success") return "border-success/30 bg-success/10 text-success";
  if (isFailedConclusion(conclusion)) return "border-destructive/30 bg-destructive/10 text-destructive";
  return "border-border bg-muted text-muted-foreground";
}

export function workflowStatusLabel(status: string, conclusion: string) {
  return status === "completed" ? conclusion || "completed" : status.replace(/_/g, " ");
}

export const reviewLabels: Record<ReviewState, [string, string]> = {
  awaiting_review: ["Awaiting review", "border-border text-soft"],
  commented: ["Commented", "border-border text-soft"],
  approved: ["Approved", "border-success-soft bg-success-soft text-success-foreground"],
  changes_requested: ["Changes requested", "border-warning-soft bg-warning-soft text-warning"],
  merged: ["Merged", "border-primary-soft bg-primary-soft text-primary"],
  closed: ["Closed", "border-border bg-muted text-muted-foreground"],
};

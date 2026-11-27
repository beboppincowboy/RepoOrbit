import { muted } from "@/features/analytics/components/workspace-parts";
import type { RepositoryOverview } from "@/features/analytics/api";

const coverageReasons: Record<string, [string, string]> = {
  not_synchronized: ["Awaiting data", muted],
  window_before_boundary: ["Window predates imported history", "text-warning"],
  historical_gap: ["Partial: issue event history incomplete", "text-warning"],
  children_incomplete: ["Partial: reviews and comments importing", "text-warning"],
};
export const coverageReason = (coverage: RepositoryOverview["openIssues"]["coverage"]) => coverageReasons[coverage.reason ?? ""] ?? ["Coverage incomplete", "text-warning"];

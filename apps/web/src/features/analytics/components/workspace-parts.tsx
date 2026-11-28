import type { ReactNode } from "react";
import { buttonVariants } from "@/shared/ui/button";
import { IssueIcon, PullRequestIcon } from "@/shared/ui/icons";

export const card = "rounded-xl border border-border bg-card min-w-0 min-h-0";
export const muted = "text-muted-foreground";
export const lrow = "flex items-center gap-3 border-b border-border px-3 py-2.5 last:border-b-0";
export const mini = "inline-flex items-center gap-1 whitespace-nowrap rounded-full border px-[7px] text-[11px]";
export const smallBtn = buttonVariants({ variant: "outline", size: "sm" });

const svg = (size: number, d: ReactNode) => <svg width={size} height={size} viewBox="0 0 16 16" aria-hidden="true" className="shrink-0">{d}</svg>;
export const icons = {
  repo: svg(16, <path d="M3 2.5h8.5v10H4.5A1.5 1.5 0 0 1 3 11zM3 11a1.5 1.5 0 0 1 1.5-1.5h7" fill="none" stroke="currentColor" strokeWidth="1.4" />),
  pr: <PullRequestIcon />,
  issue: <IssueIcon />,
  tag: svg(14, <><path d="M2.5 2.5h5.3l5.7 5.7-5.3 5.3-5.7-5.7z" fill="none" stroke="currentColor" strokeWidth="1.4" strokeLinejoin="round" /><circle cx="5.5" cy="5.5" r="1" fill="currentColor" /></>),
  ext: svg(12, <path d="M9 3h4v4M13 3L7 9M11 9.5V13H3V5h3.5" fill="none" stroke="currentColor" strokeWidth="1.5" />),
  info: svg(14, <><circle cx="8" cy="8" r="6" fill="none" stroke="currentColor" strokeWidth="1.4" /><path d="M8 7v4M8 5v.5" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" /></>),
};
export const Ico = ({ children, tone = "bg-muted text-muted-foreground" }: { children: ReactNode; tone?: string }) =>
  <span className={`grid size-7 shrink-0 place-items-center rounded-lg ${tone}`}>{children}</span>;

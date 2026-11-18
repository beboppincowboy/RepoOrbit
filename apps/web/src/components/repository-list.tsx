import { Link } from "@tanstack/react-router";
import type { ReactNode } from "react";

// Shared pieces of the Repositories screen. `*Class` exports are Tailwind class strings for the prototype styles (prototype D2 `repoTools` / `repoTable`): the My repositories / Starred tabs
// live at /repositories and /starred so both URLs stay linkable.
export const tableCardClass = "relative min-h-0 flex-1 overflow-y-auto rounded-xl border border-border bg-card [scrollbar-width:thin]";
export const thClass = "sticky top-0 z-[1] whitespace-nowrap border-b border-border bg-card px-3 py-2 text-left text-xs font-medium text-muted-foreground";
export const tdClass = "border-b border-border px-3 py-[9px] align-middle";
export const segGroupClass = "inline-flex gap-0.5 rounded-full border border-border bg-card p-0.5";
export const segItemClass = "rounded-full px-[11px] py-[3px] text-xs text-muted-foreground aria-pressed:bg-primary-soft aria-pressed:font-semibold aria-pressed:text-primary aria-[current=page]:bg-primary-soft aria-[current=page]:font-semibold aria-[current=page]:text-primary";
export const searchBoxClass = "w-[240px] rounded-lg border border-border bg-background px-2.5 py-[5px] text-foreground outline-none placeholder:text-muted-foreground";
export const noteClass = "shrink-0 rounded-[10px] border border-border bg-card px-3 py-2.5 text-[13px]";
export const warnNoteClass = "shrink-0 rounded-[10px] border border-warning bg-warning-soft px-3 py-2.5 text-[13px] text-foreground";
const chipClass = "inline-block rounded-full border border-border bg-muted px-2 text-[11.5px] text-soft";

// The "Repositories" title; children is the one-line subtitle under it.
export function RepositoriesHeading({ children }: { children: ReactNode }) {
  return <div className="shrink-0"><h1 className="text-xl">Repositories</h1><p className="m-0 text-muted-foreground">{children}</p></div>;
}

// My repositories / Starred segmented tabs: links to /repositories and /starred, current one marked aria-current.
export function SourceTabs() {
  return <nav aria-label="Source" className={segGroupClass}>
    <Link to="/repositories" className={segItemClass}>My repositories</Link>
    <Link to="/starred" className={segItemClass}>Starred</Link>
  </nav>;
}

// The wrapping toolbar row above the table (tabs, filter, view switch, Refresh).
export function ToolsRow({ children }: { children: ReactNode }) {
  return <div className="flex shrink-0 flex-wrap items-center gap-2">{children}</div>;
}

// Table cell content: full name, a "Private" chip when isPrivate, then the description. as="h2" makes the name a heading.
export function RepositoryCell({ fullName, isPrivate, description, as: Tag = "div" }: { fullName: string; isPrivate: boolean; description: string; as?: "div" | "h2" }) {
  return <>
    <Tag className="text-[13.5px] font-medium tracking-normal">{fullName}{isPrivate && <> <span className={chipClass}>Private</span></>}</Tag>
    {description && <div className="text-xs text-muted-foreground">{description}</div>}
  </>;
}

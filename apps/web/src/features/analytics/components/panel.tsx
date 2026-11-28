import type { ReactNode } from "react";
import { ChevronIcon } from "@/shared/ui/icons";

export const panelCard = "min-h-0 min-w-0 rounded-xl border border-border bg-card";

export function Panel({ id, title, count, extra, fold, onToggle, children }: { id: string; title: string; count?: number; extra?: ReactNode; fold: Record<string, boolean>; onToggle: (id: string) => void; children: ReactNode }) {
  const folded = !!fold[id];
  return <section aria-label={title} className={`${panelCard} flex flex-col ${folded ? "flex-none" : "flex-[1_1_0]"} max-[900px]:flex-none`}>
    <header className="flex shrink-0 items-center gap-2 py-2 pr-3.5 pl-2">
      <button type="button" aria-expanded={!folded} onClick={() => onToggle(id)} className="group flex min-w-0 flex-1 items-center gap-2 rounded-md px-1.5 py-1 text-left hover:bg-muted">
        <ChevronIcon className="rotate-90 transition-transform group-aria-[expanded=false]:rotate-0 motion-reduce:transition-none" />
        <h2 className="text-sm">{title}</h2>
        {count != null && <span className="inline-flex items-center rounded-full border border-border px-[7px] text-[11px] text-soft">{count}</span>}
      </button>
      {!folded && extra}
    </header>
    {!folded && <div className="flex min-h-0 flex-1 flex-col overflow-y-auto [scrollbar-width:thin] max-[900px]:max-h-80">{children}</div>}
  </section>;
}

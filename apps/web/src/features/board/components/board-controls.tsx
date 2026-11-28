import type { BoardIssue } from "@/features/board/api";
import { field, muted, panel } from "@/features/board/components/board-styles";
import { filterOptions } from "@/features/board/model";
import { saveButtonState } from "@/features/board/review";
import { Button } from "@/shared/ui/button";

type FilterSelectProps = {
  label: string;
  value: string;
  onChange: (value: string) => void;
  values: string[];
  // The label of the option that clears the filter.
  all: string;
  drawerOpen: boolean;
};

function FilterSelect({ label, value, onChange, values, all, drawerOpen }: FilterSelectProps) {
  return (
    <select aria-label={label} value={value} onChange={e => onChange(e.target.value)} className={drawerOpen ? `${field} min-[900px]:max-w-full` : field}>
      <option value="">{all}</option>
      {values.map(v => <option key={v} value={v}>{v}</option>)}
    </select>
  );
}

type BoardControlsProps = {
  // Every row of the Board read, which supplies the filter options.
  rows: BoardIssue[];
  // Whether the Board has status columns, which offers the view switch and Save.
  statusLabels: boolean;
  view: "board" | "list";
  setView: (view: "board" | "list") => void;
  text: string;
  setText: (text: string) => void;
  assignee: string;
  setAssignee: (assignee: string) => void;
  label: string;
  setLabel: (label: string) => void;
  drawerOpen: boolean;
  // How many cards a save would send.
  staged: number;
  blocked: string | null;
  saving: boolean;
  // Opens the save review.
  onSave: () => void;
};

export function BoardControls({ rows, statusLabels, view, setView, text, setText, assignee, setAssignee, label, setLabel, drawerOpen, staged, blocked, saving, onSave }: BoardControlsProps) {
  return (
    <section aria-label="Board controls" className={`${panel} flex flex-none flex-wrap items-center gap-2 px-4 py-3`}>
      {statusLabels && (
        <div role="group" aria-label="View" className="inline-flex gap-0.5 rounded-full border border-border bg-card p-0.5">
          {(["board", "list"] as const).map(v => <button key={v} type="button" aria-pressed={view === v} onClick={() => setView(v)}
            className="rounded-full px-[11px] py-[3px] text-xs text-muted-foreground aria-pressed:bg-primary-soft aria-pressed:font-semibold aria-pressed:text-primary">{v === "board" ? "Board" : "List"}</button>)}
        </div>
      )}
      <input type="search" aria-label="Filter by text" placeholder="Filter by title or #" value={text} onChange={e => setText(e.target.value)} className={`${field} w-48`} />
      <FilterSelect label="Filter by assignee" value={assignee} onChange={setAssignee} values={filterOptions(rows.flatMap(row => row.assignees))} all="All assignees" drawerOpen={drawerOpen} />
      <FilterSelect label="Filter by label" value={label} onChange={setLabel} values={filterOptions(rows.flatMap(row => row.labels))} all="All labels" drawerOpen={drawerOpen} />
      {statusLabels && (
        <div className="ml-auto flex items-center gap-2 text-xs">
          <span className={muted}>{staged === 1 ? "1 staged change" : `${staged} staged changes`} · nothing is sent to GitHub until you save</span>
          <Button size="sm" {...saveButtonState({ staged, blocked, saving })} aria-busy={saving} onClick={onSave}>{saving ? "Saving…" : "Save…"}</Button>
        </div>
      )}
    </section>
  );
}

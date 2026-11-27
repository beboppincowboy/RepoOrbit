import { useMemo, useState, type FormEvent } from "react";
import { useInfiniteQuery } from "@tanstack/react-query";
import { createColumnHelper, tableFeatures, useTable } from "@tanstack/react-table";
import { fetchWorkspaceIssues, type WorkspaceIssue, type WriteOperation } from "@/features/issues/api";
import { toEditableIssue, type EditableIssue } from "@/features/issues/model";
import { workspaceIssuesKey } from "@/features/issues/query-keys";
import { StatusSourceSetup } from "@/features/issues/status-source-setup";
import { WriteHistory } from "@/features/issues/write-history";
import { Button } from "@/shared/ui/button";
import { githubHref } from "@/shared/utils/delivery";

const card = "rounded-xl border border-border bg-card min-w-0 min-h-0";
const muted = "text-muted-foreground";
const mini = "inline-flex items-center gap-1 whitespace-nowrap rounded-full border px-[7px] text-[11px]";

const features = tableFeatures({});

const issueColumn = createColumnHelper<typeof features, WorkspaceIssue>();
const noIssues: WorkspaceIssue[] = [];

export function IssuesTab({
  id,
  viewerKey,
  demo,
  frozen,
  github,
  onNewIssue,
  onEditIssue,
  onResolved,
}: {
  id: string;
  viewerKey: string;
  demo: boolean;
  frozen: boolean;
  github: string;
  onNewIssue: () => void;
  onEditIssue: (issue: EditableIssue) => void;
  onResolved: (op: WriteOperation) => void;
}) {
  const [jumpNumber, setJumpNumber] = useState("");
  const query = useInfiniteQuery({
    queryKey: workspaceIssuesKey(id, viewerKey),
    queryFn: ({ pageParam, signal }) => fetchWorkspaceIssues(id, pageParam, signal),
    initialPageParam: null as string | null,
    getNextPageParam: last => last.hasNextPage ? last.endCursor : undefined,
    enabled: !frozen,
    gcTime: 0,
  });

  const issues = useMemo(() => query.data?.pages.flatMap(page => page.nodes) ?? noIssues, [query.data]);

  const columns = useMemo(() => issueColumn.columns([
    issueColumn.accessor("number", {
      header: "Issue",
      cell: c => (
        <a href={githubHref(c.row.original.url, `${github}/issues/${c.getValue()}`)} className="font-mono text-xs text-muted-foreground hover:underline">
          #{c.getValue()}
        </a>
      ),
    }),
    issueColumn.accessor("title", {
      header: "Title",
      cell: c => (
        <>
          <span className="font-medium">{c.getValue()}</span>
          {c.row.original.authorLogin && <span className={`ml-2 text-xs ${muted}`}>@{c.row.original.authorLogin}</span>}
        </>
      ),
    }),
    issueColumn.accessor("state", {
      header: "State",
      cell: c => (
        <span className={`${mini} ${c.getValue() === "open" ? "border-success-soft bg-success-soft text-success-foreground" : "border-border bg-muted text-muted-foreground"}`}>
          {c.getValue()}
        </span>
      ),
    }),
    issueColumn.accessor("labels", {
      header: "Labels",
      cell: c => (
        <div className="flex flex-wrap gap-1">
          {c.getValue().length
            ? c.getValue().map(name => <span key={name} className={`${mini} border-border bg-muted text-soft`}>{name}</span>)
            : <span className={`text-xs ${muted}`}>—</span>}
        </div>
      ),
    }),
    issueColumn.accessor("assignees", {
      header: "Assignees",
      cell: c => (
        <span className="text-xs text-soft">
          {c.getValue().length ? c.getValue().map(a => `@${a}`).join(", ") : <span className={muted}>Unassigned</span>}
        </span>
      ),
    }),
    issueColumn.display({
      id: "actions",
      header: () => <span className="sr-only">Actions</span>,
      cell: c => (
        <Button
          variant="outline"
          size="sm"
          disabled={frozen}
          onClick={() => onEditIssue({
            number: c.row.original.number,
            title: c.row.original.title,
            state: c.row.original.state,
            url: c.row.original.url,
            authorLogin: c.row.original.authorLogin,
            labels: c.row.original.labels,
            assignees: c.row.original.assignees,
          })}
        >
          Edit<span className="sr-only"> issue #{c.row.original.number}</span>
        </Button>
      ),
    }),
  ]), [github, frozen, onEditIssue]);

  const table = useTable({ features, columns, data: issues });
  const th = "sticky top-0 z-[1] whitespace-nowrap border-b border-border bg-card px-3 py-2 text-left text-xs font-medium text-muted-foreground";

  function openByNumber(event: FormEvent) {
    event.preventDefault();
    const num = Number.parseInt(jumpNumber.trim(), 10);
    if (!Number.isInteger(num) || num <= 0) return;
    const matched = issues.find(item => item.number === num);
    setJumpNumber("");
    onEditIssue(matched
      ? { number: matched.number, title: matched.title, state: matched.state, url: matched.url, authorLogin: matched.authorLogin, labels: matched.labels, assignees: matched.assignees }
      : { number: num });
  }

  return (
    <div className="flex min-h-0 flex-1 gap-3 max-[900px]:flex-col">
      <section aria-label="Repository issues" className={`${card} flex min-h-0 flex-[1.35_1_480px] flex-col`}>
        <div className="flex flex-none flex-wrap items-center justify-between gap-2 border-b border-border px-4 py-3">
          <div>
            <h2 className="m-0 text-sm">Issues</h2>
            <p className={`m-0 text-xs ${muted}`}>GitHub issues are tasks. Changes go through Review → Submit → Confirmed.</p>
          </div>
          <div className="flex flex-wrap items-center gap-2">
            <form onSubmit={openByNumber} className="flex items-center gap-1.5">
              <input
                type="number"
                min={1}
                step={1}
                aria-label="Edit issue by number"
                placeholder="Issue #"
                value={jumpNumber}
                disabled={frozen}
                onChange={e => setJumpNumber(e.target.value)}
                className="w-24 rounded-lg border border-border bg-card px-2 py-1 text-xs"
              />
              <Button type="submit" variant="outline" size="sm" disabled={frozen || !jumpNumber.trim()}>Edit #</Button>
            </form>
            <Button size="sm" onClick={onNewIssue} disabled={frozen}>+ New issue</Button>
          </div>
        </div>
        <div className="min-h-0 flex-1 overflow-y-auto">
          {frozen ? (
            <p className="p-4">The issue list needs a live connection. Saved issue counts are available on Overview.</p>
          ) : query.isPending ? (
            <p role="status" className="p-4">Loading issues…</p>
          ) : query.isError ? (
            <p role="alert" className="p-4">{query.error.message} <Button variant="outline" size="sm" onClick={() => void query.refetch()}>Retry</Button></p>
          ) : (
            <>
              <table className="w-full border-collapse">
                <thead>{table.getHeaderGroups().map(group => <tr key={group.id}>{group.headers.map(h => <th key={h.id} className={th}><table.FlexRender header={h} /></th>)}</tr>)}</thead>
                <tbody>
                  {table.getRowModel().rows.map(row => (
                    <tr key={row.id} className="hover:bg-muted">
                      {row.getAllCells().map(cell => (
                        <td key={cell.id} className="border-b border-border px-3 py-[9px] align-middle"><table.FlexRender cell={cell} /></td>
                      ))}
                    </tr>
                  ))}
                  {!issues.length && <tr><td colSpan={columns.length} className={`p-6 text-center ${muted}`}>No imported issues yet.</td></tr>}
                </tbody>
              </table>
              {query.hasNextPage && (
                <div className="p-3">
                  <Button variant="outline" onClick={() => void query.fetchNextPage()} disabled={query.isFetching}>Load more issues</Button>
                </div>
              )}
            </>
          )}
        </div>
      </section>

      <div className="flex min-h-0 flex-[1_1_360px] flex-col gap-3">
        <StatusSourceSetup repositoryId={id} viewerKey={viewerKey} demo={demo} frozen={frozen} />
        <WriteHistory
          repositoryId={id}
          viewerKey={viewerKey}
          demo={demo}
          frozen={frozen}
          onResolved={onResolved}
          onEditIssue={(issueNumber, confirmed) => onEditIssue(toEditableIssue(issueNumber, confirmed))}
        />
      </div>
    </div>
  );
}

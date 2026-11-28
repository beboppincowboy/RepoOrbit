import { useMemo } from "react";
import { useInfiniteQuery } from "@tanstack/react-query";
import { createColumnHelper, tableFeatures, useTable } from "@tanstack/react-table";
import { fetchWorkspacePullRequests, type WorkspacePullRequest } from "@/features/analytics/api";
import { card, mini, muted } from "@/features/analytics/components/workspace-parts";
import { day } from "@/features/analytics/format";
import { workspacePullRequestsKey } from "@/features/repositories/query-keys";
import { Button } from "@/shared/ui/button";
import { githubHref, reviewLabels } from "@/shared/utils/delivery";

const features = tableFeatures({});
const column = createColumnHelper<typeof features, WorkspacePullRequest>();
const noPulls: WorkspacePullRequest[] = [];

export function PullRequests({ id, viewer, github }: { id: string; viewer: string; github: string }) {
  const query = useInfiniteQuery({
    queryKey: workspacePullRequestsKey(id, viewer),
    queryFn: ({ pageParam, signal }) => fetchWorkspacePullRequests(id, pageParam, signal),
    initialPageParam: null as string | null,
    getNextPageParam: last => last.hasNextPage ? last.endCursor : undefined,
    gcTime: 0,
  });
  const columns = useMemo(() => column.columns([
    column.accessor("number", { header: "PR", cell: c => <a href={githubHref(c.row.original.url, `${github}/pull/${c.getValue()}`)} className="font-mono text-xs text-muted-foreground hover:underline">#{c.getValue()}</a> }),
    column.accessor("title", { header: "Title", cell: c => <><span className="font-medium">{c.getValue()}</span>{c.row.original.author && <span className={`ml-2 text-xs ${muted}`}>{c.row.original.author.login}</span>}</> }),
    column.accessor("reviewState", { header: "Review", cell: c => {
      const state = c.getValue();
      return state === null ? <span className={`${mini} border-dashed border-border-strong text-muted-foreground`}>Review history importing</span>
        : <span className={`${mini} ${reviewLabels[state][1]}`}>{reviewLabels[state][0]}</span>;
    } }),
    column.accessor("createdAt", { header: "Opened", cell: c => <span className={`font-mono text-xs ${muted}`}>{day.format(new Date(c.getValue()))}</span> }),
  ]), [github]);
  const pulls = useMemo(() => query.data?.pages.flatMap(page => page.nodes) ?? noPulls, [query.data]);
  const table = useTable({ features, columns, data: pulls });
  const th = "sticky top-0 z-[1] whitespace-nowrap border-b border-border bg-card px-3 py-2 text-left text-xs font-medium text-muted-foreground";
  return <div className={`${card} flex-1 overflow-auto`}>
    {query.isPending ? <p role="status" className="p-4">Loading pull requests…</p> : query.isError ? <p role="alert" className="p-4">{query.error.message} <Button variant="outline" onClick={() => void query.refetch()}>Retry</Button></p> : <>
      <table className="w-full border-collapse">
        <thead>{table.getHeaderGroups().map(group => <tr key={group.id}>{group.headers.map(h => <th key={h.id} className={th}><table.FlexRender header={h} /></th>)}</tr>)}</thead>
        <tbody>
          {table.getRowModel().rows.map(row => <tr key={row.id} className="hover:bg-muted">{row.getAllCells().map(cell => <td key={cell.id} className="border-b border-border px-3 py-[9px] align-middle"><table.FlexRender cell={cell} /></td>)}</tr>)}
          {!pulls.length && <tr><td colSpan={columns.length} className={`p-6 text-center ${muted}`}>No imported pull requests.</td></tr>}
        </tbody>
      </table>
      {query.hasNextPage && <div className="p-3"><Button variant="outline" onClick={() => void query.fetchNextPage()} disabled={query.isFetching}>Load more pull requests</Button></div>}
    </>}
  </div>;
}

import type { ReactNode } from "react";
import { muted, panel } from "@/features/board/components/board-styles";
import { StatusSourceSetup } from "@/features/issues/status-source-setup";

type NoStatusSourceProps = {
  // The list of the repository's issues, shown beside the setup.
  list: ReactNode;
  repositoryId: string;
  viewerKey: string;
  demo: boolean;
};

// The Board of a repository without status labels: its issue list and the status-source setup.
export function NoStatusSource({ list, repositoryId, viewerKey, demo }: NoStatusSourceProps) {
  return (
    <div className="flex min-h-0 flex-1 gap-3 max-[900px]:flex-col">
      {list}
      <div className="flex min-h-0 flex-[1_1_320px] flex-col gap-3">
        <section className={`${panel} px-4 py-3`}>
          <h2 className="m-0 text-sm">No status source</h2>
          <p className={`m-0 text-xs ${muted}`}>This repository has no status labels, so its issues are listed without columns and cannot be moved.</p>
        </section>
        <StatusSourceSetup repositoryId={repositoryId} viewerKey={viewerKey} demo={demo} frozen={false} />
      </div>
    </div>
  );
}

import { muted, panel } from "@/features/board/components/board-styles";
import { CLOSED_CAP, OPEN_CAP } from "@/features/board/model";

type BoardNoticesProps = {
  // What the last save left to tell; null when nothing.
  saveNotice: string | null;
  demo: boolean;
  truncated: boolean | undefined;
  openCapped: boolean | undefined;
  closedCapped: boolean | undefined;
};

// The lines between the Board controls and its content.
export function BoardNotices({ saveNotice, demo, truncated, openCapped, closedCapped }: BoardNoticesProps) {
  return (
    <>
      {saveNotice && <p role="status" className={`${panel} m-0 flex-none px-4 py-2 text-[13px]`}>{saveNotice}</p>}
      {demo && (
        <div role="status" className="flex-none rounded-[10px] border border-demo bg-demo-soft px-3 py-2 text-[13px] text-demo">
          <strong>Demo mode</strong> — remote GitHub writes are disabled, so Board changes cannot be staged.
        </div>
      )}
      {truncated && (
        <p role="note" className={`m-0 flex-none text-xs ${muted}`}>
          The Board shows only {[openCapped && `the ${OPEN_CAP} most recently updated open issues`, closedCapped && `the ${CLOSED_CAP} most recently closed issues`].filter(Boolean).join(" and ") || "part of this repository's issues"}.
        </p>
      )}
    </>
  );
}

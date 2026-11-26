import { useLayoutEffect, useRef } from "react";
import { Button } from "@/components/ui/button";
import type { BoardReviewItem } from "@/lib/board-review";

type BoardSaveReviewProps = {
  items: BoardReviewItem[];
  // Why saving is disabled; null when the Board is not blocked.
  blocked: string | null;
  saving: boolean;
  confirmDisabled: boolean;
  onConfirm: () => void;
  // Closes the review. It does not stop a save that is already running.
  onClose: () => void;
};

// The dialog that lists what a save will send. It takes focus when it opens, and Escape closes it.
export function BoardSaveReview({ items, blocked, saving, confirmDisabled, onConfirm, onClose }: BoardSaveReviewProps) {
  const ref = useRef<HTMLDialogElement>(null);
  useLayoutEffect(() => {
    const dialog = ref.current!;
    dialog.showModal();
    return () => dialog.close();
  }, []);

  return (
    <dialog
      ref={ref}
      aria-label="Review staged changes"
      onCancel={event => { event.preventDefault(); onClose(); }}
      onKeyDown={event => { if (event.key === "Escape") event.stopPropagation(); }}
      className="fixed top-1/2 left-1/2 max-h-[calc(100vh-24px)] w-[min(520px,calc(100vw-24px))] -translate-x-1/2 -translate-y-1/2 flex-col overflow-hidden rounded-xl border border-border bg-card text-foreground shadow-[0_18px_44px_rgba(15,23,42,.22)] outline-none open:flex backdrop:bg-scrim"
    >
      <h2 className="m-0 flex-none border-b border-border p-4 text-base">Review staged changes</h2>
      <ul className="m-0 flex min-h-0 flex-1 list-none flex-col gap-2 overflow-y-auto p-4 text-[13px]">
        {items.map(item => (
          <li key={item.number} data-review-card={item.number} className="flex flex-col gap-1 rounded-lg border border-border p-2.5">
            <span className="break-words font-medium"><span className="font-mono text-xs text-muted-foreground">#{item.number}</span> {item.title}</span>
            <span className="text-xs text-soft">{item.change}</span>
            {item.sideEffects.length > 0 && <ul className="m-0 list-disc pl-4 text-xs">{item.sideEffects.map(line => <li key={line}>{line}</li>)}</ul>}
          </li>
        ))}
      </ul>
      <div className="flex flex-none flex-col gap-2 border-t border-border p-4">
        {blocked && <p role="status" className="m-0 rounded-[10px] border border-warning bg-warning-soft px-3 py-2 text-[13px]">{blocked}</p>}
        <div className="flex justify-end gap-2">
          <Button variant="outline" size="sm" onClick={onClose}>{saving ? "Close" : "Cancel"}</Button>
          <Button size="sm" disabled={confirmDisabled} onClick={onConfirm}>{saving ? "Saving…" : "Confirm"}</Button>
        </div>
      </div>
    </dialog>
  );
}

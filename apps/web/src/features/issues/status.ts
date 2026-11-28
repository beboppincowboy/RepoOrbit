// A Board column. Backlog and Done carry no status label.
export type BoardStatus = "backlog" | "todo" | "in_progress" | "in_review" | "done";

// Every status in Board column order.
export const BOARD_STATUSES: readonly BoardStatus[] = ["backlog", "todo", "in_progress", "in_review", "done"];

// The GitHub label that marks each labelled status.
export const STATUS_LABELS: Partial<Record<BoardStatus, string>> = { todo: "status:todo", in_progress: "status:in-progress", in_review: "status:review" };

// Each Board column's name.
export const STATUS_NAMES: Record<BoardStatus, string> = { backlog: "Backlog", todo: "Todo", in_progress: "In Progress", in_review: "In Review", done: "Done" };

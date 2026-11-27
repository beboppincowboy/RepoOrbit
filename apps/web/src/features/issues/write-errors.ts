import type { BoardSnapshot, WriteOperation } from "./api";

export type IssueWriteErrorKind =
  | "validation"
  | "unauthenticated"
  | "forbidden"
  | "not_found"
  | "duplicate_create"
  | "recheck_unresolved"
  | "unprocessable"
  | "unverified"
  | "transport"
  | "server"
  | "changed_on_github"; // a Board update's expect no longer matches GitHub; current holds the live state

export type WriteDisposition = "not-recorded" | "possibly-recorded" | null;

export class IssueWriteError extends Error {
  readonly status: number;
  readonly kind: IssueWriteErrorKind;
  readonly disposition: WriteDisposition;
  readonly retryAfter: string | null;
  readonly operationId: number | null;
  readonly operation: WriteOperation | null;
  readonly current: BoardSnapshot | null;

  constructor(
    message: string,
    opts: {
      status: number;
      kind: IssueWriteErrorKind;
      disposition?: WriteDisposition;
      retryAfter?: string | null;
      operationId?: number | null;
      operation?: WriteOperation | null;
      current?: BoardSnapshot | null;
    },
  ) {
    super(message);
    this.status = opts.status;
    this.kind = opts.kind;
    this.disposition = opts.disposition ?? null;
    this.retryAfter = opts.retryAfter ?? null;
    this.operationId = opts.operationId ?? null;
    this.operation = opts.operation ?? null;
    this.current = opts.current ?? null;
  }
}

const SERVER_WRITE_RETENTION_DAYS = 30;
const CLOCK_SKEW_MARGIN_DAYS = 1;
export const WRITE_RECOVERY_WINDOW_MS = (SERVER_WRITE_RETENTION_DAYS - CLOCK_SKEW_MARGIN_DAYS) * 24 * 60 * 60 * 1000;
const WRITE_RECOVERY_FUTURE_SKEW_MS = 5 * 60 * 1000;

export function generateIdempotencyKey(): string {
  if (typeof crypto !== "undefined" && typeof crypto.randomUUID === "function") {
    return `ro-${crypto.randomUUID()}`;
  }
  return `ro-${Date.now()}-${Math.random().toString(36).slice(2, 12)}`;
}

export function canReplayWithSameKey(attemptedAt: string | null | undefined, now: number = Date.now()): boolean {
  if (!attemptedAt) return false;
  const parsed = Date.parse(attemptedAt);
  if (!Number.isFinite(parsed)) return false;
  const age = now - parsed;
  return age >= -WRITE_RECOVERY_FUTURE_SKEW_MS && age <= WRITE_RECOVERY_WINDOW_MS;
}

export type WriteFailureClassification =
  | { outcome: "duplicate_create"; operationId: number; message: string }
  | { outcome: "changed_on_github"; current: BoardSnapshot; message: string }
  | { outcome: "recheck_unresolved"; operation: WriteOperation | null; message: string }
  | { outcome: "possibly_recorded"; message: string }
  | { outcome: "not_recorded"; message: string };

export function mayHaveBeenRecorded(error: unknown, opts: { isReplay?: boolean } = {}): boolean {
  if (!(error instanceof IssueWriteError)) return true;
  if (error.kind === "duplicate_create" || error.kind === "recheck_unresolved") return false;
  if (opts.isReplay) return true;
  if (error.kind === "transport" || error.kind === "server") return true;
  if (error.disposition === "possibly-recorded") return true;
  if (error.disposition === "not-recorded") return false;
  if (error.status === 503) return true;
  return false;
}

export function classifyWriteFailure(error: unknown, opts: { isReplay?: boolean } = {}): WriteFailureClassification {
  const message = error instanceof Error ? error.message : "Could not submit to GitHub.";
  if (error instanceof IssueWriteError) {
    if (error.kind === "duplicate_create" && error.operationId) {
      return { outcome: "duplicate_create", operationId: error.operationId, message };
    }
    if (error.kind === "changed_on_github" && error.current) {
      return { outcome: "changed_on_github", current: error.current, message };
    }
    if (error.kind === "recheck_unresolved") {
      return { outcome: "recheck_unresolved", operation: error.operation, message };
    }
  }
  return mayHaveBeenRecorded(error, opts)
    ? { outcome: "possibly_recorded", message }
    : { outcome: "not_recorded", message };
}

function parseWriteDisposition(header: string | null): WriteDisposition {
  const value = header?.trim().toLowerCase();
  if (value === "not-recorded" || value === "possibly-recorded") return value;
  return null;
}

function parseRetryAfter(header: string | null): string | null {
  if (!header) return null;
  const seconds = Number(header);
  if (Number.isFinite(seconds) && seconds > 0) {
    return new Date(Date.now() + seconds * 1000).toISOString();
  }
  const parsed = Date.parse(header);
  return Number.isFinite(parsed) ? new Date(parsed).toISOString() : null;
}

export async function toIssueWriteError(response: Response): Promise<IssueWriteError> {
  const retryAfter = parseRetryAfter(response.headers.get("Retry-After"));
  const disposition = parseWriteDisposition(response.headers.get("RepoOrbit-Write-Disposition"));
  const text = (await response.text().catch(() => "")).trim();
  let parsed: Record<string, unknown> | null = null;
  if (text.startsWith("{")) {
    try {
      parsed = JSON.parse(text) as Record<string, unknown>;
    } catch { /* plain text error */ }
  }
  if (response.status === 409) {
    if (parsed && parsed.current && typeof parsed.current === "object") {
      return new IssueWriteError("This issue changed on GitHub after the change was staged.", { status: 409, kind: "changed_on_github", disposition, current: parsed.current as BoardSnapshot });
    }
    if (parsed && typeof parsed.operationId === "number") {
      const msg = typeof parsed.error === "string" && parsed.error ? parsed.error : "An earlier create of this issue is still unresolved; recheck it instead of resubmitting.";
      return new IssueWriteError(msg, { status: 409, kind: "duplicate_create", disposition, operationId: parsed.operationId });
    }
    if (parsed && typeof parsed.id === "number" && typeof parsed.state === "string") {
      return new IssueWriteError("The issue on GitHub could not be confirmed as matching this operation yet.", {
        status: 409,
        kind: "recheck_unresolved",
        disposition,
        operation: parsed as unknown as WriteOperation,
      });
    }
  }
  const fallback: Record<number, [IssueWriteErrorKind, string]> = {
    400: ["validation", "Check the issue fields and try again."],
    401: ["unauthenticated", "Your GitHub session expired. Sign in again to write to GitHub."],
    403: ["forbidden", "Your current GitHub permission does not allow this action."],
    404: ["not_found", "This repository or issue is not available for writing."],
    422: ["unprocessable", "GitHub could not accept this request."],
    502: ["transport", "No reliable answer from the server. Your draft is preserved; check Write history before retrying."],
    503: ["unverified", "GitHub access could not be verified right now. Try again shortly."],
    504: ["transport", "The request timed out before a reliable answer arrived. Your draft is preserved; check Write history before retrying."],
  };
  const [kind, defaultMessage] = fallback[response.status] ?? ["server", "Could not complete the issue request."];
  return new IssueWriteError(text || defaultMessage, { status: response.status, kind, disposition, retryAfter });
}

import { plural } from "@/features/analytics/format";
import { warnNoteClass } from "@/features/repositories/repository-list";

export function WithheldNote({ withheld, retryAt }: { withheld: number; retryAt: string | null }) {
  if (!withheld && !retryAt) return null;
  return <p role="status" className={warnNoteClass}>
    {withheld > 0 && `${withheld} ${plural(withheld)} hidden (access could not be verified).`}
    {retryAt && ` GitHub rate limited the access check; retry after ${new Date(retryAt).toLocaleTimeString()}.`}
  </p>;
}

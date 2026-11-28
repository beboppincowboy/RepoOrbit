import { clearAllIssueDrafts } from "@/features/issues/drafts";
import { getCsrfToken, restFetch, setCsrfToken, type ViewerSession } from "@/shared/api/transport";

export type { ViewerSession };

async function fetchViewerSession(): Promise<ViewerSession> {
  const response = await restFetch("/auth/me");
  if (!response.ok) throw new Error("Unable to load session");
  const result: ViewerSession = await response.json();
  setCsrfToken(result.csrfToken);
  return result;
}
// Fetches the current session on workspace entry and supplies the CSRF token for subsequent requests.
export const sessionQuery = { queryKey: ["session"], queryFn: fetchViewerSession, staleTime: 0 } as const;
// Ends the current session in Go. The caller owns navigation and local query state. Rejects on HTTP or network failure.
export async function logout() {
  const response = await fetch("/auth/logout", { method: "POST", headers: { "X-CSRF-Token": getCsrfToken() } });
  if (!response.ok) throw new Error("Could not sign out. Try again.");
  await clearAllIssueDrafts();
}

import { GraphQLUnavailableError, RepositoryAccessUnverifiedError } from "./errors";

export type ViewerSession = { mode: "demo" | "real"; user: { login: string; demo: boolean } | null; csrfToken: string | null };
export type MetricWindow = 7 | 30 | 90;
export type Coverage = { covered: boolean; reason: string | null; through: string | null };
export const coverageFields = "covered reason through";
// "merged" | "closed", or for an open PR the human reviewers' latest decision; null while its review history is not imported.
export type ReviewState = "awaiting_review" | "commented" | "approved" | "changes_requested" | "merged" | "closed";

// The only copy of the session's CSRF token. Loading the session sets it; every request that needs it reads it here.
let csrfToken: string | null = null;
export function getCsrfToken(): string {
  return csrfToken ?? "";
}
export function setCsrfToken(token: string | null): void {
  csrfToken = token;
}

type RestRequest = { method?: "GET" | "POST" | "PUT" | "DELETE"; json?: unknown; headers?: Record<string, string>; signal?: AbortSignal };

// An uncached REST request. Non-GET requests carry the CSRF token, and json is sent as a JSON body.
// Network failures, cancellation and HTTP statuses are left to the caller.
export function restFetch(path: string, { method, json, headers, signal }: RestRequest = {}): Promise<Response> {
  const sent: Record<string, string> = { ...headers };
  if (method && method !== "GET") sent["X-CSRF-Token"] = getCsrfToken();
  if (json !== undefined) sent["Content-Type"] = "application/json";
  return fetch(path, { method, headers: sent, cache: "no-store", signal, body: json === undefined ? undefined : JSON.stringify(json) });
}

type GraphQLResponse<Data> = {
  data?: Data | null;
  errors?: { message?: string; path?: (string | number)[]; extensions?: { code?: string; retryAt?: string } }[];
};

// Resolves with the response's data and errors for the caller to judge. A network failure or temporary gateway
// error rejects with GraphQLUnavailableError; cancellation rejects with the fetch's own error.
export async function graphqlQuery<Data>(queryText: string, variables: Record<string, unknown>, signal?: AbortSignal): Promise<GraphQLResponse<Data>> {
  const response = await fetch("/graphql", { method: "POST", headers: { "Content-Type": "application/json", "X-CSRF-Token": getCsrfToken() }, cache: "no-store", signal, body: JSON.stringify({ query: queryText, variables }) }).catch((error: unknown) => {
    if (signal?.aborted || !(error instanceof TypeError)) throw error;
    throw new GraphQLUnavailableError("RepoOrbit could not reach the server.");
  });
  if ([502, 503, 504].includes(response.status)) throw new GraphQLUnavailableError("The server is temporarily unavailable.");
  if (!response.ok) throw new Error("Unable to load repositories");
  return response.json();
}

// graphqlQuery that accepts only complete data: unverified access rejects with RepositoryAccessUnverifiedError,
// and any other error rejects with fallbackError.
export async function guardedGraphql<T>(query: string, variables: Record<string, unknown>, fallbackError: string, signal?: AbortSignal): Promise<T> {
  const { data, errors } = await graphqlQuery<T>(query, variables, signal);
  const accessError = errors?.find(error => error.extensions?.code === "ACCESS_UNVERIFIED");
  if (accessError) throw new RepositoryAccessUnverifiedError(accessError.extensions?.retryAt ?? null);
  if (!data || errors?.length) throw new Error(fallbackError);
  return data;
}

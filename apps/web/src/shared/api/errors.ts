// A network failure or temporary HTTP gateway error. The message explains why a saved view may be shown.
export class GraphQLUnavailableError extends Error {}

// GitHub access is unverified. retryAt is a UTC timestamp, or null when the server supplies no retry time.
export class RepositoryAccessUnverifiedError extends Error {
  constructor(readonly retryAt: string | null) {
    super("Access could not be verified");
  }
}

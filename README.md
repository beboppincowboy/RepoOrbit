# RepoOrbit

RepoOrbit is a GitHub workspace for understanding repository activity, finding contribution opportunities, and tracking delivery. Connect repositories you own or follow repositories you have starred. The workspace brings together issues, pull requests, reviews, contributor participation, and release history, with links back to the relevant GitHub records.

## Repository intelligence

- **Engineering overview** summarizes selected repositories and surfaces PRs awaiting review, issues without a response, stale open issues, failed workflow runs, and work assigned to you, alongside overall build success rate.
- **Repository workspace** shows 7-, 30-, and 90-day metrics, a pull request list, contributor activity, a timeline of merges, releases, and issue events, an issue Board, and GitHub Actions health.
- **Repository selection** lets you choose what to sync. GitHub sign-in identifies the viewer, and GitHub App installations grant access to selected private repositories. Each viewer controls their own selection.

Metrics carry coverage information. An unfinished import leaves an unknown value where history is incomplete. The timeline marks gaps in recorded events, and contributor views distinguish human participation from bot activity.

## Architecture

RepoOrbit is a modular monolith: one Go module (`backend/`), two Go processes (`api` and `worker`) sharing a single PostgreSQL 17.5 database, and a separate Bun web process (`apps/web/server.mjs`) that serves the built React SPA and proxies same-origin requests to the API. See [ARCHITECTURE.md](ARCHITECTURE.md) for package boundaries, composition roots, and architectural decisions.

```mermaid
flowchart LR
    Browser --> Web["Bun web server (apps/web/server.mjs)<br/>React SPA + same-origin proxy"]
    Web -->|"/auth, /api, /graphql"| API["Go API (backend/cmd/api)"]
    API -->|"Guarded reads (readguard), REST (issues),<br/>auth, selections, and job enqueue"| DB[("PostgreSQL 17.5")]
    API -->|"Sign-in and viewer access checks"| GitHub["GitHub"]
    GitHub -->|"Signed webhooks (/webhooks/github)"| API
    DB -->|"Transactional job queue (jobs)<br/>and checkpoints"| Worker["Go worker (backend/cmd/worker)"]
    Worker -->|"Paginated activity reads"| GitHub
    Worker -->|"Fenced projection writes (ingestion)<br/>and coverage updates"| DB
```

The web app (`apps/web/`) is built with TanStack Start/Router, React 19, Tailwind CSS, Radix UI, TanStack Query, TanStack Table, and ECharts, organized into `src/routes/`, `src/features/` (`analytics`, `auth`, `board`, `issues`, `repositories`), and `src/shared/`. In production, `apps/web/server.mjs` runs on Bun (`oven/bun:1.4.5-alpine`), serves static assets from `dist/client`, and streams `/auth`, `/api`, and `/graphql` to the Go API without holding state.

The Go backend (`backend/`) separates explicit commands and read models across `cmd/api` and `cmd/worker`:

- **`cmd/api`** runs migrations (and Demo mode seeding when GitHub credentials are unset), then serves `/auth`, `/graphql`, `/api`, `/webhooks/github`, `/live`, and `/ready`. Every non-demo GraphQL read goes through `internal/readguard` (user-scoped access fence, fresh access check, read callback, and post-read fence check), while `internal/board` and `internal/analytics` (`REPEATABLE READ` read-only transactions) serve the Board, overview, and delivery read models. REST issue commands live in `internal/issues` (`domain`, `application`, and per-repository access fences) and call `internal/ingestion` as the single owner of issue projection writes.
- **`cmd/worker`** runs the PostgreSQL-backed job queue (`internal/jobs`), GitHub ingestion and projection (`internal/ingestion`), periodic scheduling, and startup reconciliation, exposing `/live` and `/ready` on internal port `8081`.

### Synchronization and access

1. Connecting or following a repository records the selection and enqueues background work in the same database transaction. Workers claim jobs by renewable lease and resume imports from saved cursors. Stable GitHub IDs let a retried page update existing records.
2. The worker tracks issues, PRs, reviews, comments, releases, and issue events separately. A stalled import can leave one metric incomplete while other data remains readable. Signed webhooks trigger reconciliation, and periodic polling catches missed changes.
3. Eligible activity imports use short-lived GitHub App installation tokens. The viewer's token handles access checks and public activity imports outside the installation. Rate limits defer work until the next retry.
4. GraphQL reads enforce `internal/readguard` access fences before and after reading repository data, preventing reads against stale permissions. After a temporary connection failure or rate limit, a workspace the viewer already loaded can show a read-only snapshot with its timestamp and the reason it is frozen.

### Issue management, discovery, and delivery

The application architecture uses GitHub as the source of truth for issue management, discovery, and delivery.

| Area | Data and permission model |
| --- | --- |
| **Issue management and Board** | Tasks map to GitHub issues. Board columns come from a repository's status labels (`status:todo`, `status:in-progress`, `status:review`); repositories without them show issues under "No status source". Only Admin or Maintain users turn status labels on, which creates any missing labels through review, or off. When an issue closed on GitHub keeps a status label, the GitHub App removes it during sync where the installation has Issues write, and records the removal as its own. Issue edits and card moves go through review, user-attributed submission, and confirmation. An idempotency key identifies each submission. An uncertain result keeps the draft and operation record available for reconciliation before another attempt. |
| **Contribution discovery** | Recommendations draw from accessible repositories the viewer has explicitly followed from their stars. Matching uses skill and interest preferences, difficulty labels, issue availability, and repository responsiveness. Matches explain their evidence and freshness. Saved and dismissed issues are scoped to the viewer. |
| **Delivery monitoring** | Read-only GitHub Actions data supplies run and job status, success rates, p50/p95 durations, queue wait, and failures by workflow. Confirmed GitHub links connect an issue to its PR, review state, checks, and deployment, and each failed check leads to its job and GitHub logs. Deployment history comes only from GitHub Deployment records; without them, deployment data is shown as unavailable. |

## Run locally

With Docker Compose, start the database, API, worker, and web app:

```sh
docker compose up --build
```

Open [RepoOrbit](http://localhost:3000). With GitHub credentials unset, the app runs in Demo mode against seeded PostgreSQL data. The database persists in a Docker volume.

### Connect GitHub

1. Register a GitHub App. Use `http://localhost:3000/auth/github/callback` as its authorization callback and `http://localhost:3000/auth/github/installed` as its setup URL. Enable expiring user tokens and redirect on update, leave "Request user authorization (OAuth) during installation" unticked, and allow installation on any account.
2. Grant the app repository **Issues** read and write access, **Pull requests**, **Contents**, **Actions**, and **Deployments** read access, and account **Starring** read access. **Projects** access is requested only when a maintainer links a GitHub Project. Install the app on the repositories you want it to cover.
3. Copy `.env.example` to `.env` and set `GITHUB_CLIENT_ID`, `GITHUB_CLIENT_SECRET`, and `GITHUB_APP_SLUG`. Store the private key in `secrets/` and set `GITHUB_APP_PRIVATE_KEY_FILE` to its path under `/run/repoorbit-secrets/` inside Compose.
4. Set `TOKEN_ENCRYPTION_KEY` to 32 random bytes encoded in base64 and set `WEBHOOK_SECRET` to a random secret. To receive GitHub events, configure the app webhook to send to a reachable API URL ending in `/webhooks/github` and use the same secret on both sides.
5. Restart Compose and sign in through the same origin you configured in `APP_URL`. Keep `.env` and the private key local.

## Development

| Path | Responsibility |
| --- | --- |
| [`apps/web/src/routes/`](apps/web/src/routes/) | TanStack Start/Router pages and shell layout |
| [`apps/web/src/features/`](apps/web/src/features/) | Feature modules (`analytics`, `auth`, `board`, `issues`, `repositories`) |
| [`apps/web/src/shared/`](apps/web/src/shared/) | Shared API transport/errors, UI primitives, and utilities |
| [`apps/web/server.mjs`](apps/web/server.mjs) | Production Bun server serving `dist/client` and proxying `/auth`, `/api`, `/graphql` |
| [`backend/cmd/`](backend/cmd/) | Go `api`, `worker`, and `fakegithub` entry points |
| [`backend/graphql/`](backend/graphql/) | GraphQL schema (`gqlgen`) and transport mapping |
| [`backend/internal/readguard/`](backend/internal/readguard/) | Four-step guarded GraphQL read helper and user-scoped access fences |
| [`backend/internal/board/`](backend/internal/board/) & [`backend/internal/analytics/`](backend/internal/analytics/) | Board, overview, and delivery read models |
| [`backend/internal/issues/`](backend/internal/issues/) | REST issue management, domain/application commands, and per-repo access fences |
| [`backend/internal/ingestion/`](backend/internal/ingestion/) | GitHub activity imports, reconciliation, cleanup, and single issue projection owner |
| [`backend/internal/jobs/`](backend/internal/jobs/) | PostgreSQL-backed transactional job queue and worker runner |
| [`backend/internal/auth/`](backend/internal/auth/), [`repositories/`](backend/internal/repositories/), [`accessfence/`](backend/internal/accessfence/), [`githubapp/`](backend/internal/githubapp/) | GitHub OAuth/App auth, repository selection, access checks, and fencing |
| [`backend/migrations/`](backend/migrations/) and [`backend/queries/`](backend/queries/) | PostgreSQL schema (`goose`) and `sqlc` queries |

Use Go 1.24 (`GOTOOLCHAIN=go1.24.6` in `Makefile`) and Bun 1.4.5 for local development. Bun installs web dependencies with `bun install --frozen-lockfile` in `apps/web` and runs Vite, Playwright, ESLint, `tsc`, and the production web server (`server.mjs`), so Node is not needed.

- `make check`: runs `gofmt`, `go vet`, `sqlc`/`gqlgen` generated-code drift check, and web lint, typecheck, and build.
- `make generate`: regenerates `sqlc` and `gqlgen` output.
- `make test` / `make test-race`: runs Go unit tests (with or without `-race`).
- `make test-integration`: runs Go integration tests against the Compose PostgreSQL database (`DATABASE_URL`, default `postgres://repoorbit:repoorbit@localhost:54329/repoorbit?sslmode=disable`).
- `make test-e2e`: runs Playwright tests against Compose (`bun run --bun playwright install --with-deps chromium` in `apps/web` first).
- `make test-e2e-auth`: exercises sign-in and repository access against `cmd/fakegithub` (`scripts/e2e-auth.sh`).

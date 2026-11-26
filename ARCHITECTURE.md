# Architecture

RepoOrbit is a modular monolith: one Go module, two Go processes that share one PostgreSQL database, and a separate web process that serves the single-page app.

## Overview

| Process | Source | What it does |
|---|---|---|
| web | `apps/web` (`server.mjs`) | Serves the built React SPA on port 3000 and proxies API calls to `API_ORIGIN`. Holds no state. |
| api | `backend/cmd/api` | HTTP API on port 8080: GitHub sign-in (`/auth`), REST (`/api`), GraphQL (`/graphql`), GitHub webhooks (`/webhooks/github`) and health (`/live`, `/ready`). Runs database migrations and, in Demo mode, the demo seed at startup. |
| worker | `backend/cmd/worker` | Runs the PostgreSQL job queue: GitHub ingestion, the periodic scheduler and startup reconcile. Serves health on port 8081 only. Idle in Demo mode. |
| db | `postgres:17.5-alpine` | The only datastore. Holds application data and the job queue, so a job is enqueued in the same transaction as the change that needs it. |

## Module map

Backend packages and their imports of other `github.com/repoorbit/repoorbit/backend/...` packages. Generated with:

```bash
cd backend && go list -f '{{.ImportPath}}: {{join .Imports " "}}' ./...
```

| Package | Internal imports |
|---|---|
| `cmd/api` | `graphql`, `internal/auth`, `internal/db`, `internal/githubapp`, `internal/ingestion`, `internal/issues`, `internal/jobs`, `internal/platform`, `internal/repositories` |
| `cmd/worker` | `internal/auth`, `internal/db`, `internal/githubapp`, `internal/ingestion`, `internal/jobs`, `internal/platform`, `internal/repositories` |
| `cmd/fakegithub` | `internal/githubapp/fake` |
| `graphql` | `graphql/model`, `internal/accessfence`, `internal/analytics`, `internal/auth`, `internal/db`, `internal/ingestion`, `internal/jobs`, `internal/repositories` |
| `graphql/model` | `internal/analytics` |
| `internal/issues` | `internal/accessfence`, `internal/auth`, `internal/db`, `internal/githubapp`, `internal/ingestion`, `internal/repositories` |
| `internal/ingestion` | `internal/accessfence`, `internal/auth`, `internal/db`, `internal/githubapp`, `internal/jobs`, `internal/repositories` |
| `internal/repositories` | `internal/accessfence`, `internal/auth`, `internal/db`, `internal/githubapp` |
| `internal/auth` | `internal/accessfence`, `internal/db`, `internal/githubapp` |
| `internal/accessfence` | `internal/db` |
| `internal/analytics` | `internal/db` |
| `internal/jobs` | `internal/db` |
| `internal/testutil` | `internal/platform` |
| `internal/db` | none (sqlc output) |
| `internal/githubapp` | none |
| `internal/githubapp/fake` | none |
| `internal/platform` | none |

## Composition roots

Both binaries build their services by hand in `main.go`: no dependency-injection framework. Each loads `platform.LoadConfig()`, opens one `pgxpool` and creates one `db.Queries`.

**`backend/cmd/api`**

- Runs `platform.Migrate` and, when `cfg.Demo` is set, `platform.SeedDemo` before opening the pool.
- Demo mode wires only health, `/auth` and `/graphql`. The GitHub client, `/api` and `/webhooks/github` are not mounted.
- Otherwise it builds the GitHub client, `auth.Service` and `repositories.Service`, then connects repositories to ingestion through callbacks so neither package imports the other's write path:
  - `OnSelect` requests a metadata refresh and an activity sync in the selection's transaction.
  - `OnEligibilityChange` (also used as `auth.Service.OnSignIn`) schedules an eligibility recheck.
  - `SignalActivity` forwards activity signals to ingestion.
- `issues.Service` registers its routes on the `/api` mux and uses `repositories.Service` as its access check. The `/api` mux and `/graphql` sit behind `auth.Handler.Protect`. The webhook handler stays outside it because GitHub authenticates by HMAC signature.

**`backend/cmd/worker`**

- Builds the same `auth.Service` and `repositories.Service`, plus a `jobs.Worker` with `jobs.SystemClock`.
- `ingestion.Deps.Register(worker)` registers the job handlers. `ingestion.StartScheduler` adds periodic work, with `starRefresher` adapting `repositories.Service` to `ingestion.StarRefresher`.
- `ingestion.ReconcileSelections` schedules work for existing selections at startup.
- On shutdown it waits for in-flight jobs to finish or be released before closing the health server.

## Decisions

From #28. Every change follows these:

1. Modular monolith: the API and worker stay separate processes, one PostgreSQL database, no service split, no network RPC.
2. Interfaces (ports) are defined by the package that consumes them. No generic repository, ORM, dependency-injection framework, reflection-based command bus, or shared `ports` package.
3. Lightweight CQRS: explicit commands and queries. Existing SQL views and tables are the read models. Reads perform no business writes; the security bookkeeping that already happens on reads is allowed and documented.
4. Keep the PostgreSQL job queue and its transactional enqueue. No outbox, Kafka, RabbitMQ, SQS or Redis.
5. No schema migrations and no SQL rewrites. Additive read-only sqlc queries only where an issue says so.
6. Bun 1.4.5 is pinned in `apps/web/package.json` (`packageManager`), the Docker images and CI. Playwright, ESLint and tsc stay on Node 22.
7. Production keeps a separate web process (`apps/web/server.mjs`, running on Bun). Go does not serve the SPA. `/webhooks/github`, `/live` and `/ready` stay reachable only on the API.
8. Observability uses `log/slog` and the Prometheus Go client, served on internal listeners only.
9. The architecture is documented in a tracked root `ARCHITECTURE.md`.
10. sqlc and gqlgen output is regenerated only with `make generate` (the Makefile pins `GOTOOLCHAIN=go1.24.6`; gqlgen exits silently and leaves files missing under newer Go).

## Safety properties

The properties every change must keep are listed in the safety properties table in #28 ("Safety properties that must hold after every PR").

## Local development and testing

| Command | What it runs | Ports |
|---|---|---|
| `(cd apps/web && bun install --frozen-lockfile)` | Installs web dependencies | none |
| `make check` | gofmt, `go vet`, generated-code drift check, web lint, typecheck and build | none |
| `make test` | Go unit tests | none |
| `make test-race` | Go unit tests with the race detector | none |
| `docker compose up -d --build --wait` | Builds and starts db, api, worker and web | 54329 (db), 8080 (api), 3000 (web); worker health 8081 inside its container only |
| `make test-integration` | Go integration tests (`-tags=integration`) | Uses `DATABASE_URL`, default `postgres://repoorbit:repoorbit@localhost:54329/repoorbit?sslmode=disable` |
| `(cd apps/web && ./node_modules/.bin/playwright install --with-deps chromium)` | Installs the Playwright browser | none |
| `make test-e2e` | Playwright suite against the compose web (`BASE_URL`, default `http://127.0.0.1:3000`). `demo.spec.ts` runs SQL through `docker compose exec db` | 3000 |
| `make test-e2e-auth` | Sign-in and repository-access tests against a fake GitHub with the API in real-auth mode (`scripts/e2e-auth.sh`) | 9090 (fake GitHub), 8090 (api), 3100 (web); db via `E2E_DB_HOST`, default `localhost:54329` |

If 3000, 8080 or 54329 are taken, add an untracked `compose.override.yaml` that replaces each service's ports with `ports: !override [...]`, then point `DATABASE_URL`, `BASE_URL` and `E2E_DB_HOST` at the new ports.

## Issue writes

_To be written._

## Read side

_To be written._

## Deployment

_To be written._

## Observability

_To be written._

## Rollback

_To be written._

# RepoOrbit

Without GitHub credentials RepoOrbit runs in **Demo mode**: seeded repositories and issues under a labeled demo identity, no GitHub connection, no synchronization data. With `GITHUB_CLIENT_ID`, `GITHUB_CLIENT_SECRET` and `TOKEN_ENCRYPTION_KEY` set it runs in **real mode**: sign in with GitHub is required and demo rows are never shown. Register the app first; setting only one credential stops startup. `make test-e2e-auth` runs the sign-in flow against a fake GitHub.

Run `docker compose up --build`, then open http://127.0.0.1:3000. Compose starts PostgreSQL, the API, a readiness-only worker, and the web app. PostgreSQL persists in a Docker volume. On each API start, migrations run and, in Demo mode only, sample records are upserted; other records are retained. The demo has no remote write operations. Stop with `docker compose down`; add `-v` only when you intend to delete the local database.

`make check`, `make test`, `make test-integration`, `make test-e2e`, and `make test-race` provide local gates. Integration tests use the local Compose PostgreSQL database; Playwright tests require the Compose stack and `npx playwright install chromium` in `apps/web`. Go 1.24 and Node 22 are required. Generators are pinned in the Makefile and Go module. `.env.example` documents local development defaults; Compose provides its own container database URL.

The stack reserves Recharts for the analytics phase; no chart dependency is installed before there is chart data. The U1 interface uses Tailwind and a local shadcn-style Button component.

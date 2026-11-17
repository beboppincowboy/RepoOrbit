#!/usr/bin/env bash
# Runs the sign-in and repository-access browser tests against a fake GitHub, with the API in real-auth mode.
# Needs the compose database (docker compose up -d db) and Node dependencies. Uses ports 9090, 8090, 3100.
set -euo pipefail
cd "$(dirname "$0")/.."
export GOTOOLCHAIN=${GOTOOLCHAIN:-go1.24.6}
DB=repoorbit_auth_e2e
docker compose exec -T db psql -U repoorbit -d postgres -c "DROP DATABASE IF EXISTS $DB WITH (FORCE)" -c "CREATE DATABASE $DB" >/dev/null
BIN=$(mktemp -d)
PIDS=()
trap 'kill "${PIDS[@]}" 2>/dev/null || true; rm -rf "$BIN"; docker compose exec -T db psql -U repoorbit -d postgres -c "DROP DATABASE IF EXISTS $DB WITH (FORCE)" >/dev/null || true' EXIT
export GITHUB_CLIENT_ID=e2e-id GITHUB_CLIENT_SECRET=e2e-secret GITHUB_APP_SLUG=app
export TOKEN_ENCRYPTION_KEY=$(openssl rand -base64 32)
export APP_URL=http://127.0.0.1:3100 GITHUB_URL=http://127.0.0.1:9090 GITHUB_API_URL=http://127.0.0.1:9090
export DATABASE_URL="postgres://repoorbit:repoorbit@localhost:54329/$DB?sslmode=disable"
(cd backend && go build -o "$BIN/api" ./cmd/api && go build -o "$BIN/fakegithub" ./cmd/fakegithub)
"$BIN/fakegithub" & PIDS+=($!)
(cd backend && API_ADDR=127.0.0.1:8090 exec "$BIN/api") & PIDS+=($!)
(cd apps/web && API_ORIGIN=http://127.0.0.1:8090 exec npx next dev -p 3100) & PIDS+=($!)
for url in http://127.0.0.1:8090/ready http://127.0.0.1:3100/login; do
  for _ in $(seq 60); do curl -fs -o /dev/null "$url" && break; sleep 1; done
  curl -fs -o /dev/null "$url"
done
cd apps/web && E2E_AUTH_URL=http://127.0.0.1:3100 npx playwright test tests/auth.spec.ts tests/access.spec.ts

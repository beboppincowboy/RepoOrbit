# Registering the RepoOrbit GitHub App

Sign-in needs a GitHub App that you register once. RepoOrbit runs in Demo mode without it, and its tests use a fake GitHub, so nothing here blocks development. Live sign-in stays unverified until this is done.

1. GitHub → Settings → Developer settings → GitHub Apps → New GitHub App.
2. **Callback URL:** `<APP_URL>/auth/github/callback` (for local use, `http://localhost:3000/auth/github/callback`).
3. **Expire user authorization tokens:** leave on. RepoOrbit refreshes them.
4. **Setup URL:** `<APP_URL>/auth/github/installed`, with **Redirect on update** ticked. Leave **Request user authorization (OAuth) during installation** *unticked*: when it is on, GitHub ignores the setup URL.
5. **Webhook:** tick Active. **Webhook URL:** `<public API origin>/webhooks/github` (the API, not the web app; locally use a tunnel to `http://127.0.0.1:8080`). **Secret:** `openssl rand -hex 32`, the same value as `WEBHOOK_SECRET`. Subscribe to: Installation, Installation repositories, Repository, Push, Issues, Issue comment, Pull request, Pull request review, Pull request review comment, Release. GitHub always sends GitHub App authorization (revocation) events to the app. See the Webhooks section of `docs/operations.md`.
6. **Permissions** (repository): Issues — read and write; Pull requests — read; Contents — read; Starring (account) — read. Metadata read is implied. Contents is for private release metadata only; RepoOrbit never ingests source code.
7. **Where can this app be installed?** Any account, so other people can sign in.
8. Create the app, then generate a client secret and a private key. Store the downloaded PEM securely in a directory readable by the API and worker.
9. For Compose, put the PEM in `./secrets` (or set `GITHUB_APP_KEY_DIR` to its host directory) and set `GITHUB_APP_PRIVATE_KEY_FILE=/run/repoorbit-secrets/<filename>`. For non-Compose deployment set it to the absolute path readable by both processes. Existing real-mode deployments must add this key before upgrading; startup validates it. Demo requires no key.
10. Put `WEBHOOK_SECRET`, `GITHUB_CLIENT_ID` (the app's Client ID) and `GITHUB_CLIENT_SECRET` in `.env` or the shell, with `TOKEN_ENCRYPTION_KEY=$(openssl rand -base64 32)` and `APP_URL`, and run `docker compose up --build`. Lose the encryption key and everyone must sign in again.

Set `GITHUB_APP_SLUG` to the app's URL name (the last part of `github.com/apps/<slug>`) so the Repositories page can link to "Install the app". Without it the page tells people to install the app on GitHub themselves.

Installing the app on a personal account returns the browser to the Setup URL, and RepoOrbit accepts the installation only if it appears in that signed-in user's own installations. Private repositories appear on the Repositories page only after the app is installed on them. Whether a user token sees only that intersection is assumed from GitHub's documentation and must be confirmed at live acceptance.

Starting with only one of the two credentials is an error, by design.

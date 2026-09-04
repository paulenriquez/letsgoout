# AGENTS.md

## Project Overview

Let's Go Out is a dependency-light invitation web app packaged as a single Go binary.

- Backend: Go 1.26, `net/http`, and SQLite through `modernc.org/sqlite`.
- Frontend: plain HTML, CSS, and JavaScript with no package manager or build step.
- Deployment: multi-stage Docker image running as a non-root user.
- Persistence: numbered SQL migrations embedded into the binary.

## Repository Map

- `main.go`: configuration, database startup, embedded files, migrations, server lifecycle, and graceful shutdown.
- `app.go`: HTTP routes, validation, token handling, persistence, rate limiting, security headers, and cleanup.
- `app_test.go`: unit and integration coverage for the backend, migrations, security behavior, and embedded static assets.
- `index.html`, `styles.css`, `app.js`: browser UI, creator preview, and hash-based invite/status routing.
- `service-worker.js`: tombstone worker that removes legacy PWA caches and unregisters itself.
- `favicon.ico`, `favicon.svg`, `fonts/`: embedded icons and self-hosted font assets.
- `migrations/`: ordered SQLite schema migrations.
- `Dockerfile`: production build and runtime image.
- `README.md`: user-facing setup and project overview.

## Working Conventions

- Inspect `git status` and relevant diffs before editing. Preserve existing user changes and do not revert unrelated work.
- Keep the application small and dependency-light. Prefer the Go standard library and browser-native APIs unless a new dependency provides clear value.
- Run `gofmt` on every modified Go file.
- Keep API JSON fields in `snake_case` and update the Go request/response logic, frontend response validators, and tests together when contracts change.
- Use the injected clock and randomness (`a.now` and `a.random`) in application logic so behavior remains deterministic in tests.
- Add focused regression tests for bug fixes and integration tests for changes spanning handlers, storage, or migrations.

## Database Migrations

- Determine the next zero-padded numeric prefix from the highest existing migration. For example, if `004` is latest, add `005_description.sql`.
- Never rewrite a migration that may already have been deployed. Add a new migration instead.
- Migrations run transactionally and are embedded through the `//go:embed` directive in `main.go`.
- Schema changes must preserve existing invite data and update record scanning, API behavior, and migration tests as needed.
- Keep SQLite compatibility in mind; verify upgrade behavior from the previously released schema.

## Frontend and Embedded Assets

- Design and implement all UI changes mobile-first. Base styles must target narrow screens, with larger-screen enhancements added through `min-width` media queries.
- Treat mobile layout and touch interaction as the primary experience; desktop layouts must progressively enhance it without changing core behavior.
- The frontend is served directly from the embedded filesystem. A file is not available at runtime merely because it exists in the repository; it must also match the `//go:embed` patterns in `main.go`.
- PWA support is intentionally disabled. Preserve the legacy cleanup in `app.js` and the unregistering tombstone in `service-worker.js`; do not add a manifest, worker registration, or fetch-cache handler unless deliberately restoring PWA support and updating the related tests and cache policy.
- Keep DOM IDs and event bindings synchronized across `index.html` and `app.js`.
- Keep browser-side validation aligned with backend limits and allowed values. The backend remains the authoritative validator.
- Render user-provided values with safe DOM APIs such as `textContent`; do not introduce HTML interpolation for untrusted data.
- `index.html` is served with `no-cache`; all other static assets are cached for one day. When changing a referenced static asset, update its cache-busting query version or filename. This includes the `app.js`, `styles.css`, and favicon references in `index.html`, plus font URLs in `styles.css` when font bytes change.
- Preserve keyboard usability, touch-friendly controls, loading states, and visible error feedback.

## Security and Privacy

- Treat invite tokens, private status tokens, token hashes, and IP-derived rate-limit data as sensitive. Do not log or expose them unnecessarily.
- Preserve exact-origin checks, request body limits, unknown-field rejection, security headers, and generic not-found responses.
- Do not expose the private status capability through the recipient invite endpoint.
- Keep state-changing operations as same-origin JSON API requests.
- Production deployments require a `RATE_LIMIT_HMAC_KEY` containing at least 32 bytes. `DISABLE_RATE_LIMITS=true` is only for local development and tests.

## Development and Verification

Run the application locally with a disposable database:

```sh
PUBLIC_BASE_URL=http://localhost:8080 \
DATABASE_PATH=/tmp/letsgoout.db \
DISABLE_RATE_LIMITS=true \
go run .
```

Before completing a change, run:

```sh
gofmt -w <modified-go-files>
go test ./...
go vet ./...
go build ./...
```

For concurrency, token, rate-limit, or persistence changes, also run:

```sh
go test -race ./...
```

When deployment behavior changes, verify the container build:

```sh
docker build -t letsgoout .
```

For frontend changes, manually exercise create-form editing (including adding and removing custom ideas and time options), creator preview, link generation and sharing, invite response, accepted-invite revisiting, pending-status auto-refresh and error/offline handling, unavailable states, and deletion. Start verification at a narrow mobile viewport, including touch-sized controls and long content, then verify the progressively enhanced desktop layout. Check keyboard navigation and the browser console for errors.

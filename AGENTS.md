# scry

RSS reader in Go, and the source of the org's Go app template. Spec:
[docs/design.md](docs/design.md). Build order: [docs/initial_milestone.md](docs/initial_milestone.md).

## Build and test

- `make build` / `make run` (reads `.env`; copy `.env.example`)
- `make test`, `go test -race ./...`, `make vet`, `gofmt -l .` (must be empty)
- `make tools` then `make vulncheck`
- `scry doctor` validates config, checks voodoo's `/healthz` (warns only when `SCRY_PROD=false`), warns in dev if the Inngest dev server is down, and fails on pending migrations
- `make tools` also installs sqlc and goose; `make sqlc`, `make migrate-new NAME=x`, `make migrate-check`

## Conventions

- Stdlib `net/http` and `http.ServeMux`, not echo. voodoo uses echo, so code
  copied from it needs porting.
- Config is env vars prefixed `SCRY_`, loaded in `internal/config`; refuse
  unsafe prod settings in `CheckServe`.
- Table tests. Comments say why, not what.
- Follow the milestone order; record decisions and divergences in `docs/design.md`.

## Database

- `internal/db.Open` gives `Writer` (one connection, immediate txlock) and `Reader` (read-only pool). Writes and migrations go through `Writer`.
- Schema lives only in `migrations/` (goose, embedded). Each needs a working `-- +goose Down`; `make migrate-check` proves it.
- Queries go in `internal/db/queries.sql`, ASCII only (see `sqlc.yaml`). Run `make sqlc` and commit the generated code; CI fails if it is stale.

## HTTP and web

- `internal/server`: routes plus plain `func(http.Handler) http.Handler` middleware (log, headers, recover, 64 KB body cap, CSRF via `CrossOriginProtection`). `server.New(db)` is the whole handler.
- `internal/web`: layout, CSS, fonts, vendored htmx (version and sha256 in `web.go`). `make theme` regenerates `theme.css` from `../website`; do not edit it.
- Render pages with `web.Renderer.Render` and a `web.View{Page, Fragment, ...}`: full page, or the fragment on `HX-Request`. Every htmx action must also work as a plain form post (POST, 303 redirect).
- CSP forbids inline script and style: no `hx-on`, no `<script>` bodies, no `style=`. htmx is configured by the `htmx-config` meta tag in `layout.html`.
- The notes and admin pages (`internal/server/example.go`, `views/notes.html`, `views/admin.html`) are EXAMPLES; step 2 deletes them.

## Jobs

- `internal/jobs`: Inngest Go SDK (`inngestgo`), app ID `scry`. `jobs.New(jobs.Deps{...})` creates the client and registers every function; functions get the DB from `Deps`, no globals. Put writes inside `step.Run`.
- The SDK handler is mounted at `/api/inngest` (`jobs.Path`). It skips CSRF and voodoo auth (Inngest signs its requests) and has a 36 MiB body cap instead of 64 KB.
- Config is the SDK's own env vars (`INNGEST_SIGNING_KEY`, `INNGEST_EVENT_KEY`, `INNGEST_DEV`), not `SCRY_`. Prod requires both keys and refuses `INNGEST_DEV`.
- Local dev: `INNGEST_DEV=1` in `.env`, then `npx inngest-cli@latest dev -u http://localhost:8081/api/inngest`; the UI is at http://localhost:8288.
- The `scry/example.ping` and hourly notes-count functions and the admin "Send" button on `/` are EXAMPLES; step 2 deletes them.

## Auth

- voodoo owns sign-in; scry has no users table. `voodooauth.Middleware` (from voodoo's module, `replace`d to `../voodoo` for now) puts the user on the context; read it with `voodooauth.UserFrom(ctx)`. Use voodoo's `id` as the user id.
- Protect routes with `s.auth.RequireUser` / `s.auth.RequireAdmin`. Admin means voodoo's `is_admin`. voodoo down means 503 on protected routes, anonymous elsewhere.
- Local dev: run voodoo on :8080 (`SCRY_VOODOO_URL`); there is no auth bypass flag.
- The request log's route comes from `recordRoute` around the mux, so middleware may call `r.WithContext` freely.

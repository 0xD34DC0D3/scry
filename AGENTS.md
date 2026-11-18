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
- Never edit or delete an applied migration (prod has run them all); undo with a new one, as `00002` drops `00001`'s notes table. Numbering is sequential (`00003_feeds.sql`).
- Times are INTEGER unix seconds.

## HTTP and web

- `internal/server`: routes plus plain `func(http.Handler) http.Handler` middleware (log, headers, recover, 64 KB body cap, CSRF via `CrossOriginProtection`). `server.New(db)` is the whole handler.
- `internal/web`: layout, CSS, fonts, vendored htmx (version and sha256 in `web.go`). `make theme` regenerates `theme.css` from `../website`; do not edit it.
- Render pages with `web.Renderer.Render` and a `web.View{Page, Fragment, ...}`: full page, or the fragment on `HX-Request`. Every htmx action must also work as a plain form post (POST, 303 redirect).
- CSP forbids inline script and style: no `hx-on`, no `<script>` bodies, no `style=`. htmx is configured by the `htmx-config` meta tag in `layout.html`.
- Admin feed catalog: `internal/server/feeds.go`, `views/feeds.html`, under `/admin/feeds` behind `RequireAdmin`. Actions are POST (remove too, so plain forms work). htmx gets the `feeds-panel` fragment; a rejected add answers htmx with 200 and the error inside the fragment (htmx does not swap 4xx), and a plain post with the full page and 400/409. Set `web.View.Status` for a non-200 page.
- Public views (`internal/server/public.go`, `views/public.html`), signed out: `/` latest items, `/feeds` catalog, `/feeds/{id}` one feed. Excerpts only, never `item_contents.html`; item URLs go into `href` as plain strings (never `template.URL`). 50 per page, `?before=<first_seen_at>-<id>` cursor; the Older link swaps in the `items-page` fragment. `setCache`: `public, max-age=60` with no credential, `private, no-store` with one; `Vary: HX-Request, Cookie, Authorization`.

## Jobs

- `internal/jobs`: Inngest Go SDK (`inngestgo`), app ID `scry`. `jobs.New(jobs.Deps{DB: d})` creates the client and registers every function (in `register`); functions get the DB from `Deps`, no globals. Put writes inside `step.Run`.
- `syncFeed` (`syncfeed.go`, event `scry/feed.sync`, data `{feed_id}`; build it with `jobs.SyncFeed(id)`): one step that fetches (`fetch.go`), parses and sanitizes (`parse.go`), then writes in one transaction, returning only a status and item count. A 304 takes the same write with no items. A failed fetch or parse is recorded on the feed (`fail_count`, `last_error`, and backoff computed in the `FeedFailed` UPDATE) and the run ends normally; only DB errors fail the step, so Inngest does not retry a dead feed. Both feed writes skip a paused feed, so a pause during the fetch wins.
- `schedule-feeds` (`schedule.go`, cron every 10 min): step `claim` runs `ClaimDueFeeds` (one UPDATE moving up to 100 due feeds' `next_fetch_at` forward 1 h, returning ids), step `send` sends `scry/feed.sync` for each via `step.SendMany` with event ID `scry-feed-sync-<id>-<claimed time>` so a retry dedups. Lost events are fine: the feed comes due again; no outbox. `sync-feed` runs 1 per feed (key `event.data.feed_id`), 10 overall. The 10th consecutive failure sets `disabled_at` inside `FeedFailed`; admins see it as paused in `/admin/feeds` and Resume re-enables it.
- The fetcher refuses non-public addresses in `net.Dialer.Control`, on the dialled IP, so redirects and DNS rebinding are covered; no env proxy. Tests reach `httptest` (loopback) through the unexported `fetcher.allow`, set to the one test server's ip:port, so any other address still meets the real check. Never add a flag or env var for it.
- The SDK handler is mounted at `/api/inngest` (`jobs.Path`). It skips CSRF and voodoo auth (Inngest signs its requests) and has a 36 MiB body cap instead of 64 KB.
- Config is the SDK's own env vars (`INNGEST_SIGNING_KEY`, `INNGEST_EVENT_KEY`, `INNGEST_DEV`), not `SCRY_`. Prod requires both keys and refuses `INNGEST_DEV`.
- Local dev: `INNGEST_DEV=1` in `.env`, then `npx inngest-cli@latest dev -u http://localhost:8081/api/inngest`; the UI is at http://localhost:8288.

## Auth

- voodoo owns sign-in; scry has no users table. `voodooauth.Middleware` (from voodoo's module, `replace`d to `../voodoo` for now) puts the user on the context; read it with `voodooauth.UserFrom(ctx)`. Use voodoo's `id` as the user id.
- Protect routes with `s.auth.RequireUser` / `s.auth.RequireAdmin`. Admin means voodoo's `is_admin`. voodoo down means 503 on protected routes, anonymous elsewhere.
- Local dev: run voodoo on :8080 (`SCRY_VOODOO_URL`); there is no auth bypass flag.
- The request log's route comes from `recordRoute` around the mux, so middleware may call `r.WithContext` freely.

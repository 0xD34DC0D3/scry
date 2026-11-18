# scry: initial milestone

Ship scry: the template first, then the RSS reader on top of it. Each step ends
with something that runs. `design.md` is the spec; this file is the order.

Every step also ends with: `gofmt -l .` empty, `go vet`, `go test -race ./...`
green, CI green, and `docs/design.md` updated with any decision or divergence.

Until scry has a GitHub remote, "CI green" means the PM runs the CI job's
steps locally (same commands as `.github/workflows/ci.yml`) and they pass.

## 1. Template (no feed code)

Step 1 is the template described in `design.md`. It is split so each part runs
on its own. Nothing in step 1 knows about feeds.

### 1a. Skeleton

- `go.mod`, `cmd/scry` with `serve`, `migrate`, `doctor`; `internal/config`
  (env, `.env` via godotenv, refuse unsafe prod settings like voodoo's
  `CheckServe`).
- `serve` listens on loopback with timeouts and serves `/healthz`.
- Makefile with voodoo's targets, pinned tools, `.gitignore`, `.env.example`,
  LICENSE, README, CONTRIBUTING, `AGENTS.md`.
- CI: gofmt, vet, `go test -race`, govulncheck. Dependabot.

Done: `make run` serves `GET /healthz` 200; CI green on the first push.

### 1b. Database

- `internal/db`: SQLite with WAL, `foreign_keys=ON`, busy timeout; one writer
  (`MaxOpenConns=1`) and a separate read pool.
- Goose migrations embedded from `migrations/`, one example migration.
  sqlc reads `migrations/` as its schema; one example query.
- `migrate up|down|status`; `doctor` checks config, opens both pools, reports
  migration status.
- CI: sqlc output current, `queries.sql` ASCII-only, migrations up, down, up.

Done: `scry migrate up && scry doctor` pass on a fresh DB; CI runs the three
DB checks.

### 1c. HTTP and web

- Body caps, `http.CrossOriginProtection`, security headers (voodoo's set,
  CSP with no inline script), request logging, panic recovery.
- `internal/web`: voodoo's layout, `style.css`, `theme.css`, fonts, favicon,
  `make theme`; vendored htmx served from `/static`.
- One example page. Its handler returns the full page, or a fragment on
  `HX-Request`, via one shared render helper.

Done: the example page renders in a browser and one htmx action swaps a
fragment; tests cover full vs fragment.

### 1d. Auth

- voodoo auth middleware from the shared module: cookie or API key, `/me`
  cached 60 s by credential, 5 s timeout, fail closed, never cache
  "unreachable". Browsers redirect to `/login?next=`; others get 401.
- `RequireUser` and `RequireAdmin`. Example page shows the user; one
  admin-only example page. Logout form posts to voodoo.
- No dev bypass flag: local dev runs voodoo locally (voodoo's integration doc, option 1).

Done: with a local voodoo on :8080 and scry on :8081, logging in at voodoo
shows the email on scry's page; an API key works with curl; a non-admin gets
403 on the admin page.

### 1e. Jobs

- Inngest Go client, `/api/inngest` (signing key required in prod),
  `internal/jobs/` with one example function and one example cron.
- `doctor` checks Inngest config.

Done: `npx inngest-cli dev` discovers the functions; sending the example event
runs it.

### 1f. Deploy

- `deploy/`: `deploy.sh`, `install.sh`, `backup.sh`, Caddyfile, systemd unit,
  env examples, `vulncheck.sh` + allowlist, `ci-setup.sh`. Same shape as
  voodoo with the names changed.
- CI deploy job on push to main, gated on the environment existing.
- `docs/deploy.md`.

Done: `make deploy` to the droplet brings up `scry.0xd34dc0d3.dev/healthz`;
login round-trips through `auth.0xd34dc0d3.dev`.

Done 2026-11-17 (scry e29c9c1): `/healthz` 200 in production, signed-in
browser shows the email, Inngest Cloud synced both example functions, and
the example event added a note. Not checked in production: `/admin` 403 for
a non-admin (needs a non-admin account; covered by tests). Off-box backups
not configured yet.

## 2. Schema and feed catalog

- One migration with the five tables in `design.md`. sqlc queries for the
  catalog.
- Admin pages: add, pause, resume, remove a feed. Remove the example
  migration, query, page, and job from step 1.

Done: an admin adds a feed URL in the browser and sees it listed; a
non-admin cannot.

## 3. Fetch one feed

- `syncFeed {feed_id}`: safe fetch (conditional GET, timeout, 10 MB cap,
  User-Agent, private addresses refused including redirects), parse,
  sanitize with bluemonday `UGCPolicy`, plain-text excerpt, one short write
  that upserts items by `dedup_key` and rewrites content.
- Success or 304 resets `fail_count`; failure counts up and backs off.
- Admin "sync now" sends the event.

Done: admin clicks "sync now" on a real feed and items appear in the DB;
a second sync adds nothing and gets a 304 where the server supports it.
Tests use `httptest` servers, including a redirect to a private address.

## 4. Schedule

- Cron function finds due feeds, moves `next_fetch_at`, sends `syncFeed` for
  each. Concurrency 1 per feed, about 10 overall.
- Disable after 10 failures; admin page lists disabled and failing feeds with
  `last_error`, and can re-enable.

Done: with the Inngest dev server running, feeds sync on their own; a feed
pointing at a dead URL backs off and is disabled after 10 failures.

## 5. Public views

- Latest items across feeds, the catalog, one feed's items. Excerpts only,
  link out. Last 30 days by `first_seen_at`. Cached.

Done: signed out, the three pages render with real data; caching headers
are set.

## 6. Reading

- Subscribe and unsubscribe. Subscribed items with full sanitized HTML.
- Unread per `design.md`; mark one read; mark all read moves
  `read_before_at`. htmx for the actions, working without JS.
- Make it look good: reviewed in a browser on desktop and phone width.

Done: a user subscribes to two feeds, reads items, marks all read, and the
unread count is right after a new sync.

## 7. Pruning

- Daily cron: delete `item_contents` and `item_reads` older than 30 days, in
  batches.

Done: test with old rows shows them pruned in batches and newer rows kept.

## 8. Ship

- Deploy, add a handful of real feeds, watch a day of syncs.
- Split the template into its own repo (step 1's state, cleaned up) and check
  `gonew` produces a running app.

Done: scry is live and the template repo makes a new app that passes CI.

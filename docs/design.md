# scry design

An RSS reader. Its first milestone is also the org's Go app template.

## Stack

- Go, SQLite (`modernc.org/sqlite`), goose, sqlc. Same setup as voodoo.
- HTTP: stdlib `net/http` (`ServeMux` patterns), not echo.
- `html/template` + htmx, voodoo's layout and CSS. Handlers return a full page, or a fragment on `HX-Request`.
- Auth: voodoo session cookie or API key, via `/me`.
- Jobs: Inngest Cloud.

## Template (milestone 1)

scry's first commit is the template: runnable, one example page, CI green, no feed code. After scry ships it moves to its own repo; new projects start from it with `gonew`.

- `cmd/<app>` (`serve`, `migrate`, `doctor`) and `internal/`.
- Env config, `SCRY_` prefix; refuse unsafe prod settings. `SCRY_PROD` defaults to true so a server missing it fails safe; `.env.example` turns it off.
- SQLite: WAL, one writer plus a read pool. Goose migrations in `migrations/`, embedded; sqlc reads them as its schema.
  Writer: `MaxOpenConns(1)`, `_txlock=immediate`. Reader: `mode=ro` (modernc supports it). sqlc output lives in `internal/db` beside `Open`; `doctor` fails on pending migrations.
- HTTP: timeouts, body caps, `CrossOriginProtection`, security headers, `/healthz`.
- voodoo auth middleware, `/me` cached 60 s. The only shared Go code: package `voodooauth` in voodoo's module (`github.com/0xD34DC0D3/voodoo/voodooauth`), stdlib only, plain `net/http` middleware. Everything else is copied.
- Admin is voodoo's `is_admin`, via `RequireAdmin`; no local role table or option.
- Web layout, CSS, vendored htmx. CSP bans inline script and style, so htmx runs with `includeIndicatorStyles` and `allowEval` off (meta tag); no `hx-on`.
- Static files are fingerprinted (`?v=` hash): immutable cache when versioned, 1 h otherwise (fonts, loaded from the CSS).
- Inngest client, `/api/inngest`, `jobs/`.
  Inngest env vars keep the SDK's names (`INNGEST_*`), not `SCRY_`, since the SDK reads them itself; prod requires both keys and refuses `INNGEST_DEV`. `/api/inngest` skips CSRF and voodoo auth (signature-authenticated) and caps bodies at 36 MiB (Inngest's 32 MiB run-state limit plus headroom) instead of 64 KB.
- Pinned tools, CI, govulncheck, `deploy.sh`, `backup.sh`, Caddyfile, systemd.
  Deploy diverges from voodoo's in four ways: `install.sh` runs `scry doctor` (new binary, prod env) after `migrate up` and before installing it; the health check accepts any 2xx because scry's `/healthz` has an empty body; `ci-setup.sh` makes user `deploy-scry` (not `deploy`) so it does not overwrite voodoo's CI key on the shared droplet; the systemd unit leaves outbound HTTPS open (feeds, voodoo, Inngest Cloud). The CI deploy job stays dormant until the repository variable `DEPLOY_HOST` is set. CI checks out voodoo's `main` beside scry for the `../voodoo` replace (see Known issues). `install.sh` reminds the operator to re-sync the Inngest app, which is manual.
- `AGENTS.md`, `docs/design.md`, `docs/initial_milestone.md`.

CI also checks: sqlc output is current, `queries.sql` is ASCII-only (see voodoo `sqlc.yaml`), migrations run up, down, up.

Out: Antfly, MCP, GraphQL, anything app-specific.

## Features

- One feed catalog. Only admins add, pause, or remove feeds, or ask for a sync.
- Users subscribe to feeds and have read/unread state.
- Public view: latest items from all feeds, the catalog, and one feed's items. Excerpts only, linking out. Cached.
- Logged-in view: subscribed items with full HTML. Looking good is a requirement.

## Data

- `feeds(id, url UNIQUE, title, site_url, etag, last_modified, next_fetch_at, fail_count, last_error, disabled_at)`
- `items(id, feed_id, dedup_key, url, title, author, published_at, first_seen_at, UNIQUE(feed_id, dedup_key))`. Kept forever, so an old item is never re-added as new.
- `item_contents(item_id PK, html, excerpt, fetched_at)`. Pruned after 30 days.
- `subscriptions(user_id, feed_id, subscribed_at, read_before_at, PK(user_id, feed_id))`
- `item_reads(user_id, item_id, read_at, PK(user_id, item_id))`. Pruned after 30 days.
- Unread: `first_seen_at` after both `subscribed_at` and `read_before_at`, with no `item_reads` row. "Mark all read" moves `read_before_at`.
- Lists use `first_seen_at`, not `published_at`. Views show the last 30 days.
- `PRAGMA foreign_keys=ON` on every connection; deletes cascade.
- `disabled_at` is the one "not fetched" flag: an admin pause and the auto-disable after 10 failures both set it, and the catalog shows either as paused. Resume clears it, resets `fail_count` and `last_error`, and sets `next_fetch_at` to 0 so the feed is due at once (a new feed starts the same way).
- Unknown text is `''`, not NULL; `disabled_at` and `published_at` are nullable. Indexes beyond keys: `items(first_seen_at)`, `items(feed_id, first_seen_at)`, and `subscriptions(feed_id)`, `item_reads(item_id)` for the cascades.
- Migration `00001` (the template's notes table) stays because production applied it; `00002` drops the table and `00003` creates the schema.
- Feed URLs are trimmed, must be absolute http(s) with a host and no userinfo, at most 2048 bytes after encoding, and are stored with scheme and host lowercased. Adding does not fetch; private addresses are refused by the fetcher (step 3), not at add time.

## Sync

- `syncFeed {feed_id}` does everything for one feed. A cron function sends it for each due feed and moves that feed's `next_fetch_at` forward; admins can send it too.
- Concurrency: 1 per feed, about 10 overall.
- Conditional GET, 15 s timeout, 10 MB cap, User-Agent, private addresses refused (redirects included). Fetch and parse outside the transaction, then one short write. Steps return small values (ids, counts), never feed bodies: each Inngest call must finish inside `serve`'s 30 s write timeout and arrive inside its 10 s read timeout.
- Upsert items by `dedup_key`: guid, else link, else hash of title and date. Content is rewritten on every fetch.
- Success, including 304, resets `fail_count`. A failure counts up and backs `next_fetch_at` off, up to 24 h; after 10 the feed is disabled and shown to admins. Inngest retries cover only blips within one run.
- HTML is sanitized on the way in (bluemonday `UGCPolicy`); the excerpt is plain text.
- Pruning runs daily in batches.
- Step 3 decisions: event `scry/feed.sync`; fetch, parse and write run in one step returning a status and item count; a 304 takes the same write with no items. Fetch or parse failures are recorded and end the run normally (no Inngest retry); only database errors fail the step. Both feed writes skip a paused feed, so a pause during the fetch wins. Backoff is computed in the failure UPDATE from the old `fail_count`: 30 min doubling, capped at 24 h; a success sets `next_fetch_at` to now + 1 h until step 4 owns scheduling. The fetcher checks the dialled IP (covers redirects and DNS rebinding), ignores proxy env vars, allows only http(s) and 5 redirects. Excerpts are 300 runes, taken from the same HTML as the content (content preferred over description). `last_error` keeps 500 bytes.

## Deferred

- GraphQL (gqlgen), until a non-browser client needs it. Resolvers and htmx handlers would wrap the same code.
- TypeScript template, same shape: env names, deploy scripts, docs, look.
- Bookmarks: `bookmarks(user_id, item_id, created_at)`; pruning keeps their `item_contents`. Bookmarking any URL gives it an `items` row with no feed.
- Image proxy, full-article extraction, OPML, search, user-suggested feeds.
- `voodoo/user.deleted`: delete the user's rows.

## Known issues

- scry uses stdlib `net/http`; voodoo uses echo. Code copied from voodoo's server needs porting, not pasting.
- scry's admin is voodoo's `is_admin`, though voodoo's README says `is_admin` is not an app role. Fine while one person runs everything; per-app roles would need a table.
- Copies made from the template drift apart.
- `go.mod` replaces voodoo with `../voodoo` until voodoo tags a version: builds use whatever voodoo is checked out (CI: voodoo's `main`), so a scry build is not pinned to a voodoo commit.
- `voodooauth` calls voodoo's `/me` once per request carrying an unknown or garbage credential (only positive answers are cached; no format check or request coalescing). One call per request, not amplification; revisit if voodoo sees abuse.
- `backup.sh` does not read `/etc/scry/scry.env`. If `SCRY_DB_PATH` there differs from the default, backups print "nothing to back up" and exit 0 without the healthcheck ping (same as voodoo). Keep the default path.
- Images load directly from other sites, which see readers' IP addresses.
- Feeds whose guids change on every fetch produce duplicates.
- Inngest sync is manual from the dashboard: inngestgo v0.16+ refuses unsigned `PUT /api/inngest` unless `INNGEST_ENABLE_UNAUTHED_SYNC` is set. Enabling it would also need `ServeOrigin` pinned to `SCRY_BASE_URL`, or the synced URL comes from the Host header.
- Production has no off-box backups yet (`/etc/scry/backup.env` absent); deploys take a local copy only.
- A failure can be counted twice: overlapping syncs before step 4's per-feed concurrency, or a step retried after its write committed.
- Feed URL dedup folds only scheme and host case; `:443`, a trailing dot, `#fragment`, or IDN vs punycode variants store the same feed twice.

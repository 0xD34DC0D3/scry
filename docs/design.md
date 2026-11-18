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
- `SCRY_SITE_URL` is the org website the header tile and footer link to (the design guide: hosts come from env, never code). Same as voodoo's `VOODOO_SITE_URL`: default `http://localhost:4321`, trailing slash trimmed, https required in prod (`CheckServe`, so prod refuses the default). It reaches the layout through `server.New` and `web.New`, no package global. The guide names voodoo's URL `<APP>_AUTH_URL`; scry keeps `SCRY_VOODOO_URL` (see Known issues).
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
- Lists use an item's posted time: `min(coalesce(published_at, first_seen_at), first_seen_at)` (see Public views). Views show the last 30 days of it. The unread rule still uses `first_seen_at`.
- `PRAGMA foreign_keys=ON` on every connection; deletes cascade.
- `disabled_at` is the one "not fetched" flag: an admin pause and the auto-disable after 10 failures both set it, and the catalog shows either as paused. Resume clears it, resets `fail_count` and `last_error`, and sets `next_fetch_at` to 0 so the feed is due at once (a new feed starts the same way).
- Unknown text is `''`, not NULL; `disabled_at` and `published_at` are nullable. Indexes beyond keys: `items(posted)` and `items(feed_id, posted)` (expression indexes, migration `00006`, which also dropped `items(first_seen_at)`), `items(feed_id, first_seen_at)`, and `subscriptions(feed_id)`, `item_reads(item_id)` for the cascades.
- Migration `00001` (the template's notes table) stays because production applied it; `00002` drops the table and `00003` creates the schema.
- Feed URLs are trimmed, must be absolute http(s) with a host and no userinfo, at most 2048 bytes after encoding, and are stored with scheme and host lowercased. Adding does not fetch; private addresses are refused by the fetcher (step 3), not at add time.

## Sync

- `syncFeed {feed_id}` does everything for one feed. A cron function sends it for each due feed and moves that feed's `next_fetch_at` forward; admins can send it too.
- Concurrency: 1 per feed, 5 overall (the Inngest plan's limit; first planned as about 10).
- Conditional GET, 15 s timeout, 10 MB cap, User-Agent, private addresses refused (redirects included). Fetch and parse outside the transaction, then one short write. Steps return small values (ids, counts), never feed bodies: each Inngest call must finish inside `serve`'s 30 s write timeout and arrive inside its 10 s read timeout.
- Upsert items by `dedup_key`: guid, else link, else hash of title and date. Content is rewritten on every fetch.
- Success, including 304, resets `fail_count`. A failure counts up and backs `next_fetch_at` off, up to 24 h; after 10 the feed is disabled and shown to admins. Inngest retries cover only blips within one run.
- HTML is sanitized on the way in (bluemonday `UGCPolicy`); the excerpt is plain text.
- Pruning runs daily in batches (see Step 7 decisions).
- Step 3 decisions: event `scry/feed.sync`; fetch, parse and write run in one step returning a status and item count; a 304 takes the same write with no items. Fetch or parse failures are recorded and end the run normally (no Inngest retry); only database errors fail the step. Both feed writes skip a paused feed, so a pause during the fetch wins. Backoff is computed in the failure UPDATE from the old `fail_count`: 30 min doubling, capped at 24 h; a success sets `next_fetch_at` to now + 1 h. The fetcher checks the dialled IP (covers redirects and DNS rebinding), ignores proxy env vars, allows only http(s) and 5 redirects. Excerpts are 300 runes, taken from the same HTML as the content (content preferred over description). `last_error` keeps 500 bytes.
- Step 4 decisions: cron `schedule-feeds` runs every 10 min and claims at most 100 due feeds (`next_fetch_at <= now`, not disabled, oldest first). A feed is at most 10 min late, an idle tick is one cheap step, and 100 syncs drain in at most 5 min at 5 concurrent 15 s fetches; any excess waits a tick. The claim is one `UPDATE ... WHERE id IN (SELECT ... LIMIT) RETURNING id` in its own step that moves `next_fetch_at` to now + 1 h and returns the ids; a second step sends the events in one `step.SendMany`. A claim retried after its UPDATE committed finds nothing, so it never double-sends; those events are lost and the feeds come due again in 1 h (no outbox). Event IDs are `scry-feed-sync-<feed id>-<claimed next_fetch_at>`, so a retried send is deduplicated by Inngest and the next claim gets a new ID. `sync-feed` has step concurrency 1 keyed on `event.data.feed_id` plus 5 overall, the Inngest plan's limit (Inngest refused the first sync with 10; it is one step, so that covers the run); extra runs queue. `FeedFailed` sets `disabled_at` in the same UPDATE when the new `fail_count` reaches 10. The admin catalog marks an active feed with failures as "failing"; auto-disabled shows as paused with its `fail_count` and `last_error`, and Resume re-enables it. No index on `next_fetch_at`: the feeds table is tiny.
- Steps 4 and 5 were built in parallel (one git worktree each) at the owner's request, departing from AGENTS.md's milestone order. Step 4 merged first; step 5 was rebased onto it.

## Public views

- Step 5 decisions: routes `/` (latest across feeds), `/feeds` (catalog), `/feeds/{id}` (one feed); signed-out access, no login redirect. Excerpts only, escaped text; item and site URLs go through `html/template`'s URL escaping, so `javascript:` becomes `#ZgotmplZ`.
- Cache: a request with no voodoo cookie and no `Authorization` header gets `public, max-age=60`: syncs are hourly at most, so a minute is never meaningfully stale, and it absorbs reloads. A request carrying either gets `private, no-store`, even if voodoo rejected it or was down, since the page may name the user. Every response sets `Vary: HX-Request, Cookie, Authorization`, so a stored anonymous page or fragment is never served to a full-page load or to a browser that just signed in. Errors are `no-store`: error pages name the signed-in user and do not Vary on Cookie.
- Lists: 50 items per page, newest first by `(posted, id)`, with an Older link (`?before=<posted>-<id>`, a keyset cursor on the posted indexes). Without JS it is a plain link to the next page; with htmx it swaps in the next page's items in place, without pushing the URL. A malformed cursor is 400; an unknown feed 404.
- Posted time (changed after step 7, out of milestone order, at the owner's request; departs from the original "lists use `first_seen_at`" rule): lists order, window (30 days) and page by `min(coalesce(published_at, first_seen_at), first_seen_at)`, and the catalog's 30-day count uses the same. With `first_seen_at` alone, adding a feed put its whole backlog on top stamped "now", and a feed resurfacing an old post made it look new. Clamping to `first_seen_at` means a future or bogus date cannot pin an item above newer ones, and an item never appears to be posted after scry saw it. A post published more than 30 days ago is never listed, even when first seen today. Index: expression indexes on `CAST(<that> AS INTEGER)` and `(feed_id, CAST(<that> AS INTEGER))` (migration `00006`), not a stored column, so the rule lives in one place and old rows need no backfill. Queries must spell the expression exactly as the index does and CAST their parameters to INTEGER too: a CAST on one side only (or none on the index, with a CAST parameter, which sqlc needs for an int64 type) makes SQLite convert affinity and scan the whole index. `internal/db/plan_test.go` checks the plans. `items(first_seen_at)` was dropped as unused; `items(feed_id, first_seen_at)` stays for the unread rule. A feed's last post (`GetFeedLastPostAt`, `ListCatalog.last_post_at`) is the max of the same expression, so `items(feed_id, posted)` answers it with one seek to the feed's last index entry instead of aggregating every item the feed ever had.
- The catalog lists every feed, paused or auto-disabled ones marked paused, with its item count for the last 30 days; it is not paged (admins curate it, so it stays small). Paused feeds' items still appear in the lists until they age out. Failure details stay on the admin page.
- UX pass: the header nav has `aria-label="Main"` and marks the current link `aria-current="page"` (`"true"` on one feed, below `/feeds`). Item titles are h2 (no h1 to h3 skip), underlined only on hover/focus, muted once visited. Lists end with "— end of 30 days —" when there is no Older page, even in an empty Older fragment (items aged out between clicks). The Older link dims while loading (`.htmx-request`, no clicks) and the swapped-in fragment's first item has `autofocus`, which htmx focuses, so keyboard focus is not lost; full pages never autofocus. Empty states link on (`/feeds`, `/`, or `/admin/feeds` for admins). Admins see "Manage feeds →" on `/feeds`; cache-safe because an admin request carries a credential, so `setCache` makes it `private, no-store`, and Vary covers Cookie and Authorization. 404, 400, 413 and 500 render through the layout (`web.Renderer.Error`) as `no-store`; a catch-all `/` pattern covers unknown paths, at the cost of 405s becoming 404s (a `GET /` catch-all would conflict with the method-less `/api/inngest` pattern). Admin: h1 "Manage feeds", `main.wide`, buttons disabled in flight (`hx-disabled-elt="find button"` on each form, since the form is the htmx element), long URLs wrap, `last_error` collapsed in a `<details>`. Under 600px the header is not sticky and the item meta line's links (feed, discussion) get a taller tap target.

Feed display (added between steps 5 and 6, out of milestone order, at the owner's request). Every rule is generic, not Hacker News-specific:

- The item meta line shows the item link's domain (`url.Parse(...).Hostname()`, leading `www.` stripped; empty when unparsable or hostless), computed at render time, so changing the rule needs no migration. It also shows `by <author>` when `items.author` is set, and a "discussion" link when `comments_url` is.
- Link-only summaries are dropped at parse time: link-only content (e.g. "Continue reading") falls back to the description, and when that too has no text outside `<a>` elements (whitespace and `&nbsp;` only), both `item_contents.html` and the excerpt are stored as `''`. Covers HN's lone "Comments", lobste.rs link posts, and "Read more" stubs; a lobste.rs text post keeps its text and link. Exception: HTML with an `<img>` is kept (an image-only comic is not empty). Items already stored keep their old excerpt until their next fetch rewrites it.
- `items.comments_url` (migration `00004`, `TEXT NOT NULL DEFAULT ''`) holds RSS `<comments>`, rewritten on every fetch like the other item fields and read by both public list queries. gofeed v1.5.0 parses `<comments>` only onto `rss.Item`, so `parse.go` wraps `DefaultRSSTranslator`: it runs the default, then copies each item's `Comments` into the universal `Item.Custom` under `:comments` (a leading colon cannot be an XML element name, so no unknown element gofeed keeps there collides). Only absolute http(s) URLs with a host, at most 2048 bytes, are kept; anything else stores `''`. The template puts it in `href` as a plain string (never `template.URL`), so even a bad stored value renders as `#ZgotmplZ`. Atom has no equivalent.
- Step 7 decisions: cron `prune` runs daily at 03:17 UTC. Step `cutoff` records now - 30 days once (Inngest replays the function body per step, so a clock read in the body would drift), then steps `prune-contents-N` and `prune-reads-N` each delete one batch of 500 (`DELETE ... WHERE rowid IN (SELECT rowid ... ORDER BY <time> LIMIT 500)`) and return the count; the loop stops at the first short batch. 500 is a short write on the single Writer (tens of ms even with large html), so syncs wait little and each step is far inside the 30 s write timeout, and it keeps step outputs tiny. Loop of steps, capped at 100 per table (about 201 steps, well under ~1000): a backlog over 50,000 rows per table finishes on the next days' runs, and steady state is a few batches; one looping step was rejected because a crash would lose its progress and its length is bounded only by timing. A retried step after a committed delete just deletes the next batch. Migration `00005` adds `item_contents(fetched_at)` and `item_reads(read_at)`, or every batch would rescan the table. The boundary agrees with the views: both use now - 30 days, views keep posted time `>=` it (posted is never after `first_seen_at`, so that implies `first_seen_at >=` it) and the prune deletes rows strictly older. Why that is safe: `fetched_at` is rewritten on every fetch and is never before `first_seen_at`, so a content row older than the cutoff belongs to an item first seen before it, which the views no longer show (still-fed items keep fresh content). An `item_reads` row is written when a reader marks an item read, after the item was first seen, so `read_at >= first_seen_at`; a row older than the cutoff is for an item outside the window. The unread rule only counts items inside the window (`first_seen_at` after `subscribed_at` and `read_before_at`, no read row), so a pruned read row never turns a visible item unread. Public lists already `LEFT JOIN item_contents` with `coalesce(excerpt, '')`, so an item without content is listed with an empty excerpt (test `TestPublicItemWithoutContent`). Bookmarks (deferred) will need to exempt their items' contents from the prune.

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
- A failure can be counted twice when Inngest retries the sync step after its write committed (the response was lost); ten such doubles disable a feed early. Resume fixes it.
- Feed URL dedup folds only scheme and host case; `:443`, a trailing dot, `#fragment`, or IDN vs punycode variants store the same feed twice.
- Catalog feed URLs and all their items are public, so a feed whose URL carries a secret token (a private feed) leaks it. Don't add such feeds.
- Excerpts put a space wherever an inline tag was stripped, so `about <em>feeds</em>.` reads "about feeds ." (bluemonday `AddSpaceWhenStrippingTag` cannot tell inline from block tags).
- Hacker News items have no guid, so a repost of the same URL folds into one item (dedup_key falls back to the link).
- The design guide names voodoo's URL `<APP>_AUTH_URL`; scry (and voodoo's integration docs) use `SCRY_VOODOO_URL`. Rename or update the guide.
- Error pages from voodooauth's `RequireUser` / `RequireAdmin` (401/403/503) are still voodoo's plain responses, not the layout.
- `web.StaticHandler` serves a directory listing at `/static/`, and a missing static file is a text/plain 404 cached `public, max-age=3600`.
- Admin pages set no Cache-Control. They need auth and send no validators, so nothing caches them in practice.

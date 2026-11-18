-- Feed catalog (admin).

-- name: ListFeeds :many
SELECT * FROM feeds ORDER BY id;

-- name: CreateFeed :one
INSERT INTO feeds (url) VALUES (?) ON CONFLICT (url) DO NOTHING RETURNING *;

-- Pausing an already paused or auto-disabled feed keeps its original time.
-- name: PauseFeed :execrows
UPDATE feeds SET disabled_at = coalesce(disabled_at, unixepoch()) WHERE id = ?;

-- Resume also clears the failure state and makes the feed due at once.
-- name: ResumeFeed :execrows
UPDATE feeds
SET disabled_at = NULL, fail_count = 0, last_error = '', next_fetch_at = 0
WHERE id = ?;

-- name: DeleteFeed :execrows
DELETE FROM feeds WHERE id = ?;

-- name: GetFeed :one
SELECT * FROM feeds WHERE id = ?;

-- Sync (internal/jobs). Both feed updates skip a paused feed, so a pause
-- that lands during the fetch wins; zero rows means paused or removed. The
-- feed update runs first in the write, before any item.

-- A fetch that gave no title or site link (or a 304) keeps the stored ones.
-- name: FeedSynced :execrows
UPDATE feeds
SET title = coalesce(nullif(CAST(sqlc.arg(title) AS TEXT), ''), title),
    site_url = coalesce(nullif(CAST(sqlc.arg(site_url) AS TEXT), ''), site_url),
    etag = sqlc.arg(etag),
    last_modified = sqlc.arg(last_modified),
    next_fetch_at = sqlc.arg(next_fetch_at),
    fail_count = 0,
    last_error = ''
WHERE id = sqlc.arg(id) AND disabled_at IS NULL;

-- Backoff from the old fail_count, in one statement so overlapping syncs
-- cannot both read the same count: 30 min doubling, capped at 24 h. The
-- 10th consecutive failure also disables the feed (shown as paused).
-- name: FeedFailed :execrows
UPDATE feeds
SET fail_count = fail_count + 1,
    last_error = sqlc.arg(last_error),
    next_fetch_at = CAST(sqlc.arg(now) AS INTEGER) + min(1800 << min(fail_count, 6), 86400),
    disabled_at = CASE WHEN fail_count + 1 >= 10 THEN CAST(sqlc.arg(now) AS INTEGER) END
WHERE id = sqlc.arg(id) AND disabled_at IS NULL;

-- Schedule (internal/jobs). Claims up to lim due feeds in one statement:
-- moving next_fetch_at to until takes them out of the next claim, so a
-- retried or overlapping claim never returns the same feed twice.
-- name: ClaimDueFeeds :many
UPDATE feeds
SET next_fetch_at = sqlc.arg(until)
WHERE id IN (
    SELECT due.id FROM feeds AS due
    WHERE due.next_fetch_at <= CAST(sqlc.arg(now) AS INTEGER) AND due.disabled_at IS NULL
    ORDER BY due.next_fetch_at, due.id
    LIMIT sqlc.arg(lim)
)
RETURNING id;

-- An existing item keeps its id and first_seen_at, so it never shows as new
-- again; the rest follows the feed's latest copy.
-- name: UpsertItem :one
INSERT INTO items (feed_id, dedup_key, url, title, author, published_at, first_seen_at)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (feed_id, dedup_key) DO UPDATE
SET url = excluded.url,
    title = excluded.title,
    author = excluded.author,
    published_at = excluded.published_at
RETURNING id;

-- name: UpsertItemContent :exec
INSERT INTO item_contents (item_id, html, excerpt, fetched_at)
VALUES (?, ?, ?, ?)
ON CONFLICT (item_id) DO UPDATE
SET html = excluded.html, excerpt = excluded.excerpt, fetched_at = excluded.fetched_at;

-- Public views (internal/server/public.go). Lists cover first_seen_at >= since
-- (30 days back), newest first. Paging is a keyset cursor on
-- (first_seen_at, id): the first page passes the max int64 for both. The
-- upper bound on first_seen_at keeps the index range tight; the OR only
-- breaks ties. id is the rowid, so both indexes already end in it.

-- name: ListLatestItems :many
SELECT i.id, i.feed_id, i.url, i.title, i.first_seen_at,
       CAST(coalesce(c.excerpt, '') AS TEXT) AS excerpt,
       f.title AS feed_title, f.url AS feed_url
FROM items i
JOIN feeds f ON f.id = i.feed_id
LEFT JOIN item_contents c ON c.item_id = i.id
WHERE i.first_seen_at >= sqlc.arg(since)
  AND i.first_seen_at <= sqlc.arg(before_at)
  AND (i.first_seen_at < sqlc.arg(before_at) OR i.id < sqlc.arg(before_id))
ORDER BY i.first_seen_at DESC, i.id DESC
LIMIT sqlc.arg(lim);

-- name: ListFeedItems :many
SELECT i.id, i.feed_id, i.url, i.title, i.first_seen_at,
       CAST(coalesce(c.excerpt, '') AS TEXT) AS excerpt
FROM items i
LEFT JOIN item_contents c ON c.item_id = i.id
WHERE i.feed_id = sqlc.arg(feed_id)
  AND i.first_seen_at >= sqlc.arg(since)
  AND i.first_seen_at <= sqlc.arg(before_at)
  AND (i.first_seen_at < sqlc.arg(before_at) OR i.id < sqlc.arg(before_id))
ORDER BY i.first_seen_at DESC, i.id DESC
LIMIT sqlc.arg(lim);

-- Every feed, paused ones included (shown as paused), untitled ones last.
-- name: ListCatalog :many
SELECT f.id, f.url, f.title, f.site_url, f.disabled_at,
       (SELECT count(*) FROM items i
        WHERE i.feed_id = f.id AND i.first_seen_at >= sqlc.arg(since)) AS recent_items
FROM feeds f
ORDER BY f.title = '', f.title COLLATE NOCASE, f.url;

-- name: GetCatalogFeed :one
SELECT id, url, title, site_url, disabled_at FROM feeds WHERE id = ?;

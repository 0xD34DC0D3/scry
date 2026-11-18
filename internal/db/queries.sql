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
-- cannot both read the same count: 30 min doubling, capped at 24 h.
-- name: FeedFailed :execrows
UPDATE feeds
SET fail_count = fail_count + 1,
    last_error = sqlc.arg(last_error),
    next_fetch_at = CAST(sqlc.arg(now) AS INTEGER) + min(1800 << min(fail_count, 6), 86400)
WHERE id = sqlc.arg(id) AND disabled_at IS NULL;

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

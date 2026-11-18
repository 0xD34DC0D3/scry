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

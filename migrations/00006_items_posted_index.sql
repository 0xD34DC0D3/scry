-- +goose Up
-- Public lists sort, window and page by an item's "posted at" time: its
-- published date, never later than when scry first saw it (a future or bogus
-- date cannot pin an item to the top), or first_seen_at when the feed gave
-- none. An expression index, not a stored column, so the rule lives in one
-- place and cannot drift from the data. Queries must spell the expression
-- exactly like this, or SQLite will not use the index. The CAST gives it
-- INTEGER affinity, matching the CAST(sqlc.arg(...) AS INTEGER) parameters
-- in queries.sql, so comparisons need no conversion and use the index range.
CREATE INDEX items_posted ON items (CAST(min(coalesce(published_at, first_seen_at), first_seen_at) AS INTEGER));
CREATE INDEX items_feed_posted ON items (feed_id, CAST(min(coalesce(published_at, first_seen_at), first_seen_at) AS INTEGER));
-- Nothing queries first_seen_at across all feeds any more; the per-feed
-- index stays for the unread rule (step 6), which compares first_seen_at.
DROP INDEX items_first_seen;

-- +goose Down
CREATE INDEX items_first_seen ON items (first_seen_at);
DROP INDEX items_feed_posted;
DROP INDEX items_posted;

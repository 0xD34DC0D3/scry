-- +goose Up
-- Times are unix seconds. Empty string, not NULL, means "unknown" for text;
-- NULL is kept for times whose absence means something (disabled_at: active,
-- published_at: the feed gave no date).

-- disabled_at is set by an admin pause and by the auto-disable after 10
-- failures. next_fetch_at defaults to 0 so a new feed is due at once.
CREATE TABLE feeds (
    id            INTEGER PRIMARY KEY,
    url           TEXT NOT NULL UNIQUE,
    title         TEXT NOT NULL DEFAULT '',
    site_url      TEXT NOT NULL DEFAULT '',
    etag          TEXT NOT NULL DEFAULT '',
    last_modified TEXT NOT NULL DEFAULT '',
    next_fetch_at INTEGER NOT NULL DEFAULT 0,
    fail_count    INTEGER NOT NULL DEFAULT 0,
    last_error    TEXT NOT NULL DEFAULT '',
    disabled_at   INTEGER
);

CREATE TABLE items (
    id            INTEGER PRIMARY KEY,
    feed_id       INTEGER NOT NULL REFERENCES feeds (id) ON DELETE CASCADE,
    dedup_key     TEXT NOT NULL,
    url           TEXT NOT NULL DEFAULT '',
    title         TEXT NOT NULL DEFAULT '',
    author        TEXT NOT NULL DEFAULT '',
    published_at  INTEGER,
    first_seen_at INTEGER NOT NULL DEFAULT (unixepoch()),
    UNIQUE (feed_id, dedup_key)
);
-- Lists sort by first_seen_at: across all feeds, and within one feed.
CREATE INDEX items_first_seen ON items (first_seen_at);
CREATE INDEX items_feed_first_seen ON items (feed_id, first_seen_at);

CREATE TABLE item_contents (
    item_id    INTEGER PRIMARY KEY REFERENCES items (id) ON DELETE CASCADE,
    html       TEXT NOT NULL,
    excerpt    TEXT NOT NULL,
    fetched_at INTEGER NOT NULL DEFAULT (unixepoch())
);

-- user_id is voodoo's user id; scry has no users table.
CREATE TABLE subscriptions (
    user_id        TEXT NOT NULL,
    feed_id        INTEGER NOT NULL REFERENCES feeds (id) ON DELETE CASCADE,
    subscribed_at  INTEGER NOT NULL DEFAULT (unixepoch()),
    read_before_at INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (user_id, feed_id)
);

CREATE TABLE item_reads (
    user_id TEXT NOT NULL,
    item_id INTEGER NOT NULL REFERENCES items (id) ON DELETE CASCADE,
    read_at INTEGER NOT NULL DEFAULT (unixepoch()),
    PRIMARY KEY (user_id, item_id)
);

-- The primary keys lead with user_id, so cascades from a deleted feed or
-- item would scan these tables without an index on the parent column.
CREATE INDEX subscriptions_feed ON subscriptions (feed_id);
CREATE INDEX item_reads_item ON item_reads (item_id);

-- +goose Down
DROP TABLE item_reads;
DROP TABLE subscriptions;
DROP TABLE item_contents;
DROP TABLE items;
DROP TABLE feeds;

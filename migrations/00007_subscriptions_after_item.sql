-- +goose Up
-- after_item_id is the highest items.id when the user subscribed; only items
-- with a larger id can be unread. An id watermark, not subscribed_at, because
-- first_seen_at is the sync's start time: a sync that started before the
-- subscription and committed after it would otherwise be lost to the reader.
-- Item ids grow in commit order (one writer, rowid = max + 1).
ALTER TABLE subscriptions ADD COLUMN after_item_id INTEGER NOT NULL DEFAULT 0;

-- Rows from before this migration keep their old meaning as closely as the
-- data allows: everything first seen by the time they subscribed is old.
UPDATE subscriptions
SET after_item_id = coalesce(
    (SELECT max(i.id) FROM items i WHERE i.first_seen_at <= subscriptions.subscribed_at), 0);

-- +goose Down
ALTER TABLE subscriptions DROP COLUMN after_item_id;

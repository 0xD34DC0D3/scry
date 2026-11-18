-- +goose Up
-- 00001 created the template's example notes table. Production already
-- applied it, so it stays in history and this migration removes the table.
DROP TABLE notes;

-- +goose Down
CREATE TABLE notes (
    id         INTEGER PRIMARY KEY,
    body       TEXT NOT NULL,
    created_at INTEGER NOT NULL DEFAULT (unixepoch())
);

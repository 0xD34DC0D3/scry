-- +goose Up
-- EXAMPLE migration for the template. Step 2 deletes it (with its queries)
-- and replaces it with the real schema; nothing should depend on it.
CREATE TABLE notes (
    id         INTEGER PRIMARY KEY,
    body       TEXT NOT NULL,
    created_at INTEGER NOT NULL DEFAULT (unixepoch())
);

-- +goose Down
DROP TABLE notes;

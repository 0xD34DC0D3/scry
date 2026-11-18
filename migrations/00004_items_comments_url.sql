-- +goose Up
-- An item's discussion page (RSS <comments>), e.g. a Hacker News thread.
-- '' when the feed gives none or an unusable one; only absolute http(s)
-- URLs are kept (internal/jobs/parse.go).
ALTER TABLE items ADD COLUMN comments_url TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE items DROP COLUMN comments_url;

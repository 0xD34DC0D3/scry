-- +goose Up
-- The daily prune (internal/jobs/prune.go) deletes in batches by these
-- columns; without an index each batch would rescan the whole table.
CREATE INDEX item_contents_fetched ON item_contents (fetched_at);
CREATE INDEX item_reads_read ON item_reads (read_at);

-- +goose Down
DROP INDEX item_reads_read;
DROP INDEX item_contents_fetched;

-- +goose Up
ALTER TABLE site_rss_items
    ADD COLUMN IF NOT EXISTS download_secret TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE site_rss_items
    DROP COLUMN IF EXISTS download_secret;


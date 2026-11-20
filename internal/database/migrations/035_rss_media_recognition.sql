-- +goose Up
ALTER TABLE site_rss_items
    ADD COLUMN IF NOT EXISTS recognized_media_title TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS media_recognize_fail_count INTEGER NOT NULL DEFAULT 1
        CHECK (media_recognize_fail_count BETWEEN 0 AND 3);

-- +goose Down
ALTER TABLE site_rss_items
    DROP COLUMN IF EXISTS media_recognize_fail_count,
    DROP COLUMN IF EXISTS recognized_media_title;

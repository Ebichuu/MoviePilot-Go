-- +goose Up
CREATE TABLE download_failures (
    id BIGSERIAL PRIMARY KEY,
    fingerprint CHAR(64) NOT NULL CHECK (fingerprint ~ '^[0-9a-f]{64}$'),
    media_type TEXT,
    title TEXT,
    year TEXT,
    media_source TEXT,
    media_id TEXT,
    seasons TEXT,
    episodes TEXT,
    site INTEGER,
    site_name TEXT,
    torrent_id TEXT,
    torrent_name TEXT,
    torrent_size BIGINT,
    downloader TEXT,
    source VARCHAR(1000),
    error_message VARCHAR(1000),
    retry_count INTEGER NOT NULL DEFAULT 1 CHECK (retry_count > 0),
    first_failed_at TIMESTAMPTZ NOT NULL,
    last_failed_at TIMESTAMPTZ NOT NULL,
    next_retry_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT ux_download_failures_fingerprint UNIQUE (fingerprint)
);

CREATE INDEX ix_download_failures_next_retry_at
    ON download_failures (next_retry_at);

CREATE INDEX ix_download_failures_media_identity_site
    ON download_failures (media_type, media_source, media_id, site);

-- +goose Down
DROP TABLE IF EXISTS download_failures;

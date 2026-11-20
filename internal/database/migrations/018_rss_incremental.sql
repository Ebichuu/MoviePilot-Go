-- +goose Up
CREATE TABLE IF NOT EXISTS site_rss_cursors (
    site_id BIGINT PRIMARY KEY REFERENCES sites(id) ON DELETE CASCADE,
    endpoint TEXT NOT NULL,
    etag TEXT NOT NULL DEFAULT '',
    last_modified TEXT NOT NULL DEFAULT '',
    high_water_published_at TIMESTAMPTZ,
    high_water_item_key TEXT NOT NULL DEFAULT '',
    next_poll_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_attempt_at TIMESTAMPTZ,
    last_success_at TIMESTAMPTZ,
    failure_count INT NOT NULL DEFAULT 0 CHECK (failure_count >= 0),
    last_error TEXT NOT NULL DEFAULT '',
    lease_token UUID,
    lease_expires_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS site_rss_cursors_due_idx
    ON site_rss_cursors (next_poll_at, site_id);

CREATE TABLE IF NOT EXISTS site_rss_items (
    id BIGSERIAL PRIMARY KEY,
    site_id BIGINT NOT NULL REFERENCES sites(id) ON DELETE CASCADE,
    item_key TEXT NOT NULL,
    guid TEXT NOT NULL DEFAULT '',
    download_id TEXT NOT NULL DEFAULT '',
    detail_url TEXT NOT NULL DEFAULT '',
    download_url TEXT NOT NULL DEFAULT '',
    title TEXT NOT NULL,
    published_at TIMESTAMPTZ,
    resource JSONB NOT NULL DEFAULT '{}'::jsonb,
    first_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    state TEXT NOT NULL DEFAULT 'pending',
    attempt_count INT NOT NULL DEFAULT 0 CHECK (attempt_count >= 0),
    last_error TEXT NOT NULL DEFAULT '',
    matched_at TIMESTAMPTZ,
    lease_token UUID,
    lease_expires_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT site_rss_items_state_check
        CHECK (state IN ('pending', 'processing', 'matched', 'ignored', 'failed')),
    CONSTRAINT site_rss_items_key_check
        CHECK (length(item_key) BETWEEN 8 AND 256),
    CONSTRAINT site_rss_items_resource_size_check
        CHECK (pg_column_size(resource) <= 1048576),
    UNIQUE (site_id, item_key)
);

CREATE INDEX IF NOT EXISTS site_rss_items_pending_idx
    ON site_rss_items (state, first_seen_at, id);

CREATE INDEX IF NOT EXISTS site_rss_items_site_state_idx
    ON site_rss_items (site_id, state, first_seen_at, id);

-- +goose Down
DROP TABLE IF EXISTS site_rss_items;
DROP TABLE IF EXISTS site_rss_cursors;

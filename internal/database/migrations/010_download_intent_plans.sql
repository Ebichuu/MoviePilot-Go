-- +goose Up
CREATE TABLE IF NOT EXISTS download_intent_plans (
    intent_id UUID PRIMARY KEY REFERENCES download_intents(id) ON DELETE CASCADE,
    site_id BIGINT NOT NULL REFERENCES sites(id) ON DELETE RESTRICT,
    downloader_id TEXT NOT NULL,
    resolver_kind TEXT NOT NULL,
    resolver_version SMALLINT NOT NULL,
    resource_id TEXT NOT NULL,
    resource_secret TEXT NOT NULL DEFAULT '',
    save_path TEXT NOT NULL DEFAULT '',
    paused BOOLEAN NOT NULL DEFAULT FALSE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT download_intent_plans_downloader_id_check CHECK (
        downloader_id ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
    ),
    CONSTRAINT download_intent_plans_resolver_kind_check CHECK (
        char_length(resolver_kind) BETWEEN 1 AND 64
        AND resolver_kind ~ '^[a-z][a-z0-9_-]*$'
    ),
    CONSTRAINT download_intent_plans_resolver_version_check CHECK (
        resolver_version > 0
    ),
    CONSTRAINT download_intent_plans_resource_id_check CHECK (
        char_length(resource_id) BETWEEN 1 AND 255
        AND resource_id ~ '^(sha256:[0-9a-f]{64}|[A-Za-z0-9][A-Za-z0-9._-]*)$'
    ),
    CONSTRAINT download_intent_plans_resource_secret_check CHECK (
        resource_secret = '' OR resource_secret LIKE 'v1:%'
    ),
    CONSTRAINT download_intent_plans_save_path_check CHECK (
        octet_length(save_path) <= 4096
    )
);

-- +goose Down
DROP TABLE IF EXISTS download_intent_plans;

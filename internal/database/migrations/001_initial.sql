-- +goose Up
CREATE TABLE IF NOT EXISTS users (
    id BIGSERIAL PRIMARY KEY,
    username TEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,
    is_superuser BOOLEAN NOT NULL DEFAULT FALSE,
    avatar TEXT,
    permissions JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS settings (
    key TEXT PRIMARY KEY,
    value JSONB NOT NULL DEFAULT '{}'::jsonb,
    encrypted BOOLEAN NOT NULL DEFAULT FALSE,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS sites (
    id BIGSERIAL PRIMARY KEY,
    name TEXT NOT NULL,
    url TEXT NOT NULL,
    adapter TEXT NOT NULL,
    enabled BOOLEAN NOT NULL DEFAULT TRUE,
    config JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (name)
);

CREATE TABLE IF NOT EXISTS subscriptions (
    id BIGSERIAL PRIMARY KEY,
    username TEXT NOT NULL REFERENCES users(username),
    media_type TEXT NOT NULL,
    title TEXT NOT NULL,
    media_source TEXT,
    media_id TEXT,
    year INT,
    season INT,
    internal_state TEXT NOT NULL DEFAULT 'pending_identification',
    v2_state CHAR(1) NOT NULL DEFAULT 'P',
    config JSONB NOT NULL DEFAULT '{}'::jsonb,
    completed_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS subscriptions_user_state_idx ON subscriptions (username, internal_state);
CREATE UNIQUE INDEX IF NOT EXISTS subscriptions_identity_idx
    ON subscriptions (username, media_type, COALESCE(media_source, ''), COALESCE(media_id, ''), COALESCE(season, -1));

CREATE TABLE IF NOT EXISTS subscription_episodes (
    id BIGSERIAL PRIMARY KEY,
    subscription_id BIGINT NOT NULL REFERENCES subscriptions(id) ON DELETE CASCADE,
    season INT NOT NULL,
    episode INT NOT NULL,
    state TEXT NOT NULL DEFAULT 'missing',
    download_intent_id UUID,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (subscription_id, season, episode)
);

CREATE TABLE IF NOT EXISTS download_intents (
    id UUID PRIMARY KEY,
    username TEXT NOT NULL REFERENCES users(username),
    subscription_id BIGINT REFERENCES subscriptions(id) ON DELETE SET NULL,
    candidate_key TEXT NOT NULL,
    torrent_hash TEXT,
    downloader TEXT NOT NULL DEFAULT 'qbittorrent',
    label TEXT NOT NULL,
    state TEXT NOT NULL DEFAULT 'planned',
    payload JSONB NOT NULL DEFAULT '{}'::jsonb,
    last_error TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (username, candidate_key)
);
CREATE INDEX IF NOT EXISTS download_intents_state_idx ON download_intents (state, updated_at);

CREATE TABLE IF NOT EXISTS download_files (
    id BIGSERIAL PRIMARY KEY,
    intent_id UUID NOT NULL REFERENCES download_intents(id) ON DELETE CASCADE,
    path TEXT NOT NULL,
    size_bytes BIGINT,
    selected BOOLEAN NOT NULL DEFAULT TRUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS organization_batches (
    id UUID PRIMARY KEY,
    intent_id UUID REFERENCES download_intents(id) ON DELETE SET NULL,
    state TEXT NOT NULL DEFAULT 'planned',
    source_root TEXT NOT NULL,
    target_root TEXT NOT NULL,
    plan JSONB NOT NULL DEFAULT '[]'::jsonb,
    last_error TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS organization_files (
    id BIGSERIAL PRIMARY KEY,
    batch_id UUID NOT NULL REFERENCES organization_batches(id) ON DELETE CASCADE,
    source_path TEXT NOT NULL,
    target_path TEXT NOT NULL,
    state TEXT NOT NULL DEFAULT 'planned',
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (batch_id, target_path)
);

CREATE TABLE IF NOT EXISTS emby_indexes (
    id BIGSERIAL PRIMARY KEY,
    server_name TEXT NOT NULL,
    external_id TEXT NOT NULL,
    path TEXT,
    media_type TEXT,
    indexed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (server_name, external_id)
);

CREATE TABLE IF NOT EXISTS notifications (
    id UUID PRIMARY KEY,
    username TEXT NOT NULL REFERENCES users(username),
    channel TEXT NOT NULL,
    idempotency_key TEXT NOT NULL,
    payload JSONB NOT NULL DEFAULT '{}'::jsonb,
    state TEXT NOT NULL DEFAULT 'pending',
    last_error TEXT,
    sent_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (channel, idempotency_key)
);

-- +goose Down
DROP TABLE IF EXISTS notifications;
DROP TABLE IF EXISTS emby_indexes;
DROP TABLE IF EXISTS organization_files;
DROP TABLE IF EXISTS organization_batches;
DROP TABLE IF EXISTS download_files;
DROP TABLE IF EXISTS download_intents;
DROP TABLE IF EXISTS subscription_episodes;
DROP TABLE IF EXISTS subscriptions;
DROP TABLE IF EXISTS sites;
DROP TABLE IF EXISTS settings;
DROP TABLE IF EXISTS users;

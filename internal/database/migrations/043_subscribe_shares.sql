-- +goose Up

CREATE TABLE subscribe_shares (
    id BIGSERIAL PRIMARY KEY,
    share_key TEXT NOT NULL UNIQUE,
    username TEXT NOT NULL REFERENCES users(username) ON DELETE CASCADE,
    title TEXT NOT NULL,
    media_type TEXT NOT NULL DEFAULT '',
    "year" TEXT NOT NULL DEFAULT '',
    season INTEGER,
    media_source TEXT NOT NULL DEFAULT '',
    media_id TEXT NOT NULL DEFAULT '',
    sites JSONB NOT NULL DEFAULT '[]'::jsonb,
    "filter" TEXT NOT NULL DEFAULT '',
    description TEXT NOT NULL DEFAULT '',
    poster TEXT NOT NULL DEFAULT '',
    follow_count BIGINT NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX subscribe_shares_username_idx ON subscribe_shares (username, created_at DESC);

CREATE TABLE subscribe_follows (
    id BIGSERIAL PRIMARY KEY,
    username TEXT NOT NULL REFERENCES users(username) ON DELETE CASCADE,
    share_id BIGINT NOT NULL REFERENCES subscribe_shares(id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (username, share_id)
);

CREATE INDEX subscribe_follows_username_idx ON subscribe_follows (username, created_at DESC);

-- +goose Down

DROP TABLE subscribe_follows;
DROP TABLE subscribe_shares;

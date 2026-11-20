-- +goose Up
CREATE TABLE webpush_subscriptions (
    endpoint TEXT PRIMARY KEY,
    username TEXT NOT NULL DEFAULT '',
    p256dh TEXT NOT NULL,
    auth TEXT NOT NULL,
    user_agent TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT webpush_subscriptions_endpoint_check CHECK (length(endpoint) BETWEEN 1 AND 4096),
    CONSTRAINT webpush_subscriptions_username_check CHECK (length(username) <= 512),
    CONSTRAINT webpush_subscriptions_key_check CHECK (
        length(p256dh) BETWEEN 1 AND 256 AND length(auth) BETWEEN 1 AND 256
    )
);

CREATE INDEX webpush_subscriptions_username_idx ON webpush_subscriptions (username);

-- +goose Down
DROP TABLE webpush_subscriptions;

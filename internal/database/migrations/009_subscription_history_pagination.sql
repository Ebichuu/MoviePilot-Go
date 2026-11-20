-- +goose Up
CREATE INDEX IF NOT EXISTS subscriptions_completed_type_idx
    ON subscriptions (media_type, completed_at DESC, id DESC)
    WHERE completed_at IS NOT NULL;

CREATE INDEX IF NOT EXISTS subscriptions_completed_owner_type_idx
    ON subscriptions (username, media_type, completed_at DESC, id DESC)
    WHERE completed_at IS NOT NULL;

-- +goose Down
DROP INDEX IF EXISTS subscriptions_completed_owner_type_idx;
DROP INDEX IF EXISTS subscriptions_completed_type_idx;

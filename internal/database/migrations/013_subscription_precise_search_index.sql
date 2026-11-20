-- +goose Up
CREATE INDEX IF NOT EXISTS subscriptions_precise_search_idx
    ON subscriptions (updated_at, id)
    WHERE internal_state = 'pending_search' AND completed_at IS NULL;

-- +goose Down
DROP INDEX IF EXISTS subscriptions_precise_search_idx;

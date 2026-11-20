-- +goose Up
CREATE INDEX IF NOT EXISTS download_intents_failed_recovery_idx
    ON download_intents (updated_at, id)
    WHERE state = 'failed';

-- +goose Down
DROP INDEX IF EXISTS download_intents_failed_recovery_idx;

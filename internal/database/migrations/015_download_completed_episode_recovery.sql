-- +goose Up
CREATE INDEX IF NOT EXISTS download_intents_completed_episode_recovery_idx
    ON download_intents (updated_at, id)
    WHERE state = 'completed';

-- +goose Down
DROP INDEX IF EXISTS download_intents_completed_episode_recovery_idx;

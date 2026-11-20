-- +goose Up
CREATE INDEX IF NOT EXISTS download_intents_reconciliation_idx
    ON download_intents (updated_at, id)
    WHERE state IN ('submitting', 'downloading');

-- +goose Down
DROP INDEX IF EXISTS download_intents_reconciliation_idx;

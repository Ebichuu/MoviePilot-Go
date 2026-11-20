-- +goose NO TRANSACTION

-- +goose Up
DROP INDEX CONCURRENTLY IF EXISTS download_intents_reconciliation_candidates_idx;

CREATE INDEX CONCURRENTLY download_intents_reconciliation_candidates_idx
    ON download_intents (updated_at, id)
    WHERE state IN ('submitting', 'downloading', 'failed')
       OR (
           state='completed'
           AND organization_reconciliation_pending
       );

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS download_intents_reconciliation_candidates_idx;

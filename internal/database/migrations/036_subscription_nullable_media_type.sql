-- +goose Up
-- Official V2 accepts type=null on a full subscription update. A NULL type is
-- intentionally non-executable and is excluded by both subscription scanners.
ALTER TABLE subscriptions
    ALTER COLUMN media_type DROP NOT NULL;

-- +goose Down
-- PostgreSQL deliberately rejects this rollback while NULL rows remain. The
-- operator must repair those rows explicitly; guessing movie or TV would
-- silently change subscription identity.
ALTER TABLE subscriptions
    ALTER COLUMN media_type SET NOT NULL;

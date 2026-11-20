-- +goose Up
ALTER TABLE subscription_episodes
    ADD CONSTRAINT subscription_episodes_coordinate_check
    CHECK (season BETWEEN 0 AND 999 AND episode BETWEEN 1 AND 5000);

ALTER TABLE subscription_episodes
    ADD CONSTRAINT subscription_episodes_state_check
    CHECK (state IN (
        'missing',
        'acquired',
        'downloading',
        'pending_organization',
        'organizing',
        'indexed'
    ));

ALTER TABLE subscription_episodes
    ADD CONSTRAINT subscription_episodes_download_intent_fk
    FOREIGN KEY (download_intent_id)
    REFERENCES download_intents(id)
    ON DELETE SET NULL;

CREATE INDEX IF NOT EXISTS subscription_episodes_subscription_state_idx
    ON subscription_episodes (subscription_id, state, season, episode);

CREATE INDEX IF NOT EXISTS subscription_episodes_download_intent_idx
    ON subscription_episodes (download_intent_id)
    WHERE download_intent_id IS NOT NULL;

-- +goose Down
DROP INDEX IF EXISTS subscription_episodes_download_intent_idx;
DROP INDEX IF EXISTS subscription_episodes_subscription_state_idx;

ALTER TABLE subscription_episodes
    DROP CONSTRAINT IF EXISTS subscription_episodes_download_intent_fk;

ALTER TABLE subscription_episodes
    DROP CONSTRAINT IF EXISTS subscription_episodes_state_check;

ALTER TABLE subscription_episodes
    DROP CONSTRAINT IF EXISTS subscription_episodes_coordinate_check;

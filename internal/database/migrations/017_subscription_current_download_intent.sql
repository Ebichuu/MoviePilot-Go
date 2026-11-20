-- +goose Up
ALTER TABLE subscriptions
    ADD COLUMN IF NOT EXISTS current_download_intent_id UUID;

WITH ranked_intents AS (
    SELECT di.subscription_id,
        di.id AS intent_id,
        row_number() OVER (
            PARTITION BY di.subscription_id
            ORDER BY
                CASE WHEN EXISTS (
                    SELECT 1
                    FROM subscription_episodes AS se
                    WHERE se.subscription_id=di.subscription_id
                      AND se.download_intent_id=di.id
                      AND se.state IN ('acquired', 'downloading', 'pending_organization', 'organizing')
                ) THEN 0 ELSE 1 END,
                di.updated_at DESC,
                di.id DESC
        ) AS position
    FROM download_intents AS di
    JOIN subscriptions AS s ON s.id=di.subscription_id AND s.username=di.username
    WHERE s.completed_at IS NULL
      AND s.internal_state IN ('acquired', 'downloading', 'pending_organization', 'organizing')
)
UPDATE subscriptions AS s
SET current_download_intent_id=ranked_intents.intent_id
FROM ranked_intents
WHERE ranked_intents.subscription_id=s.id
  AND ranked_intents.position=1
  AND s.current_download_intent_id IS NULL;

ALTER TABLE subscriptions
    DROP CONSTRAINT IF EXISTS subscriptions_current_download_intent_fk;

ALTER TABLE subscriptions
    ADD CONSTRAINT subscriptions_current_download_intent_fk
    FOREIGN KEY (current_download_intent_id)
    REFERENCES download_intents(id)
    ON DELETE SET NULL;

CREATE UNIQUE INDEX IF NOT EXISTS subscriptions_current_download_intent_idx
    ON subscriptions (current_download_intent_id)
    WHERE current_download_intent_id IS NOT NULL;

-- +goose Down
DROP INDEX IF EXISTS subscriptions_current_download_intent_idx;

ALTER TABLE subscriptions
    DROP CONSTRAINT IF EXISTS subscriptions_current_download_intent_fk;

ALTER TABLE subscriptions
    DROP COLUMN IF EXISTS current_download_intent_id;

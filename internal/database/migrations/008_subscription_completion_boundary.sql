-- +goose Up
UPDATE subscriptions
SET internal_state = 'completed',
    v2_state = '',
    completed_at = COALESCE(completed_at, updated_at)
WHERE internal_state = 'completed'
   OR completed_at IS NOT NULL;

ALTER TABLE subscriptions
    DROP CONSTRAINT IF EXISTS subscriptions_completion_state_check;

ALTER TABLE subscriptions
    ADD CONSTRAINT subscriptions_completion_state_check
    CHECK (
        (
            completed_at IS NULL
            AND internal_state <> 'completed'
            AND v2_state IN ('N', 'R', 'P', 'S')
        )
        OR
        (
            completed_at IS NOT NULL
            AND internal_state = 'completed'
            AND v2_state = ''
        )
    );

DROP INDEX IF EXISTS subscriptions_identified_identity_idx;
DROP INDEX IF EXISTS subscriptions_unidentified_identity_idx;

CREATE UNIQUE INDEX subscriptions_identified_identity_idx
    ON subscriptions (
        username,
        media_type,
        lower(btrim(media_source)),
        btrim(media_id),
        COALESCE(season, -1)
    )
    WHERE media_source IS NOT NULL
      AND media_id IS NOT NULL
      AND completed_at IS NULL;

CREATE UNIQUE INDEX subscriptions_unidentified_identity_idx
    ON subscriptions (
        username,
        media_type,
        lower(btrim(title)),
        btrim(COALESCE(year, '')),
        COALESCE(season, -1)
    )
    WHERE media_source IS NULL
      AND media_id IS NULL
      AND completed_at IS NULL;

CREATE INDEX IF NOT EXISTS subscriptions_completed_history_idx
    ON subscriptions (username, completed_at DESC, id DESC)
    WHERE completed_at IS NOT NULL;

-- +goose Down
ALTER TABLE subscriptions
    DROP CONSTRAINT IF EXISTS subscriptions_completion_state_check;

DROP INDEX IF EXISTS subscriptions_completed_history_idx;
DROP INDEX IF EXISTS subscriptions_unidentified_identity_idx;
DROP INDEX IF EXISTS subscriptions_identified_identity_idx;

-- 若历史与活动行身份重复，恢复旧索引会失败并回滚，避免删除历史数据。
CREATE UNIQUE INDEX subscriptions_identified_identity_idx
    ON subscriptions (
        username,
        media_type,
        lower(btrim(media_source)),
        btrim(media_id),
        COALESCE(season, -1)
    )
    WHERE media_source IS NOT NULL AND media_id IS NOT NULL;

CREATE UNIQUE INDEX subscriptions_unidentified_identity_idx
    ON subscriptions (
        username,
        media_type,
        lower(btrim(title)),
        btrim(COALESCE(year, '')),
        COALESCE(season, -1)
    )
    WHERE media_source IS NULL AND media_id IS NULL;

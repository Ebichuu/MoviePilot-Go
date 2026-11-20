-- +goose Up
DROP INDEX IF EXISTS subscriptions_identity_idx;

UPDATE subscriptions
SET media_source = NULL,
    media_id = NULL
WHERE media_source IS NULL
   OR media_id IS NULL
   OR btrim(media_source) = ''
   OR btrim(media_id) = '';

ALTER TABLE subscriptions
    ADD CONSTRAINT subscriptions_media_identity_pair_check
    CHECK (
        (media_source IS NULL AND media_id IS NULL)
        OR
        (
            media_source IS NOT NULL
            AND media_id IS NOT NULL
            AND btrim(media_source) <> ''
            AND btrim(media_id) <> ''
        )
    );

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

-- +goose Down
DROP INDEX IF EXISTS subscriptions_unidentified_identity_idx;
DROP INDEX IF EXISTS subscriptions_identified_identity_idx;

ALTER TABLE subscriptions
    DROP CONSTRAINT IF EXISTS subscriptions_media_identity_pair_check;

-- 若库中已有多条未识别订阅，恢复旧索引会失败并回滚，避免静默合并数据。
CREATE UNIQUE INDEX subscriptions_identity_idx
    ON subscriptions (
        username,
        media_type,
        COALESCE(media_source, ''),
        COALESCE(media_id, ''),
        COALESCE(season, -1)
    );

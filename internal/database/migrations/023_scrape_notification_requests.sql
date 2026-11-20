-- +goose Up

CREATE TABLE scrape_requests (
    id UUID PRIMARY KEY,
    batch_id UUID NOT NULL REFERENCES organization_batches(id) ON DELETE CASCADE,
    intent_id UUID NOT NULL REFERENCES download_intents(id) ON DELETE CASCADE,
    username TEXT NOT NULL REFERENCES users(username),
    kind TEXT NOT NULL,
    media_path TEXT NOT NULL,
    tmdb_id INT NOT NULL DEFAULT 0,
    season INT NOT NULL DEFAULT 0,
    episode INT NOT NULL DEFAULT 0,
    skip_reason TEXT NOT NULL DEFAULT '',
    state TEXT NOT NULL,
    attempt_count INT NOT NULL DEFAULT 0,
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_error TEXT NOT NULL DEFAULT '',
    lease_token UUID,
    lease_expires_at TIMESTAMPTZ,
    completed_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (batch_id, kind, media_path)
);

ALTER TABLE scrape_requests
    ADD CONSTRAINT scrape_requests_kind_check
    CHECK (kind IN ('movie', 'tvshow', 'episode', 'unknown'));

ALTER TABLE scrape_requests
    ADD CONSTRAINT scrape_requests_state_check
    CHECK (state IN ('pending', 'working', 'failed', 'completed', 'skipped'));

ALTER TABLE scrape_requests
    ADD CONSTRAINT scrape_requests_path_check
    CHECK (octet_length(media_path) BETWEEN 1 AND 4096);

ALTER TABLE scrape_requests
    ADD CONSTRAINT scrape_requests_identity_check
    CHECK (
        season BETWEEN 0 AND 999
        AND episode BETWEEN 0 AND 5000
        AND octet_length(skip_reason) <= 128
        AND (
            (state='skipped' AND tmdb_id=0 AND skip_reason<>'')
            OR
            (state<>'skipped' AND skip_reason='' AND tmdb_id>0 AND kind<>'unknown')
        )
        AND (
            kind IN ('movie', 'tvshow', 'unknown')
            OR (kind='episode' AND episode>0)
        )
    );

ALTER TABLE scrape_requests
    ADD CONSTRAINT scrape_requests_attempt_check
    CHECK (attempt_count BETWEEN 0 AND 5);

ALTER TABLE scrape_requests
    ADD CONSTRAINT scrape_requests_error_check
    CHECK (octet_length(last_error) <= 128);

ALTER TABLE scrape_requests
    ADD CONSTRAINT scrape_requests_lease_check
    CHECK (
        (state='working' AND lease_token IS NOT NULL AND lease_expires_at IS NOT NULL)
        OR (state<>'working' AND lease_token IS NULL AND lease_expires_at IS NULL)
    );

ALTER TABLE scrape_requests
    ADD CONSTRAINT scrape_requests_completed_check
    CHECK (
        (state IN ('completed', 'skipped') AND completed_at IS NOT NULL)
        OR (state NOT IN ('completed', 'skipped') AND completed_at IS NULL)
    );

CREATE INDEX scrape_requests_due_idx
    ON scrape_requests (next_attempt_at, id)
    WHERE state IN ('pending', 'failed') AND attempt_count < 5;

CREATE INDEX scrape_requests_batch_terminal_idx
    ON scrape_requests (batch_id, state, attempt_count);

ALTER TABLE notifications
    ADD COLUMN IF NOT EXISTS batch_id UUID REFERENCES organization_batches(id) ON DELETE CASCADE,
    ADD COLUMN IF NOT EXISTS attempt_count INT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    ADD COLUMN IF NOT EXISTS lease_token UUID,
    ADD COLUMN IF NOT EXISTS lease_expires_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS legacy_state TEXT,
    ADD COLUMN IF NOT EXISTS legacy_last_error TEXT,
    ADD COLUMN IF NOT EXISTS legacy_sent_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS updated_at TIMESTAMPTZ NOT NULL DEFAULT now();

-- Notifications created before this migration are historical records and do
-- not have the organization batch identity required by the durable pipeline.
-- Preserve them without making the additive migration fail on old lease or
-- sent-state shapes.
UPDATE notifications
SET legacy_state=state, legacy_last_error=last_error, legacy_sent_at=sent_at,
    state='skipped', sent_at=NULL, last_error='legacy_notification',
    lease_token=NULL, lease_expires_at=NULL, updated_at=now()
WHERE batch_id IS NULL;

UPDATE notifications
SET state='pending', updated_at=now()
WHERE batch_id IS NOT NULL
  AND state NOT IN ('blocked', 'pending', 'working', 'failed', 'sent', 'skipped');

UPDATE notifications
SET last_error=COALESCE(last_error, ''), updated_at=now()
WHERE last_error IS NULL;

ALTER TABLE notifications
    ALTER COLUMN last_error SET DEFAULT '',
    ALTER COLUMN last_error SET NOT NULL;

ALTER TABLE notifications
    DROP CONSTRAINT IF EXISTS notifications_state_check;

ALTER TABLE notifications
    ADD CONSTRAINT notifications_state_check
    CHECK (state IN ('blocked', 'pending', 'working', 'failed', 'sent', 'skipped'));

ALTER TABLE notifications
    ADD CONSTRAINT notifications_channel_check
    CHECK (batch_id IS NULL OR channel IN ('webpush', 'telegram', 'webhook'));

ALTER TABLE notifications
    ADD CONSTRAINT notifications_batch_check
    CHECK (batch_id IS NOT NULL OR state='skipped');

ALTER TABLE notifications
    ADD CONSTRAINT notifications_attempt_check
    CHECK (attempt_count BETWEEN 0 AND 5);

ALTER TABLE notifications
    ADD CONSTRAINT notifications_error_check
    CHECK (octet_length(last_error) <= 128);

ALTER TABLE notifications
    ADD CONSTRAINT notifications_lease_check
    CHECK (
        (state='working' AND lease_token IS NOT NULL AND lease_expires_at IS NOT NULL)
        OR (state<>'working' AND lease_token IS NULL AND lease_expires_at IS NULL)
    );

ALTER TABLE notifications
    ADD CONSTRAINT notifications_sent_check
    CHECK ((state='sent') = (sent_at IS NOT NULL));

CREATE INDEX notifications_due_idx
    ON notifications (next_attempt_at, id)
    WHERE state IN ('pending', 'failed') AND attempt_count < 5;

CREATE INDEX notifications_batch_state_idx
    ON notifications (batch_id, state, attempt_count);

-- +goose StatementBegin
CREATE FUNCTION prevent_scrape_request_plan_mutation()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF NEW.batch_id IS DISTINCT FROM OLD.batch_id
        OR NEW.intent_id IS DISTINCT FROM OLD.intent_id
        OR NEW.username IS DISTINCT FROM OLD.username
        OR NEW.kind IS DISTINCT FROM OLD.kind
        OR NEW.media_path IS DISTINCT FROM OLD.media_path
        OR NEW.tmdb_id IS DISTINCT FROM OLD.tmdb_id
        OR NEW.season IS DISTINCT FROM OLD.season
        OR NEW.episode IS DISTINCT FROM OLD.episode
        OR NEW.skip_reason IS DISTINCT FROM OLD.skip_reason THEN
        RAISE EXCEPTION 'scrape request plan is immutable';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER scrape_request_plan_immutable
    BEFORE UPDATE ON scrape_requests
    FOR EACH ROW
    EXECUTE FUNCTION prevent_scrape_request_plan_mutation();

-- +goose StatementBegin
CREATE FUNCTION prevent_notification_plan_mutation()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF NEW.batch_id IS DISTINCT FROM OLD.batch_id
        OR NEW.username IS DISTINCT FROM OLD.username
        OR NEW.channel IS DISTINCT FROM OLD.channel
        OR NEW.idempotency_key IS DISTINCT FROM OLD.idempotency_key
        OR NEW.payload IS DISTINCT FROM OLD.payload THEN
        RAISE EXCEPTION 'notification plan is immutable';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER notification_plan_immutable
    BEFORE UPDATE ON notifications
    FOR EACH ROW
    EXECUTE FUNCTION prevent_notification_plan_mutation();

-- +goose Down
DROP TRIGGER IF EXISTS notification_plan_immutable ON notifications;
DROP FUNCTION IF EXISTS prevent_notification_plan_mutation();
DROP TRIGGER IF EXISTS scrape_request_plan_immutable ON scrape_requests;
DROP FUNCTION IF EXISTS prevent_scrape_request_plan_mutation();

DROP INDEX IF EXISTS notifications_batch_state_idx;
DROP INDEX IF EXISTS notifications_due_idx;
ALTER TABLE notifications DROP CONSTRAINT IF EXISTS notifications_sent_check;
ALTER TABLE notifications DROP CONSTRAINT IF EXISTS notifications_lease_check;
ALTER TABLE notifications DROP CONSTRAINT IF EXISTS notifications_error_check;
ALTER TABLE notifications DROP CONSTRAINT IF EXISTS notifications_attempt_check;
ALTER TABLE notifications DROP CONSTRAINT IF EXISTS notifications_batch_check;
ALTER TABLE notifications DROP CONSTRAINT IF EXISTS notifications_channel_check;
ALTER TABLE notifications DROP CONSTRAINT IF EXISTS notifications_state_check;
ALTER TABLE notifications
    ALTER COLUMN last_error DROP NOT NULL,
    ALTER COLUMN last_error DROP DEFAULT;
UPDATE notifications
SET state=legacy_state, last_error=legacy_last_error, sent_at=legacy_sent_at
WHERE batch_id IS NULL AND legacy_state IS NOT NULL;
ALTER TABLE notifications
    DROP COLUMN IF EXISTS updated_at,
    DROP COLUMN IF EXISTS legacy_sent_at,
    DROP COLUMN IF EXISTS legacy_last_error,
    DROP COLUMN IF EXISTS legacy_state,
    DROP COLUMN IF EXISTS lease_expires_at,
    DROP COLUMN IF EXISTS lease_token,
    DROP COLUMN IF EXISTS next_attempt_at,
    DROP COLUMN IF EXISTS attempt_count,
    DROP COLUMN IF EXISTS batch_id;

DROP TABLE IF EXISTS scrape_requests;

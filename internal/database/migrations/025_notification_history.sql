-- +goose Up

CREATE TABLE notification_events (
    id BIGSERIAL PRIMARY KEY,
    batch_id UUID UNIQUE REFERENCES organization_batches(id) ON DELETE SET NULL,
    event_key TEXT NOT NULL UNIQUE,
    channel TEXT NOT NULL DEFAULT '',
    source TEXT,
    mtype TEXT,
    title TEXT,
    text TEXT,
    image TEXT,
    link TEXT,
    userid TEXT,
    note JSONB NOT NULL DEFAULT '{}'::jsonb,
    published_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT notification_events_event_key_check
        CHECK (octet_length(event_key) BETWEEN 1 AND 255 AND btrim(event_key)=event_key),
    CONSTRAINT notification_events_note_check
        CHECK (jsonb_typeof(note) IN ('object', 'array'))
);

CREATE INDEX notification_events_published_idx
    ON notification_events (published_at DESC, id DESC)
    WHERE published_at IS NOT NULL;

CREATE INDEX notification_events_system_idx
    ON notification_events (published_at DESC, id DESC)
    WHERE published_at IS NOT NULL AND (image IS NULL OR image='');

CREATE INDEX notification_events_media_idx
    ON notification_events (published_at DESC, id DESC)
    WHERE published_at IS NOT NULL AND image IS NOT NULL AND image<>'';

ALTER TABLE notifications
    ADD COLUMN event_id BIGINT REFERENCES notification_events(id),
    ADD COLUMN source TEXT NOT NULL DEFAULT '';

ALTER TABLE notifications
    DROP CONSTRAINT IF EXISTS notifications_channel_idempotency_key_key;

ALTER TABLE notifications
    ADD CONSTRAINT notifications_channel_source_idempotency_key_key
    UNIQUE (channel, source, idempotency_key);

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION prevent_notification_plan_mutation()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF NEW.batch_id IS DISTINCT FROM OLD.batch_id
        OR NEW.event_id IS DISTINCT FROM OLD.event_id
        OR NEW.username IS DISTINCT FROM OLD.username
        OR NEW.channel IS DISTINCT FROM OLD.channel
        OR NEW.source IS DISTINCT FROM OLD.source
        OR NEW.idempotency_key IS DISTINCT FROM OLD.idempotency_key
        OR NEW.payload IS DISTINCT FROM OLD.payload THEN
        RAISE EXCEPTION 'notification plan is immutable';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

-- +goose Down

-- Restore the 023 trigger body before removing columns referenced by it.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION prevent_notification_plan_mutation()
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

ALTER TABLE notifications
    DROP CONSTRAINT IF EXISTS notifications_channel_source_idempotency_key_key;

ALTER TABLE notifications
    ADD CONSTRAINT notifications_channel_idempotency_key_key
    UNIQUE (channel, idempotency_key);

ALTER TABLE notifications
    DROP COLUMN event_id,
    DROP COLUMN source;

DROP TABLE notification_events;

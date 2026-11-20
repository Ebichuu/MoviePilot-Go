-- +goose Up

CREATE TABLE business_notification_outbox (
    id BIGSERIAL PRIMARY KEY,
    event_key TEXT NOT NULL,
    content_type TEXT NOT NULL,
    username TEXT NOT NULL REFERENCES users(username),
    context JSONB NOT NULL,
    image TEXT NOT NULL DEFAULT '',
    route TEXT NOT NULL DEFAULT '',
    userid TEXT NOT NULL DEFAULT '',
    event_id BIGINT REFERENCES notification_events(id) ON DELETE SET NULL,
    published_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT business_notification_outbox_identity_unique UNIQUE (content_type, event_key),
    CONSTRAINT business_notification_outbox_event_key_check
        CHECK (octet_length(event_key) BETWEEN 1 AND 255 AND btrim(event_key)=event_key),
    CONSTRAINT business_notification_outbox_type_check
        CHECK (content_type IN ('downloadAdded', 'subscribeAdded', 'subscribeComplete')),
    CONSTRAINT business_notification_outbox_context_check
        CHECK (jsonb_typeof(context)='object'),
    CONSTRAINT business_notification_outbox_text_check
        CHECK (octet_length(image) <= 4096 AND octet_length(route) <= 4096 AND octet_length(userid) <= 512),
    CONSTRAINT business_notification_outbox_published_check
        CHECK ((published_at IS NULL AND event_id IS NULL) OR (published_at IS NOT NULL AND event_id IS NOT NULL))
);

CREATE INDEX business_notification_outbox_pending_idx
    ON business_notification_outbox (id)
    WHERE published_at IS NULL;

ALTER TABLE notifications
    DROP CONSTRAINT IF EXISTS notifications_batch_check;

ALTER TABLE notifications
    ADD CONSTRAINT notifications_batch_check
    CHECK (batch_id IS NOT NULL OR state='skipped' OR event_id IS NOT NULL);

ALTER TABLE notifications
    DROP CONSTRAINT IF EXISTS notifications_channel_check;

ALTER TABLE notifications
    ADD CONSTRAINT notifications_channel_check
    CHECK ((batch_id IS NULL AND state='skipped') OR channel IN (
        'webpush', 'telegram', 'webhook', 'discord', 'feishu', 'slack',
        'synologychat', 'vocechat', 'qqbot', 'wechat'
    ));

-- New subscriptions and new completions are captured from their source write.
-- Existing completed rows are deliberately not backfilled.
-- +goose StatementBegin
CREATE FUNCTION enqueue_subscription_business_notifications()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
    title_year TEXT;
    season_fmt TEXT;
    completion_context JSONB;
    route TEXT;
BEGIN
    title_year := COALESCE(NULLIF(BTRIM(NEW.title), ''), '');
    IF NULLIF(BTRIM(COALESCE(NEW.year, '')), '') IS NOT NULL THEN
        title_year := title_year || ' (' || BTRIM(NEW.year) || ')';
    END IF;
    season_fmt := CASE WHEN NEW.season IS NULL THEN '' ELSE 'S' || LPAD(NEW.season::TEXT, 2, '0') END;

    IF TG_OP = 'INSERT' THEN
        INSERT INTO business_notification_outbox (
            event_key, content_type, username, context, image, route
        ) VALUES (
            NEW.id::TEXT, 'subscribeAdded', NEW.username,
            jsonb_build_object('title_year', title_year, 'season_fmt', season_fmt, 'username', NEW.username),
            '', CASE WHEN LOWER(COALESCE(NEW.media_type, '')) IN ('tv', '电视剧')
                THEN '#/subscribe/tv?tab=mysub' ELSE '#/subscribe/movie?tab=mysub' END
        ) ON CONFLICT (content_type, event_key) DO NOTHING;
        RETURN NEW;
    END IF;

    IF OLD.completed_at IS NULL AND NEW.completed_at IS NOT NULL THEN
        completion_context := jsonb_build_object(
            'title_year', title_year,
            'season_fmt', season_fmt,
            'msgstr', CASE WHEN NEW.config->'best_version' IN ('true'::jsonb, '1'::jsonb) THEN '洗版' ELSE '订阅' END,
            'vote_average', '',
            'username', NEW.username,
            'actors', '',
            'overview', ''
        );
        route := CASE WHEN LOWER(COALESCE(NEW.media_type, '')) IN ('tv', '电视剧')
            THEN '#/subscribe/tv?tab=mysub' ELSE '#/subscribe/movie?tab=mysub' END;
        INSERT INTO business_notification_outbox (
            event_key, content_type, username, context, image, route
        ) VALUES (
            NEW.id::TEXT, 'subscribeComplete', NEW.username, completion_context, '', route
        ) ON CONFLICT (content_type, event_key) DO NOTHING;
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER subscriptions_business_notification_insert
    AFTER INSERT ON subscriptions
    FOR EACH ROW EXECUTE FUNCTION enqueue_subscription_business_notifications();

CREATE TRIGGER subscriptions_business_notification_complete
    AFTER UPDATE OF completed_at ON subscriptions
    FOR EACH ROW EXECUTE FUNCTION enqueue_subscription_business_notifications();

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM notifications
        WHERE batch_id IS NULL AND event_id IS NOT NULL AND state<>'skipped'
    ) THEN
        RAISE EXCEPTION 'cannot remove business notification requests while they are active';
    END IF;
END;
$$;
-- +goose StatementEnd

DROP TRIGGER IF EXISTS subscriptions_business_notification_complete ON subscriptions;
DROP TRIGGER IF EXISTS subscriptions_business_notification_insert ON subscriptions;
DROP FUNCTION IF EXISTS enqueue_subscription_business_notifications();
DROP TABLE IF EXISTS business_notification_outbox;

ALTER TABLE notifications
    DROP CONSTRAINT IF EXISTS notifications_channel_check;

ALTER TABLE notifications
    ADD CONSTRAINT notifications_channel_check
    CHECK (batch_id IS NULL OR channel IN ('webpush', 'telegram', 'webhook'));

ALTER TABLE notifications
    DROP CONSTRAINT IF EXISTS notifications_batch_check;

ALTER TABLE notifications
    ADD CONSTRAINT notifications_batch_check
    CHECK (batch_id IS NOT NULL OR state='skipped');

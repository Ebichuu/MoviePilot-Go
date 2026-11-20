-- +goose Up

ALTER TABLE notification_events
    DROP CONSTRAINT notification_events_batch_id_key;

CREATE INDEX notification_events_batch_idx
    ON notification_events (batch_id, id)
    WHERE batch_id IS NOT NULL;

-- +goose Down

-- Reinstating the V2 single-event shape is only lossless when every batch has
-- at most one event. Refuse the rollback explicitly instead of deleting or
-- arbitrarily merging durable history.
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM notification_events
        WHERE batch_id IS NOT NULL
        GROUP BY batch_id
        HAVING count(*) > 1
    ) THEN
        RAISE EXCEPTION 'cannot restore one notification event per batch while grouped events exist';
    END IF;
END;
$$;
-- +goose StatementEnd

DROP INDEX notification_events_batch_idx;

ALTER TABLE notification_events
    ADD CONSTRAINT notification_events_batch_id_key UNIQUE (batch_id);

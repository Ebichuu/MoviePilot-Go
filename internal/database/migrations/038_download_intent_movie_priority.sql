-- +goose Up
-- Freeze selected movie quality for acceptance and unknown-Add reconciliation.
-- NULL retains the meaning of old plans: no recorded quality, never inferred.
ALTER TABLE download_intent_plans ADD COLUMN movie_priority INTEGER;

-- +goose Down
LOCK TABLE download_intent_plans IN ACCESS EXCLUSIVE MODE;
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM download_intent_plans WHERE movie_priority IS NOT NULL) THEN
        RAISE EXCEPTION 'Cannot drop recorded movie quality facts; back up and reconcile first';
    END IF;
END;
$$;
-- +goose StatementEnd
ALTER TABLE download_intent_plans DROP COLUMN movie_priority;

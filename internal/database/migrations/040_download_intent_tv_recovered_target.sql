-- +goose Up
ALTER TABLE download_intent_plans ADD COLUMN tv_recovered_target JSONB;

ALTER TABLE download_intent_plans ADD CONSTRAINT download_intent_plans_tv_recovered_target_check CHECK (
    tv_recovered_target IS NULL OR (
        jsonb_typeof(tv_recovered_target) = 'object'
        AND tv_priority IS NOT NULL AND tv_coverage IS NOT NULL AND movie_priority IS NULL
    )
);

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
    LOCK TABLE download_intent_plans IN ACCESS EXCLUSIVE MODE;
    IF EXISTS (SELECT 1 FROM download_intent_plans WHERE tv_recovered_target IS NOT NULL) THEN
        RAISE EXCEPTION 'cannot remove persisted TV recovered target facts';
    END IF;
END;
$$;
-- +goose StatementEnd
ALTER TABLE download_intent_plans DROP CONSTRAINT download_intent_plans_tv_recovered_target_check;
ALTER TABLE download_intent_plans DROP COLUMN tv_recovered_target;

-- +goose Up
ALTER TABLE download_intent_plans
    ADD COLUMN tv_priority INTEGER,
    ADD COLUMN tv_coverage JSONB,
    ADD COLUMN tv_full_coverage BOOLEAN NOT NULL DEFAULT FALSE;

ALTER TABLE download_intent_plans ADD CONSTRAINT download_intent_plans_tv_quality_check CHECK (
    (tv_priority IS NULL OR tv_coverage IS NOT NULL)
    AND (tv_coverage IS NULL OR jsonb_typeof(tv_coverage) = 'array')
    AND (NOT tv_full_coverage OR tv_coverage IS NOT NULL)
    AND (movie_priority IS NULL OR (tv_priority IS NULL AND tv_coverage IS NULL AND NOT tv_full_coverage))
);

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
    LOCK TABLE download_intent_plans IN ACCESS EXCLUSIVE MODE;
    IF EXISTS (SELECT 1 FROM download_intent_plans
               WHERE tv_priority IS NOT NULL OR tv_coverage IS NOT NULL OR tv_full_coverage) THEN
        RAISE EXCEPTION 'cannot remove persisted TV download quality facts';
    END IF;
END;
$$;
-- +goose StatementEnd
ALTER TABLE download_intent_plans DROP CONSTRAINT download_intent_plans_tv_quality_check;
ALTER TABLE download_intent_plans DROP COLUMN tv_priority, DROP COLUMN tv_coverage, DROP COLUMN tv_full_coverage;

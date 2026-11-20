-- +goose Up

-- Existing installations may contain rows written by the initial schema.  The
-- new columns intentionally keep conservative defaults so the migration is
-- additive and can be applied without a data rewrite.
ALTER TABLE organization_batches
    ADD COLUMN IF NOT EXISTS lease_token UUID,
    ADD COLUMN IF NOT EXISTS lease_expires_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS attempt_count INT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    ADD COLUMN IF NOT EXISTS completed_at TIMESTAMPTZ;

UPDATE organization_batches SET last_error='' WHERE last_error IS NULL;
ALTER TABLE organization_batches
    ALTER COLUMN last_error SET DEFAULT '',
    ALTER COLUMN last_error SET NOT NULL;

ALTER TABLE organization_files
    ADD COLUMN IF NOT EXISTS ordinal INT,
    ADD COLUMN IF NOT EXISTS mode TEXT NOT NULL DEFAULT 'copy',
    ADD COLUMN IF NOT EXISTS attempt_count INT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS last_error TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS completed_at TIMESTAMPTZ;

WITH ranked AS (
    SELECT id, (row_number() OVER (PARTITION BY batch_id ORDER BY id) - 1)::integer AS ordinal
    FROM organization_files
)
UPDATE organization_files AS f
SET ordinal=ranked.ordinal
FROM ranked
WHERE ranked.id=f.id AND f.ordinal IS NULL;

ALTER TABLE organization_files
    ALTER COLUMN ordinal SET NOT NULL;

ALTER TABLE organization_batches
    DROP CONSTRAINT IF EXISTS organization_batches_intent_id_fkey;

ALTER TABLE organization_batches
    ADD CONSTRAINT organization_batches_intent_id_fkey
    FOREIGN KEY (intent_id) REFERENCES download_intents(id) ON DELETE CASCADE;

CREATE UNIQUE INDEX IF NOT EXISTS organization_batches_intent_idx
    ON organization_batches (intent_id)
    WHERE intent_id IS NOT NULL;

CREATE UNIQUE INDEX IF NOT EXISTS organization_files_ordinal_idx
    ON organization_files (batch_id, ordinal);

CREATE INDEX IF NOT EXISTS organization_batches_claim_idx
    ON organization_batches (state, next_attempt_at, updated_at, id);

CREATE INDEX IF NOT EXISTS organization_files_claim_idx
    ON organization_files (batch_id, state, ordinal, id);

ALTER TABLE organization_batches
    ADD CONSTRAINT organization_batches_state_check
    CHECK (state IN ('planned', 'organizing', 'failed', 'completed')) NOT VALID;

ALTER TABLE organization_batches
    ADD CONSTRAINT organization_batches_attempt_count_check
    CHECK (attempt_count BETWEEN 0 AND 8) NOT VALID;

ALTER TABLE organization_batches
    ADD CONSTRAINT organization_batches_plan_check
    CHECK (
        jsonb_typeof(plan) = 'array'
        AND jsonb_array_length(plan) BETWEEN 1 AND 10000
        AND octet_length(plan::text) <= 16777216
    ) NOT VALID;

ALTER TABLE organization_batches
    ADD CONSTRAINT organization_batches_error_length_check
    CHECK (last_error IS NULL OR octet_length(last_error) <= 2048) NOT VALID;

ALTER TABLE organization_batches
    ADD CONSTRAINT organization_batches_root_length_check
    CHECK (octet_length(source_root) BETWEEN 1 AND 4096 AND octet_length(target_root) BETWEEN 1 AND 4096) NOT VALID;

ALTER TABLE organization_batches
    ADD CONSTRAINT organization_batches_lease_check
    CHECK (
        (state='organizing' AND lease_token IS NOT NULL AND lease_expires_at IS NOT NULL)
        OR (state<>'organizing' AND lease_token IS NULL AND lease_expires_at IS NULL)
    ) NOT VALID;

ALTER TABLE organization_batches
    ADD CONSTRAINT organization_batches_completed_at_check
    CHECK ((state='completed') = (completed_at IS NOT NULL)) NOT VALID;

ALTER TABLE organization_files
    ADD CONSTRAINT organization_files_state_check
    CHECK (state IN ('planned', 'organizing', 'failed', 'completed')) NOT VALID;

ALTER TABLE organization_files
    ADD CONSTRAINT organization_files_attempt_count_check
    CHECK (attempt_count BETWEEN 0 AND 8) NOT VALID;

ALTER TABLE organization_files
    ADD CONSTRAINT organization_files_mode_check
    CHECK (mode IN ('copy', 'move', 'hardlink', 'symlink')) NOT VALID;

ALTER TABLE organization_files
    ADD CONSTRAINT organization_files_path_length_check
    CHECK (octet_length(source_path) BETWEEN 1 AND 4096 AND octet_length(target_path) BETWEEN 1 AND 4096) NOT VALID;

ALTER TABLE organization_files
    ADD CONSTRAINT organization_files_error_length_check
    CHECK (octet_length(last_error) <= 2048) NOT VALID;

ALTER TABLE organization_files
    ADD CONSTRAINT organization_files_completed_at_check
    CHECK ((state='completed') = (completed_at IS NOT NULL)) NOT VALID;

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION prevent_organization_batch_plan_mutation()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF NEW.intent_id IS DISTINCT FROM OLD.intent_id
        OR NEW.source_root IS DISTINCT FROM OLD.source_root
        OR NEW.target_root IS DISTINCT FROM OLD.target_root
        OR NEW.plan IS DISTINCT FROM OLD.plan THEN
        RAISE EXCEPTION 'organization batch plan is immutable';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

DROP TRIGGER IF EXISTS organization_batch_plan_immutable ON organization_batches;
CREATE TRIGGER organization_batch_plan_immutable
    BEFORE UPDATE ON organization_batches
    FOR EACH ROW
    EXECUTE FUNCTION prevent_organization_batch_plan_mutation();

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION prevent_organization_file_plan_mutation()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF NEW.batch_id IS DISTINCT FROM OLD.batch_id
        OR NEW.ordinal IS DISTINCT FROM OLD.ordinal
        OR NEW.source_path IS DISTINCT FROM OLD.source_path
        OR NEW.target_path IS DISTINCT FROM OLD.target_path
        OR NEW.mode IS DISTINCT FROM OLD.mode THEN
        RAISE EXCEPTION 'organization file plan is immutable';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

DROP TRIGGER IF EXISTS organization_file_plan_immutable ON organization_files;
CREATE TRIGGER organization_file_plan_immutable
    BEFORE UPDATE ON organization_files
    FOR EACH ROW
    EXECUTE FUNCTION prevent_organization_file_plan_mutation();

-- +goose Down
DROP TRIGGER IF EXISTS organization_file_plan_immutable ON organization_files;
DROP TRIGGER IF EXISTS organization_batch_plan_immutable ON organization_batches;
DROP FUNCTION IF EXISTS prevent_organization_file_plan_mutation();
DROP FUNCTION IF EXISTS prevent_organization_batch_plan_mutation();
ALTER TABLE organization_files DROP CONSTRAINT IF EXISTS organization_files_error_length_check;
ALTER TABLE organization_files DROP CONSTRAINT IF EXISTS organization_files_completed_at_check;
ALTER TABLE organization_files DROP CONSTRAINT IF EXISTS organization_files_path_length_check;
ALTER TABLE organization_files DROP CONSTRAINT IF EXISTS organization_files_mode_check;
ALTER TABLE organization_files DROP CONSTRAINT IF EXISTS organization_files_attempt_count_check;
ALTER TABLE organization_files DROP CONSTRAINT IF EXISTS organization_files_state_check;
ALTER TABLE organization_batches DROP CONSTRAINT IF EXISTS organization_batches_error_length_check;
ALTER TABLE organization_batches DROP CONSTRAINT IF EXISTS organization_batches_root_length_check;
ALTER TABLE organization_batches DROP CONSTRAINT IF EXISTS organization_batches_completed_at_check;
ALTER TABLE organization_batches DROP CONSTRAINT IF EXISTS organization_batches_lease_check;
ALTER TABLE organization_batches DROP CONSTRAINT IF EXISTS organization_batches_plan_check;
ALTER TABLE organization_batches DROP CONSTRAINT IF EXISTS organization_batches_attempt_count_check;
ALTER TABLE organization_batches DROP CONSTRAINT IF EXISTS organization_batches_state_check;
DROP INDEX IF EXISTS organization_files_claim_idx;
DROP INDEX IF EXISTS organization_batches_claim_idx;
DROP INDEX IF EXISTS organization_files_ordinal_idx;
DROP INDEX IF EXISTS organization_batches_intent_idx;
ALTER TABLE organization_batches
    DROP CONSTRAINT IF EXISTS organization_batches_intent_id_fkey;
ALTER TABLE organization_batches
    ADD CONSTRAINT organization_batches_intent_id_fkey
    FOREIGN KEY (intent_id) REFERENCES download_intents(id) ON DELETE SET NULL;
ALTER TABLE organization_batches
    ALTER COLUMN last_error DROP NOT NULL,
    ALTER COLUMN last_error DROP DEFAULT;
ALTER TABLE organization_files
    DROP COLUMN IF EXISTS completed_at,
    DROP COLUMN IF EXISTS last_error,
    DROP COLUMN IF EXISTS attempt_count,
    DROP COLUMN IF EXISTS mode,
    DROP COLUMN IF EXISTS ordinal;
ALTER TABLE organization_batches
    DROP COLUMN IF EXISTS completed_at,
    DROP COLUMN IF EXISTS next_attempt_at,
    DROP COLUMN IF EXISTS attempt_count,
    DROP COLUMN IF EXISTS lease_expires_at,
    DROP COLUMN IF EXISTS lease_token;

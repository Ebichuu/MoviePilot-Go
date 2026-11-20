-- +goose Up

-- Legacy rows cannot be fingerprinted from PostgreSQL because their source
-- files live on the application host. Conservative sentinel values keep the
-- migration additive and readable while ensuring execution fails closed until
-- a real fingerprinted plan is created.
ALTER TABLE organization_files
    ADD COLUMN IF NOT EXISTS expected_size BIGINT,
    ADD COLUMN IF NOT EXISTS expected_sha256 TEXT;

UPDATE organization_files
SET expected_size=COALESCE(expected_size, 0),
    expected_sha256=COALESCE(expected_sha256, repeat('0', 64))
WHERE expected_size IS NULL OR expected_sha256 IS NULL;

ALTER TABLE organization_files
    ALTER COLUMN expected_size SET DEFAULT 0,
    ALTER COLUMN expected_size SET NOT NULL,
    ALTER COLUMN expected_sha256 SET DEFAULT repeat('0', 64),
    ALTER COLUMN expected_sha256 SET NOT NULL;

ALTER TABLE organization_files
    ADD CONSTRAINT organization_files_expected_size_check
    CHECK (expected_size >= 0) NOT VALID;

ALTER TABLE organization_files
    ADD CONSTRAINT organization_files_expected_sha256_check
    CHECK (expected_sha256 ~ '^[0-9a-f]{64}$') NOT VALID;

ALTER TABLE organization_files
    VALIDATE CONSTRAINT organization_files_expected_size_check;

ALTER TABLE organization_files
    VALIDATE CONSTRAINT organization_files_expected_sha256_check;

-- Backfill legacy batch JSON with the same fail-closed sentinel fingerprints.
-- Extremely large legacy plans that would cross the application's 16 MiB
-- bound are intentionally left untouched so the migration itself can finish;
-- the strict scanner will reject those plans instead of executing them.
DROP TRIGGER IF EXISTS organization_batch_plan_immutable ON organization_batches;

WITH patched AS (
    SELECT b.id,
        jsonb_agg(
            CASE
                WHEN jsonb_typeof(operation.value) <> 'object' THEN operation.value
                WHEN operation.value ? 'expected_size'
                    AND operation.value ? 'expected_sha256' THEN operation.value
                ELSE operation.value || jsonb_build_object(
                    'expected_size', COALESCE(f.expected_size, 0),
                    'expected_sha256', COALESCE(f.expected_sha256, repeat('0', 64))
                )
            END
            ORDER BY operation.ordinality
        ) AS plan
    FROM (
        SELECT id, plan
        FROM organization_batches
        WHERE jsonb_typeof(plan)='array'
    ) AS b
    CROSS JOIN LATERAL jsonb_array_elements(b.plan)
        WITH ORDINALITY AS operation(value, ordinality)
    LEFT JOIN organization_files AS f
        ON f.batch_id=b.id AND f.ordinal=(operation.ordinality - 1)
    GROUP BY b.id
)
UPDATE organization_batches AS b
SET plan=patched.plan
FROM patched
WHERE b.id=patched.id
    AND b.plan IS DISTINCT FROM patched.plan
    AND octet_length(patched.plan::text) <= 16777216;

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
        OR NEW.mode IS DISTINCT FROM OLD.mode
        OR NEW.expected_size IS DISTINCT FROM OLD.expected_size
        OR NEW.expected_sha256 IS DISTINCT FROM OLD.expected_sha256 THEN
        RAISE EXCEPTION 'organization file plan is immutable';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

-- +goose Down
DROP TRIGGER IF EXISTS organization_batch_plan_immutable ON organization_batches;

WITH stripped AS (
    SELECT b.id,
        jsonb_agg(
            CASE
                WHEN jsonb_typeof(operation.value) <> 'object' THEN operation.value
                ELSE (operation.value - 'expected_size') - 'expected_sha256'
            END
            ORDER BY operation.ordinality
        ) AS plan
    FROM (
        SELECT id, plan
        FROM organization_batches
        WHERE jsonb_typeof(plan)='array'
    ) AS b
    CROSS JOIN LATERAL jsonb_array_elements(b.plan)
        WITH ORDINALITY AS operation(value, ordinality)
    GROUP BY b.id
)
UPDATE organization_batches AS b
SET plan=stripped.plan
FROM stripped
WHERE b.id=stripped.id AND b.plan IS DISTINCT FROM stripped.plan;

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

ALTER TABLE organization_files
    DROP CONSTRAINT IF EXISTS organization_files_expected_sha256_check;

ALTER TABLE organization_files
    DROP CONSTRAINT IF EXISTS organization_files_expected_size_check;

ALTER TABLE organization_files
    DROP COLUMN IF EXISTS expected_sha256,
    DROP COLUMN IF EXISTS expected_size;

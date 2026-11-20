-- +goose Up

ALTER TABLE organization_files
    DROP CONSTRAINT IF EXISTS organization_files_completed_at_check;

ALTER TABLE organization_files
    DROP CONSTRAINT IF EXISTS organization_files_state_check;

ALTER TABLE organization_files
    ADD CONSTRAINT organization_files_state_check
    CHECK (state IN ('planned', 'organizing', 'failed', 'terminal_failed', 'completed')) NOT VALID;

ALTER TABLE organization_files
    ADD CONSTRAINT organization_files_completed_at_check
    CHECK ((state IN ('completed', 'terminal_failed')) = (completed_at IS NOT NULL)) NOT VALID;

ALTER TABLE organization_files
    ADD CONSTRAINT organization_files_terminal_error_check
    CHECK (state <> 'terminal_failed' OR octet_length(last_error) BETWEEN 1 AND 2048) NOT VALID;

ALTER TABLE organization_files
    VALIDATE CONSTRAINT organization_files_state_check;

ALTER TABLE organization_files
    VALIDATE CONSTRAINT organization_files_completed_at_check;

ALTER TABLE organization_files
    VALIDATE CONSTRAINT organization_files_terminal_error_check;

-- +goose Down

UPDATE organization_files SET state='failed', completed_at=NULL WHERE state='terminal_failed';

ALTER TABLE organization_files
    DROP CONSTRAINT IF EXISTS organization_files_terminal_error_check;

ALTER TABLE organization_files
    DROP CONSTRAINT IF EXISTS organization_files_completed_at_check;

ALTER TABLE organization_files
    DROP CONSTRAINT IF EXISTS organization_files_state_check;

ALTER TABLE organization_files
    ADD CONSTRAINT organization_files_state_check
    CHECK (state IN ('planned', 'organizing', 'failed', 'completed')) NOT VALID;

ALTER TABLE organization_files
    ADD CONSTRAINT organization_files_completed_at_check
    CHECK ((state='completed') = (completed_at IS NOT NULL)) NOT VALID;

ALTER TABLE organization_files
    VALIDATE CONSTRAINT organization_files_state_check;

ALTER TABLE organization_files
    VALIDATE CONSTRAINT organization_files_completed_at_check;

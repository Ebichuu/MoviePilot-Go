-- +goose Up

ALTER TABLE subscription_episodes
    DROP CONSTRAINT IF EXISTS subscription_episodes_state_check;

ALTER TABLE subscription_episodes
    ADD CONSTRAINT subscription_episodes_state_check
    CHECK (state IN (
        'missing',
        'acquired',
        'downloading',
        'pending_organization',
        'organizing',
        'pending_index',
        'indexed'
    )) NOT VALID;

ALTER TABLE subscription_episodes
    VALIDATE CONSTRAINT subscription_episodes_state_check;

CREATE TABLE media_index_requests (
    id UUID PRIMARY KEY,
    batch_id UUID NOT NULL REFERENCES organization_batches(id) ON DELETE CASCADE,
    server_id UUID NOT NULL,
    server_name TEXT NOT NULL,
    server_type TEXT NOT NULL,
    server_fingerprint TEXT NOT NULL,
    expected_file_count INT NOT NULL,
    state TEXT NOT NULL DEFAULT 'pending',
    attempt_count INT NOT NULL DEFAULT 0,
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_error TEXT NOT NULL DEFAULT '',
    lease_token UUID,
    lease_expires_at TIMESTAMPTZ,
    refresh_requested_at TIMESTAMPTZ,
    completed_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (batch_id, server_id)
);

CREATE TABLE media_index_files (
    id BIGSERIAL PRIMARY KEY,
    request_id UUID NOT NULL REFERENCES media_index_requests(id) ON DELETE CASCADE,
    ordinal INT NOT NULL,
    local_path TEXT NOT NULL,
    remote_path TEXT NOT NULL,
    path_style TEXT NOT NULL,
    case_sensitive BOOLEAN NOT NULL,
    expected_size BIGINT NOT NULL,
    expected_sha256 TEXT NOT NULL,
    media_type TEXT NOT NULL,
    season INT NOT NULL DEFAULT 0,
    episode INT NOT NULL DEFAULT 0,
    provider_id TEXT NOT NULL DEFAULT '',
    confirmed_item_id TEXT NOT NULL DEFAULT '',
    confirmed_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (request_id, ordinal)
);

ALTER TABLE media_index_requests
    ADD CONSTRAINT media_index_requests_state_check
    CHECK (state IN ('pending', 'working', 'failed', 'confirmed'));

ALTER TABLE media_index_requests
    ADD CONSTRAINT media_index_requests_attempt_count_check
    CHECK (attempt_count >= 0);

ALTER TABLE media_index_requests
    ADD CONSTRAINT media_index_requests_file_count_check
    CHECK (expected_file_count BETWEEN 1 AND 10000);

ALTER TABLE media_index_requests
    ADD CONSTRAINT media_index_requests_error_length_check
    CHECK (octet_length(last_error) <= 2048);

ALTER TABLE media_index_requests
    ADD CONSTRAINT media_index_requests_server_check
    CHECK (
        octet_length(server_name) BETWEEN 1 AND 255
        AND server_type = 'emby'
    );

ALTER TABLE media_index_requests
    ADD CONSTRAINT media_index_requests_server_fingerprint_check
    CHECK (server_fingerprint ~ '^[0-9a-f]{64}$');

ALTER TABLE media_index_requests
    ADD CONSTRAINT media_index_requests_lease_check
    CHECK (
        (state='working' AND lease_token IS NOT NULL AND lease_expires_at IS NOT NULL)
        OR (state<>'working' AND lease_token IS NULL AND lease_expires_at IS NULL)
    );

ALTER TABLE media_index_requests
    ADD CONSTRAINT media_index_requests_completed_at_check
    CHECK ((state='confirmed') = (completed_at IS NOT NULL));

ALTER TABLE media_index_requests
    ADD CONSTRAINT media_index_requests_refresh_check
    CHECK (
        (state='pending' AND refresh_requested_at IS NULL)
        OR state<>'pending'
    );

ALTER TABLE media_index_requests
    ADD CONSTRAINT media_index_requests_completed_refresh_check
    CHECK (completed_at IS NULL OR refresh_requested_at IS NOT NULL);

ALTER TABLE media_index_files
    ADD CONSTRAINT media_index_files_ordinal_check
    CHECK (ordinal BETWEEN 0 AND 9999);

ALTER TABLE media_index_files
    ADD CONSTRAINT media_index_files_path_length_check
    CHECK (
        octet_length(local_path) BETWEEN 1 AND 4096
        AND octet_length(remote_path) BETWEEN 1 AND 4096
    );

ALTER TABLE media_index_files
    ADD CONSTRAINT media_index_files_path_style_check
    CHECK (path_style IN ('posix', 'windows'));

ALTER TABLE media_index_files
    ADD CONSTRAINT media_index_files_fingerprint_check
    CHECK (expected_size >= 0 AND expected_sha256 ~ '^[0-9a-f]{64}$');

ALTER TABLE media_index_files
    ADD CONSTRAINT media_index_files_media_check
    CHECK (
        octet_length(media_type) BETWEEN 1 AND 64
        AND season BETWEEN 0 AND 999
        AND episode BETWEEN 0 AND 5000
        AND octet_length(provider_id) <= 4096
    );

ALTER TABLE media_index_files
    ADD CONSTRAINT media_index_files_confirmation_check
    CHECK (
        (
            confirmed_at IS NULL
            AND confirmed_item_id = ''
        )
        OR
        (
            confirmed_at IS NOT NULL
            AND octet_length(confirmed_item_id) BETWEEN 1 AND 4096
        )
    );

CREATE INDEX media_index_requests_claim_idx
    ON media_index_requests (state, next_attempt_at, lease_expires_at, id)
    WHERE state <> 'confirmed';

CREATE INDEX media_index_files_pending_idx
    ON media_index_files (request_id, ordinal, id)
    WHERE confirmed_at IS NULL;

-- +goose StatementBegin
CREATE FUNCTION prevent_media_index_request_plan_mutation()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF NEW.batch_id IS DISTINCT FROM OLD.batch_id
        OR NEW.server_id IS DISTINCT FROM OLD.server_id
        OR NEW.server_name IS DISTINCT FROM OLD.server_name
        OR NEW.server_type IS DISTINCT FROM OLD.server_type
        OR NEW.server_fingerprint IS DISTINCT FROM OLD.server_fingerprint
        OR NEW.expected_file_count IS DISTINCT FROM OLD.expected_file_count THEN
        RAISE EXCEPTION 'media index request plan is immutable';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER media_index_request_plan_immutable
    BEFORE UPDATE ON media_index_requests
    FOR EACH ROW
    EXECUTE FUNCTION prevent_media_index_request_plan_mutation();

-- +goose StatementBegin
CREATE FUNCTION prevent_media_index_file_plan_mutation()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF NEW.request_id IS DISTINCT FROM OLD.request_id
        OR NEW.ordinal IS DISTINCT FROM OLD.ordinal
        OR NEW.local_path IS DISTINCT FROM OLD.local_path
        OR NEW.remote_path IS DISTINCT FROM OLD.remote_path
        OR NEW.path_style IS DISTINCT FROM OLD.path_style
        OR NEW.case_sensitive IS DISTINCT FROM OLD.case_sensitive
        OR NEW.expected_size IS DISTINCT FROM OLD.expected_size
        OR NEW.expected_sha256 IS DISTINCT FROM OLD.expected_sha256
        OR NEW.media_type IS DISTINCT FROM OLD.media_type
        OR NEW.season IS DISTINCT FROM OLD.season
        OR NEW.episode IS DISTINCT FROM OLD.episode
        OR NEW.provider_id IS DISTINCT FROM OLD.provider_id THEN
        RAISE EXCEPTION 'media index file plan is immutable';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER media_index_file_plan_immutable
    BEFORE UPDATE ON media_index_files
    FOR EACH ROW
    EXECUTE FUNCTION prevent_media_index_file_plan_mutation();

-- +goose Down
UPDATE subscription_episodes
SET state='pending_organization', updated_at=now()
WHERE state='pending_index';

UPDATE subscriptions
SET internal_state='pending_organization', v2_state='R', updated_at=now()
WHERE internal_state='pending_index';

DROP TRIGGER IF EXISTS media_index_file_plan_immutable ON media_index_files;
DROP TRIGGER IF EXISTS media_index_request_plan_immutable ON media_index_requests;
DROP FUNCTION IF EXISTS prevent_media_index_file_plan_mutation();
DROP FUNCTION IF EXISTS prevent_media_index_request_plan_mutation();
DROP TABLE IF EXISTS media_index_files;
DROP TABLE IF EXISTS media_index_requests;

ALTER TABLE subscription_episodes
    DROP CONSTRAINT IF EXISTS subscription_episodes_state_check;

ALTER TABLE subscription_episodes
    ADD CONSTRAINT subscription_episodes_state_check
    CHECK (state IN (
        'missing',
        'acquired',
        'downloading',
        'pending_organization',
        'organizing',
        'indexed'
    )) NOT VALID;

ALTER TABLE subscription_episodes
    VALIDATE CONSTRAINT subscription_episodes_state_check;

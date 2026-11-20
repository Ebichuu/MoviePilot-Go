-- +goose Up

CREATE TABLE download_intent_success_inputs (
    intent_id UUID PRIMARY KEY
        REFERENCES download_intent_plans(intent_id) ON DELETE CASCADE,
    version SMALLINT NOT NULL,
    legacy BOOLEAN NOT NULL,
    username TEXT NOT NULL,
    manual BOOLEAN NOT NULL,
    background BOOLEAN NOT NULL,
    media_source TEXT NOT NULL,
    media_id TEXT NOT NULL,
    title_year TEXT NOT NULL,
    media_type TEXT NOT NULL,
    category TEXT NOT NULL,
    vote_average DOUBLE PRECISION NOT NULL,
    resource_term TEXT NOT NULL,
    poster_url TEXT NOT NULL,
    backdrop_url TEXT NOT NULL,
    episodes JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT download_intent_success_inputs_version_check CHECK (
        (legacy AND version=0) OR (NOT legacy AND version=1)
    ),
    CONSTRAINT download_intent_success_inputs_text_bounds_check CHECK (
        octet_length(username) <= 256
        AND octet_length(media_source) <= 32
        AND octet_length(media_id) <= 128
        AND octet_length(title_year) <= 512
        AND octet_length(media_type) <= 32
        AND octet_length(category) <= 128
        AND octet_length(resource_term) <= 1024
        AND octet_length(poster_url) <= 4096
        AND octet_length(backdrop_url) <= 4096
    ),
    CONSTRAINT download_intent_success_inputs_vote_average_check CHECK (
        vote_average <> 'NaN'::double precision
        AND vote_average >= 0
        AND vote_average <= 10
    ),
    CONSTRAINT download_intent_success_inputs_episodes_check CHECK (
        jsonb_typeof(episodes)='array'
        AND jsonb_array_length(episodes) <= 10000
        AND octet_length(episodes::text) <= 1048576
        AND position(E'\\u0000' in episodes::text)=0
    )
);

-- Every plan which predates input capture gets an explicit legacy v0 row.
-- Application-created plans after this migration must insert non-legacy v1 in
-- the same transaction as download_intent_plans.
INSERT INTO download_intent_success_inputs (
    intent_id,
    version,
    legacy,
    username,
    manual,
    background,
    media_source,
    media_id,
    title_year,
    media_type,
    category,
    vote_average,
    resource_term,
    poster_url,
    backdrop_url,
    episodes
)
SELECT
    intent_id,
    0,
    true,
    '',
    false,
    false,
    '',
    '',
    '',
    '',
    '',
    0,
    '',
    '',
    '',
    '[]'::jsonb
FROM download_intent_plans;

-- The composite key lets PostgreSQL enforce that every immutable snapshot
-- belongs to the same intent as its organization batch.
ALTER TABLE organization_batches
    ADD CONSTRAINT organization_batches_id_intent_id_key
    UNIQUE (id, intent_id);

CREATE TABLE organization_success_snapshots (
    batch_id UUID NOT NULL,
    group_key TEXT NOT NULL,
    intent_id UUID NOT NULL
        REFERENCES download_intent_success_inputs(intent_id) ON DELETE RESTRICT,
    version SMALLINT NOT NULL,
    legacy BOOLEAN NOT NULL,
    username TEXT NOT NULL,
    need_notify BOOLEAN NOT NULL,
    manual BOOLEAN NOT NULL,
    background BOOLEAN NOT NULL,
    media_source TEXT NOT NULL,
    media_id TEXT NOT NULL,
    title_year TEXT NOT NULL,
    media_type TEXT NOT NULL,
    category TEXT NOT NULL,
    vote_average DOUBLE PRECISION NOT NULL,
    poster_url TEXT NOT NULL,
    backdrop_url TEXT NOT NULL,
    file_facts JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT organization_success_snapshots_pkey PRIMARY KEY (batch_id, group_key),
    CONSTRAINT organization_success_snapshots_batch_intent_fkey
        FOREIGN KEY (batch_id, intent_id)
        REFERENCES organization_batches(id, intent_id) ON DELETE CASCADE,
    CONSTRAINT organization_success_snapshots_group_key_check CHECK (
        octet_length(group_key) BETWEEN 1 AND 128
        AND btrim(group_key)=group_key
    ),
    CONSTRAINT organization_success_snapshots_version_check CHECK (
        (legacy AND version=0) OR (NOT legacy AND version=1)
    ),
    CONSTRAINT organization_success_snapshots_text_bounds_check CHECK (
        octet_length(username) <= 256
        AND octet_length(media_source) <= 32
        AND octet_length(media_id) <= 128
        AND octet_length(title_year) <= 512
        AND octet_length(media_type) <= 32
        AND octet_length(category) <= 128
        AND octet_length(poster_url) <= 4096
        AND octet_length(backdrop_url) <= 4096
    ),
    CONSTRAINT organization_success_snapshots_vote_average_check CHECK (
        vote_average <> 'NaN'::double precision
        AND vote_average >= 0
        AND vote_average <= 10
    ),
    CONSTRAINT organization_success_snapshots_file_facts_check CHECK (
        jsonb_typeof(file_facts)='array'
        AND jsonb_array_length(file_facts) <= 10000
        AND octet_length(file_facts::text) <= 4194304
        AND position(E'\\u0000' in file_facts::text)=0
    )
);

CREATE INDEX organization_success_snapshots_intent_idx
    ON organization_success_snapshots (intent_id, batch_id, group_key);

-- +goose StatementBegin
CREATE FUNCTION prevent_organization_success_snapshot_mutation()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    RAISE EXCEPTION 'organization success snapshots are immutable';
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER download_intent_success_inputs_immutable
    BEFORE UPDATE OR DELETE ON download_intent_success_inputs
    FOR EACH ROW
    EXECUTE FUNCTION prevent_organization_success_snapshot_mutation();

CREATE TRIGGER organization_success_snapshots_immutable
    BEFORE UPDATE OR DELETE ON organization_success_snapshots
    FOR EACH ROW
    EXECUTE FUNCTION prevent_organization_success_snapshot_mutation();

-- +goose Down

DROP TRIGGER IF EXISTS organization_success_snapshots_immutable
    ON organization_success_snapshots;
DROP TRIGGER IF EXISTS download_intent_success_inputs_immutable
    ON download_intent_success_inputs;
DROP FUNCTION IF EXISTS prevent_organization_success_snapshot_mutation();
DROP TABLE organization_success_snapshots;
ALTER TABLE organization_batches
    DROP CONSTRAINT organization_batches_id_intent_id_key;
DROP TABLE download_intent_success_inputs;

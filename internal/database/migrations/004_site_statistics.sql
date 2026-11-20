-- +goose Up
CREATE TABLE IF NOT EXISTS site_statistics (
    domain TEXT PRIMARY KEY,
    success BIGINT NOT NULL DEFAULT 0 CHECK (success >= 0),
    fail BIGINT NOT NULL DEFAULT 0 CHECK (fail >= 0),
    seconds BIGINT NOT NULL DEFAULT 0 CHECK (seconds >= 0),
    lst_state SMALLINT NOT NULL DEFAULT 0 CHECK (lst_state IN (0, 1)),
    lst_mod_date TEXT NOT NULL DEFAULT '',
    note JSONB NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(note) = 'object'),
    CHECK (btrim(domain) <> '')
);

-- +goose Down
DROP TABLE IF EXISTS site_statistics;

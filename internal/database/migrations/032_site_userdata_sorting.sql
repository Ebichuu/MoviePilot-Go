-- +goose Up
CREATE TABLE IF NOT EXISTS site_userdata (
    id BIGSERIAL PRIMARY KEY,
    domain TEXT NOT NULL CHECK (btrim(domain) <> ''),
    name TEXT NOT NULL CHECK (btrim(name) <> ''),
    upload DOUBLE PRECISION DEFAULT 0,
    err_msg TEXT,
    updated_day TEXT NOT NULL DEFAULT to_char(CURRENT_TIMESTAMP, 'YYYY-MM-DD'),
    updated_time TEXT NOT NULL DEFAULT to_char(CURRENT_TIMESTAMP, 'HH24:MI:SS')
);

CREATE INDEX IF NOT EXISTS site_userdata_updated_day_id_idx
    ON site_userdata (updated_day, id);
CREATE INDEX IF NOT EXISTS site_userdata_domain_updated_day_updated_time_idx
    ON site_userdata (domain, updated_day DESC, updated_time DESC, id DESC);

-- +goose Down
DROP TABLE IF EXISTS site_userdata;

-- +goose Up
ALTER TABLE sites ADD COLUMN IF NOT EXISTS domain TEXT NOT NULL DEFAULT '';

UPDATE sites
SET domain = lower(split_part(split_part(regexp_replace(url, '^[a-zA-Z][a-zA-Z0-9+.-]*://', ''), '/', 1), ':', 1))
WHERE domain = '';

CREATE UNIQUE INDEX IF NOT EXISTS sites_domain_unique_idx
    ON sites (domain) WHERE domain <> '';

-- +goose Down
DROP INDEX IF EXISTS sites_domain_unique_idx;
ALTER TABLE sites DROP COLUMN IF EXISTS domain;

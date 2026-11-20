-- +goose Up
-- V2 `mediaserveritem` sync cache. The movie fast path reads one bare `item_id`
-- from here (no server condition, no ordering) and then broadcasts it across
-- the configured media servers, so the table is the fact source and River only
-- wakes the sync task.
CREATE TABLE media_server_items (
    id BIGSERIAL PRIMARY KEY,
    server TEXT NOT NULL,
    library TEXT,
    item_id TEXT NOT NULL,
    item_type TEXT,
    title TEXT,
    original_title TEXT,
    year TEXT,
    tmdbid BIGINT,
    imdbid TEXT,
    tvdbid TEXT,
    path TEXT,
    seasoninfo JSONB NOT NULL DEFAULT '{}'::jsonb,
    lst_mod_date TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT ux_media_server_items_server_item_id UNIQUE (server, item_id)
);

-- V2 declares the same composite index for `tmdbid + item_type` and a title
-- index; both lookups mirror V2's un-ordered `.first()`.
CREATE INDEX ix_media_server_items_tmdbid_item_type
    ON media_server_items (tmdbid, item_type);

CREATE INDEX ix_media_server_items_title
    ON media_server_items (title);

-- +goose Down
DROP TABLE IF EXISTS media_server_items;

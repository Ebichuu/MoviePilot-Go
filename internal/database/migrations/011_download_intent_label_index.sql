-- +goose Up
CREATE UNIQUE INDEX IF NOT EXISTS download_intents_label_uidx
    ON download_intents (label);

-- +goose Down
DROP INDEX IF EXISTS download_intents_label_uidx;

-- +goose Up
ALTER TABLE subscriptions
    ALTER COLUMN v2_state TYPE TEXT USING rtrim(v2_state),
    ALTER COLUMN v2_state SET NOT NULL;

ALTER TABLE subscriptions
    DROP CONSTRAINT IF EXISTS subscriptions_v2_state_check;

ALTER TABLE subscriptions
    ADD CONSTRAINT subscriptions_v2_state_check
    CHECK (v2_state IN ('', 'N', 'R', 'P', 'S'));

-- +goose Down
ALTER TABLE subscriptions
    DROP CONSTRAINT IF EXISTS subscriptions_v2_state_check;

ALTER TABLE subscriptions
    ALTER COLUMN v2_state TYPE CHAR(1) USING v2_state::CHAR(1),
    ALTER COLUMN v2_state SET NOT NULL;

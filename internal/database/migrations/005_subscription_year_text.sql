-- +goose Up
ALTER TABLE subscriptions
    ALTER COLUMN year TYPE TEXT USING year::text;

-- +goose Down
-- 非数字年份会让回滚失败，避免把 V2 合法字符串年份静默转换为 NULL。
ALTER TABLE subscriptions
    ALTER COLUMN year TYPE INTEGER USING NULLIF(btrim(year), '')::integer;

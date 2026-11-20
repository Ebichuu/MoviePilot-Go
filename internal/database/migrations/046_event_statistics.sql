-- +goose Up

-- 事件统计：把「下载完成 / 整理完成 / 订阅完成」等域事件按天计数，
-- 供仪表盘展示趋势，同时给后续插件/外部集成一个稳定的读模型。
-- 粒度：day + event_type + username，重复事件只累加计数。

CREATE TABLE IF NOT EXISTS event_statistics (
    day        date        NOT NULL,
    event_type text        NOT NULL,
    username   text        NOT NULL DEFAULT '',
    count      integer     NOT NULL DEFAULT 0,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (day, event_type, username)
);

CREATE INDEX IF NOT EXISTS event_statistics_type_day_idx
    ON event_statistics (event_type, day DESC);

-- +goose Down

DROP TABLE IF EXISTS event_statistics;

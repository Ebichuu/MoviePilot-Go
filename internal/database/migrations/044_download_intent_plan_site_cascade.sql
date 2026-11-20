-- +goose Up

-- 下载计划依附于站点，站点被重置/删除时计划必须一起消失，否则
-- `GET /site/reset` 会被外键拦成 500（V2 的重置语义是整个站点链清空）。
ALTER TABLE download_intent_plans
    DROP CONSTRAINT IF EXISTS download_intent_plans_site_id_fkey;

ALTER TABLE download_intent_plans
    ADD CONSTRAINT download_intent_plans_site_id_fkey
    FOREIGN KEY (site_id) REFERENCES sites(id) ON DELETE CASCADE;

-- +goose Down

ALTER TABLE download_intent_plans
    DROP CONSTRAINT IF EXISTS download_intent_plans_site_id_fkey;

ALTER TABLE download_intent_plans
    ADD CONSTRAINT download_intent_plans_site_id_fkey
    FOREIGN KEY (site_id) REFERENCES sites(id);

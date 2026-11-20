-- +goose Up

-- 站点重置（GET /site/reset，等价 V2 Site.reset）需要连带清空下载计划与成功快照，
-- 但正常业务路径上这些快照必须保持不可变。这里保留不可变语义，只允许在
-- 显式开启 moviepilot.allow_snapshot_purge 的事务里删除快照。

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION prevent_organization_success_snapshot_mutation()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF TG_OP = 'DELETE' AND current_setting('moviepilot.allow_snapshot_purge', true) = 'on' THEN
        RETURN OLD;
    END IF;
    RAISE EXCEPTION 'organization success snapshots are immutable';
END;
$$;
-- +goose StatementEnd

ALTER TABLE organization_success_snapshots
    DROP CONSTRAINT IF EXISTS organization_success_snapshots_intent_id_fkey;

ALTER TABLE organization_success_snapshots
    ADD CONSTRAINT organization_success_snapshots_intent_id_fkey
    FOREIGN KEY (intent_id) REFERENCES download_intent_success_inputs(intent_id) ON DELETE CASCADE;

-- +goose Down

ALTER TABLE organization_success_snapshots
    DROP CONSTRAINT IF EXISTS organization_success_snapshots_intent_id_fkey;

ALTER TABLE organization_success_snapshots
    ADD CONSTRAINT organization_success_snapshots_intent_id_fkey
    FOREIGN KEY (intent_id) REFERENCES download_intent_success_inputs(intent_id);

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION prevent_organization_success_snapshot_mutation()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    RAISE EXCEPTION 'organization success snapshots are immutable';
END;
$$;
-- +goose StatementEnd

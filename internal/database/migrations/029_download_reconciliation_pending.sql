-- +goose Up
ALTER TABLE download_intents
    ADD COLUMN organization_reconciliation_pending BOOLEAN NOT NULL DEFAULT TRUE;

UPDATE download_intents AS di
SET organization_reconciliation_pending=FALSE
WHERE EXISTS (
    SELECT 1
    FROM organization_batches AS b
    WHERE b.intent_id=di.id
);

-- +goose StatementBegin
CREATE FUNCTION maintain_download_intent_organization_reconciliation_pending()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF TG_OP='INSERT' THEN
        UPDATE download_intents
        SET organization_reconciliation_pending=FALSE
        WHERE id=NEW.intent_id;
        RETURN NEW;
    ELSIF TG_OP='DELETE' THEN
        UPDATE download_intents
        SET organization_reconciliation_pending=TRUE
        WHERE id=OLD.intent_id;
        RETURN OLD;
    END IF;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER organization_batches_reconciliation_pending
    AFTER INSERT OR DELETE ON organization_batches
    FOR EACH ROW
    EXECUTE FUNCTION maintain_download_intent_organization_reconciliation_pending();

-- +goose Down
DROP TRIGGER IF EXISTS organization_batches_reconciliation_pending
    ON organization_batches;
DROP FUNCTION IF EXISTS maintain_download_intent_organization_reconciliation_pending();
ALTER TABLE download_intents
    DROP COLUMN organization_reconciliation_pending;

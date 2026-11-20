-- +goose Up

ALTER TABLE scrape_requests
    DROP CONSTRAINT IF EXISTS scrape_requests_identity_check;

ALTER TABLE scrape_requests
    ADD CONSTRAINT scrape_requests_identity_check
    CHECK (
        season BETWEEN 0 AND 999
        AND episode BETWEEN 0 AND 5000
        AND octet_length(skip_reason) <= 128
        AND (
            (state='skipped' AND tmdb_id=0 AND skip_reason<>'')
            OR
            (state<>'skipped' AND skip_reason='' AND tmdb_id>0 AND kind<>'unknown')
        )
        AND (
            kind IN ('movie', 'tvshow', 'unknown')
            OR (kind='episode' AND (episode>0 OR state='skipped'))
        )
    );

-- +goose Down

ALTER TABLE scrape_requests DISABLE TRIGGER scrape_request_plan_immutable;

UPDATE scrape_requests
SET episode=1, updated_at=now()
WHERE kind='episode' AND state='skipped' AND episode=0;

ALTER TABLE scrape_requests ENABLE TRIGGER scrape_request_plan_immutable;

ALTER TABLE scrape_requests
    DROP CONSTRAINT IF EXISTS scrape_requests_identity_check;

ALTER TABLE scrape_requests
    ADD CONSTRAINT scrape_requests_identity_check
    CHECK (
        season BETWEEN 0 AND 999
        AND episode BETWEEN 0 AND 5000
        AND octet_length(skip_reason) <= 128
        AND (
            (state='skipped' AND tmdb_id=0 AND skip_reason<>'')
            OR
            (state<>'skipped' AND skip_reason='' AND tmdb_id>0 AND kind<>'unknown')
        )
        AND (
            kind IN ('movie', 'tvshow', 'unknown')
            OR (kind='episode' AND episode>0)
        )
    );

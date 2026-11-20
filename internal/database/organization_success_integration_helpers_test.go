//go:build integration

package database

import (
	"testing"

	"github.com/google/uuid"

	"github.com/moviepilot-go/moviepilot-go/internal/organization"
	"github.com/moviepilot-go/moviepilot-go/internal/organizer"
)

func organizationLegacySuccessSnapshotsIntegration(
	t *testing.T,
	store *Store,
	username string,
	intentID uuid.UUID,
	plan organizer.Plan,
) []organization.SuccessSnapshot {
	t.Helper()
	var siteID int64
	if err := store.db.Pool.QueryRow(t.Context(), `
		INSERT INTO sites (name, domain, url, adapter, enabled, config)
		VALUES ('organization-success-fixture', 'organization-success.invalid',
			'https://organization-success.invalid', 'declarative', TRUE, '{}'::jsonb)
		ON CONFLICT (name) DO UPDATE SET updated_at=sites.updated_at
		RETURNING id`).Scan(&siteID); err != nil {
		t.Fatalf("create organization success fixture site: %v", err)
	}
	if _, err := store.db.Pool.Exec(t.Context(), `
		INSERT INTO download_intent_plans (
			intent_id, site_id, downloader_id, resolver_kind, resolver_version,
			resource_id, resource_secret, save_path, paused
		) VALUES ($1,$2,$3,'legacy',1,$4,'','',FALSE)
		ON CONFLICT (intent_id) DO NOTHING`,
		intentID, siteID, uuid.NewString(), "organization-"+intentID.String()); err != nil {
		t.Fatalf("create organization success fixture download plan: %v", err)
	}
	if _, err := store.db.Pool.Exec(t.Context(), `
		INSERT INTO download_intent_success_inputs (
			intent_id, version, legacy, username, manual, background,
			media_source, media_id, title_year, media_type, category,
			vote_average, resource_term, poster_url, backdrop_url, episodes
		) VALUES ($1,0,TRUE,'',FALSE,FALSE,'','','','','',0,'','','','[]'::jsonb)
		ON CONFLICT (intent_id) DO NOTHING`, intentID); err != nil {
		t.Fatalf("create organization legacy success input: %v", err)
	}

	groupKey, err := organization.BuildGroupKey(organization.MediaContext{}, 0)
	if err != nil {
		t.Fatalf("build organization legacy group key: %v", err)
	}
	files := make([]organization.FileFact, len(plan.Operations))
	for ordinal, operation := range plan.Operations {
		files[ordinal] = organization.FileFact{
			Ordinal: ordinal, Role: "video", SizeBytes: operation.ExpectedSize, Season: 0,
		}
	}
	return []organization.SuccessSnapshot{{
		BatchID: uuid.NewSHA1(intentID, []byte(plan.ID)), IntentID: intentID,
		GroupKey: groupKey, Version: 0, Legacy: true, Username: username,
		NeedNotify: true, Files: files,
	}}
}

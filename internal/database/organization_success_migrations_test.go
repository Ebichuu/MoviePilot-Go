package database

import (
	"strings"
	"testing"
)

func TestNotificationEventGroupsMigrationIsLossless(t *testing.T) {
	t.Parallel()

	up, down := organizationSuccessMigrationSections(t, "migrations/026_notification_event_groups.sql")
	for _, fragment := range []string{
		"ALTER TABLE notification_events DROP CONSTRAINT notification_events_batch_id_key",
		"CREATE INDEX notification_events_batch_idx ON notification_events (batch_id, id) WHERE batch_id IS NOT NULL",
	} {
		if !strings.Contains(up, fragment) {
			t.Fatalf("026 Up missing %q", fragment)
		}
	}
	if strings.Contains(up, "CREATE UNIQUE INDEX notification_events_batch_idx") {
		t.Fatal("026 Up must permit multiple events in one batch")
	}
	for _, fragment := range []string{
		"GROUP BY batch_id HAVING count(*) > 1",
		"RAISE EXCEPTION 'cannot restore one notification event per batch while grouped events exist'",
		"DROP INDEX notification_events_batch_idx",
		"ADD CONSTRAINT notification_events_batch_id_key UNIQUE (batch_id)",
	} {
		if !strings.Contains(down, fragment) {
			t.Fatalf("026 Down missing lossless rollback guard %q", fragment)
		}
	}
	for _, destructive := range []string{"DELETE FROM notification_events", "DISTINCT ON", "min(id)", "max(id)"} {
		if strings.Contains(strings.ToLower(down), strings.ToLower(destructive)) {
			t.Fatalf("026 Down contains destructive reconciliation %q", destructive)
		}
	}
}

func TestOrganizationSuccessSnapshotsMigrationBoundsImmutableFacts(t *testing.T) {
	t.Parallel()

	up, down := organizationSuccessMigrationSections(t, "migrations/027_organization_success_snapshots.sql")
	for _, fragment := range []string{
		"CREATE TABLE download_intent_success_inputs",
		"intent_id UUID PRIMARY KEY REFERENCES download_intent_plans(intent_id) ON DELETE CASCADE",
		"version SMALLINT NOT NULL",
		"legacy BOOLEAN NOT NULL",
		"username TEXT NOT NULL",
		"manual BOOLEAN NOT NULL",
		"background BOOLEAN NOT NULL",
		"media_source TEXT NOT NULL",
		"media_id TEXT NOT NULL",
		"title_year TEXT NOT NULL",
		"media_type TEXT NOT NULL",
		"category TEXT NOT NULL",
		"vote_average DOUBLE PRECISION NOT NULL",
		"resource_term TEXT NOT NULL",
		"poster_url TEXT NOT NULL",
		"backdrop_url TEXT NOT NULL",
		"episodes JSONB NOT NULL",
		"CONSTRAINT download_intent_success_inputs_version_check CHECK ( (legacy AND version=0) OR (NOT legacy AND version=1) )",
		"octet_length(username) <= 256",
		"octet_length(resource_term) <= 1024",
		"octet_length(poster_url) <= 4096",
		"vote_average <> 'NaN'::double precision",
		"jsonb_typeof(episodes)='array'",
		"jsonb_array_length(episodes) <= 10000",
		"octet_length(episodes::text) <= 1048576",
		"position(E'\\\\u0000' in episodes::text)=0",
		"INSERT INTO download_intent_success_inputs",
		"SELECT intent_id, 0, true, '', false, false",
		"'[]'::jsonb FROM download_intent_plans",
		"ALTER TABLE organization_batches ADD CONSTRAINT organization_batches_id_intent_id_key UNIQUE (id, intent_id)",
		"CREATE TABLE organization_success_snapshots",
		"CONSTRAINT organization_success_snapshots_pkey PRIMARY KEY (batch_id, group_key)",
		"need_notify BOOLEAN NOT NULL",
		"octet_length(group_key) BETWEEN 1 AND 128",
		"btrim(group_key)=group_key",
		"REFERENCES download_intent_success_inputs(intent_id) ON DELETE RESTRICT",
		"FOREIGN KEY (batch_id, intent_id) REFERENCES organization_batches(id, intent_id) ON DELETE CASCADE",
		"jsonb_typeof(file_facts)='array'",
		"jsonb_array_length(file_facts) <= 10000",
		"octet_length(file_facts::text) <= 4194304",
		"position(E'\\\\u0000' in file_facts::text)=0",
		"BEFORE UPDATE OR DELETE ON download_intent_success_inputs",
		"BEFORE UPDATE OR DELETE ON organization_success_snapshots",
		"EXECUTE FUNCTION prevent_organization_success_snapshot_mutation()",
	} {
		if !strings.Contains(up, fragment) {
			t.Fatalf("027 Up missing %q", fragment)
		}
	}
	if strings.Contains(up, "snapshot JSONB") {
		t.Fatal("027 must use explicit scalar columns instead of an untyped snapshot JSONB contract")
	}
	for _, sharedColumn := range []string{
		"version SMALLINT NOT NULL",
		"legacy BOOLEAN NOT NULL",
		"username TEXT NOT NULL",
		"manual BOOLEAN NOT NULL",
		"background BOOLEAN NOT NULL",
		"media_source TEXT NOT NULL",
		"media_id TEXT NOT NULL",
		"title_year TEXT NOT NULL",
		"media_type TEXT NOT NULL",
		"category TEXT NOT NULL",
		"vote_average DOUBLE PRECISION NOT NULL",
		"poster_url TEXT NOT NULL",
		"backdrop_url TEXT NOT NULL",
	} {
		if count := strings.Count(up, sharedColumn); count != 2 {
			t.Fatalf("027 must copy shared field %q into input and snapshot tables, got %d columns", sharedColumn, count)
		}
	}
	if count := strings.Count(up, " JSONB NOT NULL"); count != 2 {
		t.Fatalf("027 must persist only bounded episodes and file_facts JSONB collections, got %d JSONB columns", count)
	}
	for _, forbidden := range []string{
		"source_path", "target_path", "download_url", "resource_secret", "site_cookie", "passkey", "chr(0)",
	} {
		if strings.Contains(strings.ToLower(up), forbidden) {
			t.Fatalf("027 persists forbidden path/secret primitive %q", forbidden)
		}
	}
	for _, fragment := range []string{
		"DROP TRIGGER IF EXISTS organization_success_snapshots_immutable ON organization_success_snapshots",
		"DROP TRIGGER IF EXISTS download_intent_success_inputs_immutable ON download_intent_success_inputs",
		"DROP FUNCTION IF EXISTS prevent_organization_success_snapshot_mutation()",
		"DROP TABLE organization_success_snapshots",
		"ALTER TABLE organization_batches DROP CONSTRAINT organization_batches_id_intent_id_key",
		"DROP TABLE download_intent_success_inputs",
	} {
		if !strings.Contains(down, fragment) {
			t.Fatalf("027 Down missing %q", fragment)
		}
	}
}

func organizationSuccessMigrationSections(t *testing.T, name string) (string, string) {
	t.Helper()
	content, err := migrationFS.ReadFile(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	sections := strings.SplitN(string(content), "-- +goose Down", 2)
	if len(sections) != 2 {
		t.Fatalf("%s must contain one Down section", name)
	}
	return strings.Join(strings.Fields(sections[0]), " "), strings.Join(strings.Fields(sections[1]), " ")
}

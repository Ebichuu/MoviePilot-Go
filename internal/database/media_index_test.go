package database

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/moviepilot-go/moviepilot-go/internal/mediaindex"
)

func TestNormalizeMediaIndexCompletionPlan(t *testing.T) {
	valid := mediaIndexTestCompletionPlan()
	tests := []struct {
		name   string
		mutate func(*mediaindex.CompletionPlan)
	}{
		{name: "invalid server id", mutate: func(plan *mediaindex.CompletionPlan) { plan.RequiredServer.ID = "invalid" }},
		{name: "unsupported server", mutate: func(plan *mediaindex.CompletionPlan) { plan.RequiredServer.Type = "unknown" }},
		{name: "empty server fingerprint", mutate: func(plan *mediaindex.CompletionPlan) { plan.RequiredServer.Fingerprint = "" }},
		{name: "invalid server fingerprint", mutate: func(plan *mediaindex.CompletionPlan) { plan.RequiredServer.Fingerprint = strings.Repeat("A", 64) }},
		{name: "empty files", mutate: func(plan *mediaindex.CompletionPlan) { plan.Files = nil }},
		{name: "empty local path", mutate: func(plan *mediaindex.CompletionPlan) { plan.Files[0].LocalPath = "" }},
		{name: "empty remote path", mutate: func(plan *mediaindex.CompletionPlan) { plan.Files[0].RemotePath = "" }},
		{name: "invalid path style", mutate: func(plan *mediaindex.CompletionPlan) { plan.Files[0].PathStyle = "native" }},
		{name: "invalid fingerprint", mutate: func(plan *mediaindex.CompletionPlan) { plan.Files[0].ExpectedSHA256 = strings.Repeat("A", 64) }},
		{name: "empty media type", mutate: func(plan *mediaindex.CompletionPlan) { plan.Files[0].MediaType = "" }},
		{name: "duplicate normalized remote path", mutate: func(plan *mediaindex.CompletionPlan) {
			plan.Files = append(plan.Files, plan.Files[0])
			plan.Files[0].RemotePath = `D:\Media\Series\One.mkv`
			plan.Files[1].RemotePath = `d:/media/series/one.mkv/`
			plan.Files[1].CaseSensitive = false
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan := valid.Clone()
			tt.mutate(&plan)
			if _, _, err := normalizeMediaIndexCompletionPlan(plan); !errors.Is(err, mediaindex.ErrInvalidRequest) {
				t.Fatalf("normalizeMediaIndexCompletionPlan() error = %v, want ErrInvalidRequest", err)
			}
		})
	}

	normalized, serverID, err := normalizeMediaIndexCompletionPlan(valid)
	if err != nil {
		t.Fatalf("normalizeMediaIndexCompletionPlan(valid) error = %v", err)
	}
	if normalized.RequiredServer.ID != serverID.String() || normalized.RequiredServer.Type != "emby" ||
		normalized.RequiredServer.Fingerprint != valid.RequiredServer.Fingerprint {
		t.Fatalf("normalized server = %#v, id = %s", normalized.RequiredServer, serverID)
	}
}

func TestMatchMediaIndexFilesAllowsUnplannedSidecars(t *testing.T) {
	secondSHA256 := strings.Repeat("b", 64)
	stored := []mediaIndexOrganizationFile{
		{Ordinal: 0, TargetPath: `D:\Library\Series\One.mkv`, ExpectedSize: 5, ExpectedSHA256: organizationTestSHA256},
		{Ordinal: 1, TargetPath: `D:\Library\Series\One.nfo`, ExpectedSize: 2, ExpectedSHA256: strings.Repeat("c", 64)},
		{Ordinal: 2, TargetPath: `D:\Library\Series\Two.mkv`, ExpectedSize: 8, ExpectedSHA256: secondSHA256},
	}
	planned := []mediaindex.TargetFile{
		{LocalPath: stored[2].TargetPath, ExpectedSize: stored[2].ExpectedSize, ExpectedSHA256: stored[2].ExpectedSHA256},
		{LocalPath: stored[0].TargetPath, ExpectedSize: stored[0].ExpectedSize, ExpectedSHA256: stored[0].ExpectedSHA256},
	}
	if err := matchMediaIndexFiles(stored, planned); err != nil {
		t.Fatalf("matchMediaIndexFiles() error = %v", err)
	}
}

func TestMatchMediaIndexFilesRequiresUniqueFingerprintMatch(t *testing.T) {
	target := mediaIndexOrganizationFile{
		TargetPath:     `D:\Library\Series\One.mkv`,
		ExpectedSize:   5,
		ExpectedSHA256: organizationTestSHA256,
	}
	planned := []mediaindex.TargetFile{{
		LocalPath:      target.TargetPath,
		ExpectedSize:   target.ExpectedSize,
		ExpectedSHA256: target.ExpectedSHA256,
	}}
	tests := []struct {
		name   string
		stored []mediaIndexOrganizationFile
	}{
		{name: "missing", stored: []mediaIndexOrganizationFile{{
			TargetPath: target.TargetPath, ExpectedSize: target.ExpectedSize + 1, ExpectedSHA256: target.ExpectedSHA256,
		}}},
		{name: "ambiguous", stored: []mediaIndexOrganizationFile{target, target}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := matchMediaIndexFiles(tt.stored, planned); !errors.Is(err, ErrOrganizationStateConflict) {
				t.Fatalf("matchMediaIndexFiles() error = %v, want ErrOrganizationStateConflict", err)
			}
		})
	}
}

func TestMediaIndexStoreRejectsInvalidCalls(t *testing.T) {
	var store *Store
	requestID := uuid.New()
	token := uuid.New()
	now := time.Now().UTC()
	tests := []struct {
		name string
		call func() error
	}{
		{name: "claim", call: func() error { _, _, err := store.Claim(context.Background(), requestID, now, time.Minute); return err }},
		{name: "renew", call: func() error {
			return store.Renew(context.Background(), requestID, token, now, now.Add(time.Minute))
		}},
		{name: "legacy renew", call: func() error {
			return store.RenewMediaIndexLease(context.Background(), requestID, token, now, time.Minute)
		}},
		{name: "reschedule", call: func() error { return store.Reschedule(context.Background(), requestID, token, now, "retry") }},
		{name: "record refresh", call: func() error {
			return store.RecordRefresh(context.Background(), requestID, token, now, now.Add(time.Minute))
		}},
		{name: "list files", call: func() error { _, err := store.ListPendingFiles(context.Background(), requestID, token, 1); return err }},
		{name: "confirm file", call: func() error { return store.ConfirmFile(context.Background(), requestID, token, 1, "item") }},
		{name: "confirm request", call: func() error { _, err := store.ConfirmRequest(context.Background(), requestID, token, now); return err }},
		{name: "recover", call: func() error { _, err := store.RecoverExpired(context.Background(), now, 1); return err }},
		{name: "list due", call: func() error { _, err := store.ListDue(context.Background(), now, 1); return err }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.call(); !errors.Is(err, mediaindex.ErrInvalidRequest) {
				t.Fatalf("error = %v, want ErrInvalidRequest", err)
			}
		})
	}
}

func TestMediaIndexMigrationContracts(t *testing.T) {
	content, err := migrationFS.ReadFile("migrations/022_media_index_requests.sql")
	if err != nil {
		t.Fatal(err)
	}
	migration := strings.Join(strings.Fields(string(content)), " ")
	for _, fragment := range []string{
		"'pending_index'",
		"CREATE TABLE media_index_requests",
		"CREATE TABLE media_index_files",
		"server_fingerprint TEXT NOT NULL",
		"expected_file_count INT NOT NULL",
		"CHECK (attempt_count >= 0)",
		"CHECK (expected_file_count BETWEEN 1 AND 10000)",
		"server_fingerprint ~ '^[0-9a-f]{64}$'",
		"media_index_requests_server_fingerprint_check",
		"state IN ('pending', 'working', 'failed', 'confirmed')",
		"state='working' AND lease_token IS NOT NULL AND lease_expires_at IS NOT NULL",
		"(state='confirmed') = (completed_at IS NOT NULL)",
		"UNIQUE (batch_id, server_id)",
		"UNIQUE (request_id, ordinal)",
		"media_index_request_plan_immutable",
		"media_index_file_plan_immutable",
		"NEW.server_fingerprint IS DISTINCT FROM OLD.server_fingerprint",
		"UPDATE subscription_episodes SET state='pending_organization'",
		"UPDATE subscriptions SET internal_state='pending_organization', v2_state='R'",
	} {
		if !strings.Contains(migration, fragment) {
			t.Fatalf("migration missing %q", fragment)
		}
	}
	if strings.Contains(migration, "attempt_count BETWEEN 0 AND 32") {
		t.Fatal("migration must not cap media index attempt_count")
	}
}

func TestMediaIndexRequestIDIsStablePerBatchAndServer(t *testing.T) {
	batchID := uuid.New()
	serverID := uuid.New()
	first := mediaIndexRequestID(batchID, serverID)
	if first == uuid.Nil || first != mediaIndexRequestID(batchID, serverID) {
		t.Fatalf("mediaIndexRequestID() is not stable: %s", first)
	}
	if first == mediaIndexRequestID(batchID, uuid.New()) {
		t.Fatal("different servers must not share a request ID")
	}
}

func mediaIndexTestCompletionPlan() mediaindex.CompletionPlan {
	return mediaindex.CompletionPlan{
		RequiredServer: &mediaindex.ServerRef{
			ID: uuid.NewString(), Name: "Primary Emby", Type: "Emby", Fingerprint: strings.Repeat("d", 64),
		},
		Files: []mediaindex.TargetFile{{
			LocalPath:      `D:\Library\Series\One.mkv`,
			RemotePath:     `D:\Media\Series\One.mkv`,
			PathStyle:      "windows",
			CaseSensitive:  false,
			ExpectedSize:   5,
			ExpectedSHA256: organizationTestSHA256,
			MediaType:      "Episode",
			Season:         1,
			Episode:        1,
			ProviderID:     "tmdb:1",
		}},
	}
}

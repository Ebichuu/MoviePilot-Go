//go:build integration

package database

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/moviepilot-go/moviepilot-go/internal/organization"
)

// TestListOrganizationFileOutcomesIntegration 覆盖整理批次终态投影：完成/失败映射、
// 归属隔离、未终态与非法入参。
func TestListOrganizationFileOutcomesIntegration(t *testing.T) {
	store := newDownloadIntentPlanIntegrationStore(t)
	const username = "file-outcomes-owner"
	insertDownloadIntentIntegrationUsers(t, store, username, "file-outcomes-other")
	ctx := t.Context()

	intent := mustDatabaseDownloadIntent(t, username, "file-outcomes-resource")
	if _, err := store.CreateDownloadIntent(ctx, username, intent); err != nil {
		t.Fatalf("CreateDownloadIntent() error = %v", err)
	}

	if _, err := store.ListOrganizationFileOutcomes(ctx, "", uuid.New()); !errors.Is(err, ErrInvalidOrganizationInput) {
		t.Fatalf("空 username error = %v", err)
	}
	if _, err := store.ListOrganizationFileOutcomes(ctx, username, uuid.Nil); !errors.Is(err, ErrInvalidOrganizationInput) {
		t.Fatalf("空 batchID error = %v", err)
	}

	batchID := insertOrganizationOutcomeBatchIntegration(t, store, username, intent.ID, "completed",
		[]organizationOutcomeFileIntegration{
			{Ordinal: 0, State: "completed", Source: "/downloads/a.mkv", Target: "/library/a.mkv"},
			{Ordinal: 1, State: "terminal_failed", Source: "/downloads/b.mkv", Target: "/library/b.mkv", Error: "perms"},
		})

	outcomes, err := store.ListOrganizationFileOutcomes(ctx, " "+username+" ", batchID)
	if err != nil {
		t.Fatalf("ListOrganizationFileOutcomes() error = %v", err)
	}
	if len(outcomes) != 2 {
		t.Fatalf("结果条数 = %d, want 2", len(outcomes))
	}
	if outcomes[0].Ordinal != 0 || outcomes[0].State != organization.FileOutcomeCompleted || outcomes[0].Error != "" {
		t.Fatalf("完成项 = %#v", outcomes[0])
	}
	if outcomes[1].Ordinal != 1 || outcomes[1].State != organization.FileOutcomeTerminalFailed || outcomes[1].Error != "perms" {
		t.Fatalf("失败项 = %#v", outcomes[1])
	}

	if _, err := store.ListOrganizationFileOutcomes(ctx, "file-outcomes-other", batchID); !errors.Is(err, ErrOrganizationFilesIncomplete) {
		t.Fatalf("他人查批次 error = %v", err)
	}

	// 仍有 planned 行时视为未终态。
	plannedIntent := mustDatabaseDownloadIntent(t, username, "file-outcomes-planned")
	if _, err := store.CreateDownloadIntent(ctx, username, plannedIntent); err != nil {
		t.Fatalf("CreateDownloadIntent(planned) error = %v", err)
	}
	plannedBatch := insertOrganizationOutcomeBatchIntegration(t, store, username, plannedIntent.ID, "planned",
		[]organizationOutcomeFileIntegration{
			{Ordinal: 0, State: "planned", Source: "/downloads/c.mkv", Target: "/library/c.mkv"},
		})
	if _, err := store.ListOrganizationFileOutcomes(ctx, username, plannedBatch); !errors.Is(err, ErrOrganizationFilesIncomplete) {
		t.Fatalf("未终态批次 error = %v", err)
	}

	// 负序号属于持久化损坏，应报 ErrOrganizationPersistence。
	brokenIntent := mustDatabaseDownloadIntent(t, username, "file-outcomes-broken")
	if _, err := store.CreateDownloadIntent(ctx, username, brokenIntent); err != nil {
		t.Fatalf("CreateDownloadIntent(broken) error = %v", err)
	}
	brokenBatch := insertOrganizationOutcomeBatchIntegration(t, store, username, brokenIntent.ID, "completed",
		[]organizationOutcomeFileIntegration{
			{Ordinal: -1, State: "completed", Source: "/downloads/d.mkv", Target: "/library/d.mkv"},
		})
	if _, err := store.ListOrganizationFileOutcomes(ctx, username, brokenBatch); !errors.Is(err, ErrOrganizationPersistence) {
		t.Fatalf("负序号批次 error = %v", err)
	}
}

type organizationOutcomeFileIntegration struct {
	Ordinal int
	State   string
	Source  string
	Target  string
	Error   string
}

// insertOrganizationOutcomeBatchIntegration 直接落一个批次与若干文件行，
// 因为本测试只关心终态投影，不需要走组织执行流程。
func insertOrganizationOutcomeBatchIntegration(
	t *testing.T,
	store *Store,
	username string,
	intentID uuid.UUID,
	batchState string,
	files []organizationOutcomeFileIntegration,
) uuid.UUID {
	t.Helper()
	ctx := t.Context()
	batchID := uuid.New()
	if _, err := store.db.Pool.Exec(ctx, `
		INSERT INTO organization_batches (
			id, intent_id, state, source_root, target_root, plan, last_error, completed_at
		) VALUES (
			$1, $2, $3, '/downloads', '/library',
			(SELECT jsonb_agg(jsonb_build_object(
				'source_path', value->>'source', 'target_path', value->>'target', 'mode', 'copy'
			)) FROM jsonb_array_elements($4::jsonb) AS value),
			'', CASE WHEN $3='completed' THEN now() ELSE NULL END
		)`, batchID, intentID, batchState, outcomeFilesJSONIntegration(t, files)); err != nil {
		t.Fatalf("插入整理批次: %v", err)
	}
	for _, file := range files {
		if _, err := store.db.Pool.Exec(ctx, `
			INSERT INTO organization_files (
				batch_id, source_path, target_path, state, ordinal, mode, last_error, completed_at
			) VALUES (
				$1, $2, $3, $4, $5, 'copy', $6,
				CASE WHEN $4 IN ('completed', 'terminal_failed') THEN now() ELSE NULL END
			)`, batchID, file.Source, file.Target, file.State, file.Ordinal, file.Error); err != nil {
			t.Fatalf("插入整理文件: %v", err)
		}
	}
	return batchID
}

func outcomeFilesJSONIntegration(t *testing.T, files []organizationOutcomeFileIntegration) string {
	t.Helper()
	entries := make([]map[string]string, 0, len(files))
	for _, file := range files {
		entries = append(entries, map[string]string{"source": file.Source, "target": file.Target})
	}
	encoded, err := json.Marshal(entries)
	if err != nil {
		t.Fatalf("编码计划: %v", err)
	}
	return string(encoded)
}

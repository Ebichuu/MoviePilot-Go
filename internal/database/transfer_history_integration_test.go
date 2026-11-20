//go:build integration

package database

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/moviepilot-go/moviepilot-go/internal/organization"
	"github.com/moviepilot-go/moviepilot-go/internal/organizer"
)

// TestTransferHistoryStoreIntegration 用真实的整理批次/快照验证只读历史投影：
// 列表、按用户/标题过滤、总数以及按天/按月统计。
func TestTransferHistoryStoreIntegration(t *testing.T) {
	store := newDownloadIntentPlanIntegrationStore(t)
	const username = "transfer-history-owner"
	insertDownloadIntentIntegrationUsers(t, store, username, "transfer-history-other")
	siteID := insertDownloadIntentPlanIntegrationSite(t, store, "transfer-history-site")

	intent := mustDatabaseDownloadIntent(t, username, "transfer-history-resource")
	if _, err := store.CreateDownloadIntent(t.Context(), username, intent); err != nil {
		t.Fatalf("create download intent: %v", err)
	}
	plan := downloadIntentPlanIntegrationFixture(intent.ID, siteID, "transfer-history-resource")
	if _, err := store.CreateDownloadIntentPlan(t.Context(), username, plan); err != nil {
		t.Fatalf("create download plan: %v", err)
	}
	if _, err := store.db.Pool.Exec(t.Context(), `
		UPDATE download_intents SET state='completed', torrent_hash=$2 WHERE id=$1`,
		intent.ID, strings.Repeat("b", 40)); err != nil {
		t.Fatalf("complete download intent fixture: %v", err)
	}

	input := organization.SuccessInput{
		IntentID: intent.ID, Version: organization.CurrentSuccessVersion,
		Username: username,
		Media: organization.MediaContext{
			Source: "tmdb", MediaID: "42", TitleYear: "History Example (2026)",
			MediaType: "电视剧", Category: "剧情",
		},
		ResourceTerm: "1080p WEB-DL",
		Episodes:     []organization.Episode{{Season: 1, Episode: 1}, {Season: 2, Episode: 1}},
	}
	createDownloadSuccessInputIntegration(t, store, username, input)

	root := t.TempDir()
	organizationPlan, err := organizer.BuildPlan(filepath.Join(root, "library"), []organizer.Operation{
		organizationTestOperation(filepath.Join(root, "downloads", "s01e01.mkv"), filepath.Join(root, "library", "history", "s01e01.mkv"), organizer.Copy),
		organizationTestOperation(filepath.Join(root, "downloads", "s02e01.mkv"), filepath.Join(root, "library", "history", "s02e01.mkv"), organizer.Copy),
	})
	if err != nil {
		t.Fatalf("BuildPlan() error = %v", err)
	}
	groupOne, err := organization.BuildGroupKey(input.Media, 1)
	if err != nil {
		t.Fatalf("BuildGroupKey(1) error = %v", err)
	}
	groupTwo, err := organization.BuildGroupKey(input.Media, 2)
	if err != nil {
		t.Fatalf("BuildGroupKey(2) error = %v", err)
	}
	batchID := uuid.NewSHA1(intent.ID, []byte(organizationPlan.ID))
	snapshots := []organization.SuccessSnapshot{
		{
			BatchID: batchID, IntentID: intent.ID, GroupKey: groupTwo,
			Version: input.Version, Username: username, NeedNotify: true, Media: input.Media,
			Files: []organization.FileFact{{
				Ordinal: 1, Role: "video", SizeBytes: organizationPlan.Operations[1].ExpectedSize,
				Season: 2, Episodes: []int{1}, ResourceTerm: input.ResourceTerm,
			}},
		},
		{
			BatchID: batchID, IntentID: intent.ID, GroupKey: groupOne,
			Version: input.Version, Username: username, NeedNotify: true, Media: input.Media,
			Files: []organization.FileFact{{
				Ordinal: 0, Role: "video", SizeBytes: organizationPlan.Operations[0].ExpectedSize,
				Season: 1, Episodes: []int{1}, ResourceTerm: input.ResourceTerm,
			}},
		},
	}
	if _, err := store.CreateOrganizationBatch(
		t.Context(), username, intent.ID, filepath.Join(root, "downloads"), organizationPlan, snapshots,
	); err != nil {
		t.Fatalf("CreateOrganizationBatch() error = %v", err)
	}
	createOrganizationSuccessSnapshotsIntegration(t, store, username, snapshots)

	entries, err := store.ListTransferHistory(t.Context(), "", "", 0, -5)
	if err != nil {
		t.Fatalf("ListTransferHistory() error = %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("ListTransferHistory() = %d 条, want 2", len(entries))
	}
	if entries[0].Username != username || entries[0].TitleYear != "History Example (2026)" ||
		entries[0].MediaType != "电视剧" {
		t.Fatalf("历史投影 = %#v", entries[0])
	}
	if len(entries[0].FileFacts) != 1 || entries[0].FileFacts[0].ResourceTerm != "1080p WEB-DL" {
		t.Fatalf("历史文件事实 = %#v", entries[0].FileFacts)
	}

	filtered, err := store.ListTransferHistory(t.Context(), username, "example", 10, 0)
	if err != nil || len(filtered) != 2 {
		t.Fatalf("按标题过滤 = (%d, %v)", len(filtered), err)
	}
	if missing, err := store.ListTransferHistory(t.Context(), username, "不存在的标题", 10, 0); err != nil || len(missing) != 0 {
		t.Fatalf("无匹配过滤 = (%d, %v)", len(missing), err)
	}
	if other, err := store.ListTransferHistory(t.Context(), "transfer-history-other", "", 10, 0); err != nil || len(other) != 0 {
		t.Fatalf("其他用户历史 = (%d, %v)", len(other), err)
	}

	total, err := store.CountTransferHistory(t.Context(), "", "")
	if err != nil || total != 2 {
		t.Fatalf("CountTransferHistory() = (%d, %v)", total, err)
	}
	if total, err := store.CountTransferHistory(t.Context(), "transfer-history-other", ""); err != nil || total != 0 {
		t.Fatalf("其他用户总数 = (%d, %v)", total, err)
	}

	byDay, err := store.CountTransferHistoryByDay(t.Context(), 3)
	if err != nil {
		t.Fatalf("CountTransferHistoryByDay() error = %v", err)
	}
	if len(byDay) != 3 || byDay[2] != 2 || byDay[0] != 0 || byDay[1] != 0 {
		t.Fatalf("CountTransferHistoryByDay(3) = %#v", byDay)
	}
	ignored, err := store.CountTransferHistoryByDay(t.Context(), 0)
	if err != nil || len(ignored) != 7 {
		t.Fatalf("非法 days 归一化 = (%d, %v)", len(ignored), err)
	}

	movies, shows, err := store.CountTransferHistoryMonth(t.Context())
	if err != nil || movies != 0 || shows != 2 {
		t.Fatalf("CountTransferHistoryMonth() = (%d, %d, %v)", movies, shows, err)
	}
}

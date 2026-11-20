//go:build integration

package database

import (
	"errors"
	"testing"
	"time"
)

// TestFailOrphanDownloadIntentsIntegration 覆盖「缺少提交计划的遗留意图」收尾：
// 只看 submitting/downloading、只处理超过保留期的行，带计划的意图不受影响。
func TestFailOrphanDownloadIntentsIntegration(t *testing.T) {
	store := newDownloadIntentPlanIntegrationStore(t)
	const username = "orphan-intent-owner"
	insertDownloadIntentIntegrationUsers(t, store, username)
	siteID := insertDownloadIntentPlanIntegrationSite(t, store, "orphan-intent-site")
	ctx := t.Context()

	orphan := mustDatabaseDownloadIntent(t, username, "orphan-intent-resource")
	if _, err := store.CreateDownloadIntent(ctx, username, orphan); err != nil {
		t.Fatalf("CreateDownloadIntent(orphan) error = %v", err)
	}
	if _, err := store.db.Pool.Exec(ctx, `
		UPDATE download_intents SET state='downloading', updated_at=now() - interval '3 hours'
		WHERE id=$1`, orphan.ID); err != nil {
		t.Fatalf("准备孤儿意图: %v", err)
	}

	planned := mustDatabaseDownloadIntent(t, username, "planned-intent-resource")
	if _, err := store.CreateDownloadIntent(ctx, username, planned); err != nil {
		t.Fatalf("CreateDownloadIntent(planned) error = %v", err)
	}
	if _, err := store.CreateDownloadIntentPlan(ctx, username,
		downloadIntentPlanIntegrationFixture(planned.ID, siteID, "planned-intent-resource")); err != nil {
		t.Fatalf("CreateDownloadIntentPlan() error = %v", err)
	}
	if _, err := store.db.Pool.Exec(ctx, `
		UPDATE download_intents SET state='downloading', updated_at=now() - interval '3 hours'
		WHERE id=$1`, planned.ID); err != nil {
		t.Fatalf("准备有计划意图: %v", err)
	}

	fresh := mustDatabaseDownloadIntent(t, username, "fresh-intent-resource")
	if _, err := store.CreateDownloadIntent(ctx, username, fresh); err != nil {
		t.Fatalf("CreateDownloadIntent(fresh) error = %v", err)
	}
	if _, err := store.db.Pool.Exec(ctx, `
		UPDATE download_intents SET state='downloading', updated_at=now()
		WHERE id=$1`, fresh.ID); err != nil {
		t.Fatalf("准备新意图: %v", err)
	}

	// 保留期一小时：只有 updated_at 早于窗口且无计划的孤儿意图被收尾。
	failed, err := store.FailOrphanDownloadIntents(ctx, time.Now().UTC().Add(-time.Hour))
	if err != nil {
		t.Fatalf("FailOrphanDownloadIntents() error = %v", err)
	}
	if failed != 1 {
		t.Fatalf("失败意图数 = %d, want 1", failed)
	}
	states := map[string]int{}
	rows, err := store.db.Pool.Query(ctx, `SELECT state, COALESCE(last_error, '') FROM download_intents`)
	if err != nil {
		t.Fatalf("读取意图状态: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var state, lastError string
		if err := rows.Scan(&state, &lastError); err != nil {
			t.Fatalf("扫描意图状态: %v", err)
		}
		if state == "failed" && lastError == "" {
			t.Fatal("失败意图缺少 last_error")
		}
		states[state]++
	}
	if states["failed"] != 1 || states["downloading"] != 2 {
		t.Fatalf("意图状态分布 = %#v", states)
	}

	// 再跑一次不会重复收尾。
	if failed, err := store.FailOrphanDownloadIntents(ctx, time.Now().UTC().Add(-time.Hour)); err != nil || failed != 0 {
		t.Fatalf("重复 FailOrphanDownloadIntents() = (%d, %v)", failed, err)
	}
	if _, err := store.FailOrphanDownloadIntents(ctx, time.Time{}); !errors.Is(err, ErrInvalidDownloadReconciliationQuery) {
		t.Fatalf("零值 olderThan error = %v", err)
	}
}

// TestManualOrganizationHistoryIntegration 覆盖手动整理记录的写入与倒序读取。
func TestManualOrganizationHistoryIntegration(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	ctx := t.Context()

	if records, err := store.ListManualOrganizationHistory(ctx, 10); err != nil || len(records) != 0 {
		t.Fatalf("空表 ListManualOrganizationHistory() = (%d, %v)", len(records), err)
	}
	if err := store.RecordManualOrganization(ctx, "/downloads/a.mkv", "/library/a.mkv"); err != nil {
		t.Fatalf("RecordManualOrganization(a) error = %v", err)
	}
	if err := store.RecordManualOrganization(ctx, "/downloads/b.mkv", "/library/b.mkv"); err != nil {
		t.Fatalf("RecordManualOrganization(b) error = %v", err)
	}

	records, err := store.ListManualOrganizationHistory(ctx, 0)
	if err != nil {
		t.Fatalf("ListManualOrganizationHistory() error = %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("手动整理记录数 = %d, want 2", len(records))
	}
	if records[0].SourcePath != "/downloads/b.mkv" || records[0].TargetPath != "/library/b.mkv" ||
		records[0].State != "completed" || records[0].UpdatedAt.IsZero() {
		t.Fatalf("最近记录 = %#v", records[0])
	}
	if limited, err := store.ListManualOrganizationHistory(ctx, 1); err != nil || len(limited) != 1 {
		t.Fatalf("limit=1 = (%d, %v)", len(limited), err)
	}

	// 未接数据库的 Store 走空结果分支而不是报错。
	empty := &Store{}
	if records, err := empty.ListManualOrganizationHistory(ctx, 10); err != nil || len(records) != 0 {
		t.Fatalf("空 Store ListManualOrganizationHistory() = (%d, %v)", len(records), err)
	}
	if err := empty.RecordManualOrganization(ctx, "a", "b"); err != nil {
		t.Fatalf("空 Store RecordManualOrganization() error = %v", err)
	}
}

// TestListSubscriptionDownloadFilesEmptyIntegration 覆盖无下载文件与非法订阅 ID 的返回。
func TestListSubscriptionDownloadFilesEmptyIntegration(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	const username = "download-files-owner"
	insertSubscriptionIntegrationUsers(t, store, username)
	ctx := t.Context()

	if files, err := store.ListSubscriptionDownloadFiles(ctx, 0); err != nil || len(files) != 0 {
		t.Fatalf("非法订阅 ID = (%d, %v)", len(files), err)
	}
	subscriptionID := createSubscriptionIntegrationFixture(t, store, username, "download-files")
	files, err := store.ListSubscriptionDownloadFiles(ctx, subscriptionID)
	if err != nil {
		t.Fatalf("ListSubscriptionDownloadFiles() error = %v", err)
	}
	if len(files) != 0 {
		t.Fatalf("无文件订阅返回 %d 条, want 0", len(files))
	}
}

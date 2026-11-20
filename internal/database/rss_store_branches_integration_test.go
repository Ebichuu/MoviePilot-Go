//go:build integration

package database

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/moviepilot-go/moviepilot-go/internal/auth"
)

// 本文件补的是 RSS 存储层里「真实存在但尚未被走到的业务分支」：
// 游标/条目不存在的稳定错误、未到期游标、识别失败计数的累加与封顶，
// 以及提交轮询前对敏感与非法条目的拒绝。

func newRSSBranchStore(t *testing.T) *Store {
	t.Helper()
	baseStore := newSiteStatisticsIntegrationStore(t)
	secretBox, err := auth.NewSecretBox("rss-branch-secret-master-key")
	if err != nil {
		t.Fatalf("创建 RSS 分支测试 SecretBox: %v", err)
	}
	return NewStore(baseStore.db, secretBox)
}

func TestRSSLeaseAcquireMissingAndNotDuePostgres(t *testing.T) {
	store := newRSSBranchStore(t)
	now := time.Now().UTC()
	const endpoint = "https://tracker.example/rss"

	cursor, acquired, err := store.TryAcquireRSSLease(t.Context(), 987654, endpoint, now, time.Minute)
	if acquired || !errors.Is(err, ErrRSSCursorNotFound) {
		t.Fatalf("缺失游标 TryAcquireRSSLease() = %#v, acquired %v, error %v", cursor, acquired, err)
	}

	siteID := insertRSSIntegrationSite(t, store, "rss-not-due")
	if _, err := store.EnsureRSSCursor(t.Context(), siteID, endpoint); err != nil {
		t.Fatalf("EnsureRSSCursor() error = %v", err)
	}
	if _, err := store.db.Pool.Exec(t.Context(), `
		UPDATE site_rss_cursors SET next_poll_at = now() + interval '1 hour' WHERE site_id=$1`, siteID); err != nil {
		t.Fatalf("推迟游标 error = %v", err)
	}
	cursor, acquired, err = store.TryAcquireRSSLease(t.Context(), siteID, endpoint, now, time.Minute)
	if acquired || err != nil || cursor.SiteID != 0 || cursor.Endpoint != "" {
		t.Fatalf("未到期游标 TryAcquireRSSLease() = %#v, acquired %v, error %v", cursor, acquired, err)
	}
}

func TestRSSFailPollAndCompleteMissingRowsPostgres(t *testing.T) {
	store := newRSSBranchStore(t)
	now := time.Now().UTC()

	if err := store.FailRSSPoll(t.Context(), 987654, "https://tracker.example/rss", uuid.New(), now, "boom"); !errors.Is(err, ErrRSSCursorNotFound) {
		t.Fatalf("缺失游标 FailRSSPoll() error = %v, want ErrRSSCursorNotFound", err)
	}
	if err := store.CompleteRSSItem(t.Context(), 987654321, uuid.New(), "matched", ""); !errors.Is(err, ErrRSSItemNotFound) {
		t.Fatalf("缺失条目 CompleteRSSItem() error = %v, want ErrRSSItemNotFound", err)
	}
}

func TestRSSMediaRecognitionFailureAccountingPostgres(t *testing.T) {
	store := newRSSBranchStore(t)
	siteID := insertRSSIntegrationSite(t, store, "rss-recognition")
	itemID := insertRSSBoundaryItem(t, store, siteID, "recognition-item-key", "pending", time.Now().UTC(), nil)

	if err := store.UpdateRSSItemMediaRecognition(t.Context(), 987654321, "不存在"); !errors.Is(err, ErrRSSItemNotFound) {
		t.Fatalf("缺失条目 UpdateRSSItemMediaRecognition() error = %v, want ErrRSSItemNotFound", err)
	}

	for attempt := 1; attempt <= maxRSSMediaRecognizeFailures+2; attempt++ {
		if err := store.UpdateRSSItemMediaRecognition(t.Context(), itemID, ""); err != nil {
			t.Fatalf("第 %d 次失败识别 error = %v", attempt, err)
		}
		var title string
		var failCount int
		if err := store.db.Pool.QueryRow(t.Context(), `
			SELECT recognized_media_title, media_recognize_fail_count FROM site_rss_items WHERE id=$1`, itemID).Scan(&title, &failCount); err != nil {
			t.Fatalf("读取识别状态 error = %v", err)
		}
		// 迁移 035 把该列默认值设为 1（首次发现即算一次未识别），因此期望值是 1+attempt。
		want := 1 + attempt
		if want > maxRSSMediaRecognizeFailures {
			want = maxRSSMediaRecognizeFailures
		}
		if title != "" || failCount != want {
			t.Fatalf("第 %d 次失败识别后 = (%q, %d), want (\"\", %d)", attempt, title, failCount, want)
		}
	}

	if err := store.UpdateRSSItemMediaRecognition(t.Context(), itemID, " 识别出的标题 "); err != nil {
		t.Fatalf("成功识别 error = %v", err)
	}
	var title string
	var failCount int
	if err := store.db.Pool.QueryRow(t.Context(), `
		SELECT recognized_media_title, media_recognize_fail_count FROM site_rss_items WHERE id=$1`, itemID).Scan(&title, &failCount); err != nil {
		t.Fatalf("读取识别成功状态 error = %v", err)
	}
	if title != "识别出的标题" || failCount != 0 {
		t.Fatalf("成功识别后 = (%q, %d), want (\"识别出的标题\", 0)", title, failCount)
	}
}

func TestRSSCommitPollRejectsUnsafeItemsPostgres(t *testing.T) {
	store := newRSSBranchStore(t)
	siteID := insertRSSIntegrationSite(t, store, "rss-commit-guard")
	const endpoint = "https://tracker.example/rss"
	now := time.Now().UTC()
	token := uuid.New()

	if err := store.CommitRSSPoll(t.Context(), siteID, endpoint, token, RSSPollResult{
		ObservedAt: now,
		Items:      []RSSItemInput{{ItemKey: "short", Title: "标题"}},
	}, now.Add(time.Minute)); !errors.Is(err, ErrInvalidRSSInput) {
		t.Fatalf("短 ItemKey CommitRSSPoll() error = %v, want ErrInvalidRSSInput", err)
	}

	if err := store.CommitRSSPoll(t.Context(), siteID, endpoint, token, RSSPollResult{
		ObservedAt: now,
		Items: []RSSItemInput{{
			ItemKey:        "long-enough-item-key",
			Title:          "标题",
			ResourceSecret: "https://tracker.example/download?id=1\npasskey=leak",
		}},
	}, now.Add(time.Minute)); !errors.Is(err, ErrInvalidRSSInput) {
		t.Fatalf("含换行的 ResourceSecret CommitRSSPoll() error = %v, want ErrInvalidRSSInput", err)
	}
}

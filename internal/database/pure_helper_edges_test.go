package database

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/moviepilot-go/moviepilot-go/internal/downloader"
	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

// 本文件覆盖若干纯辅助函数的边界分支：嵌套敏感值识别、配置解码、
// 剧集状态兼容表、下载对账目标校验、通知清理时间解码等。

func TestContainsSensitiveRSSValueNested(t *testing.T) {
	if !containsSensitiveRSSValue([]any{map[string]any{"cookie": "abc"}}) {
		t.Fatal("数组内的敏感键应被识别")
	}
	if !containsSensitiveRSSValue([]any{[]any{"token=abc"}}) {
		t.Fatal("嵌套数组内的敏感值应被识别")
	}
	if containsSensitiveRSSValue([]any{"plain", map[string]any{"title": "x"}}) {
		t.Fatal("普通内容不应被判为敏感")
	}
}

func TestDecodeDownloadIntentAcceptanceConfigRejects(t *testing.T) {
	if _, err := decodeDownloadIntentAcceptanceConfig(nil); err == nil {
		t.Fatal("空配置应报错")
	}
	if _, err := decodeDownloadIntentAcceptanceConfig([]byte("null")); err == nil {
		t.Fatal("null 配置应报错")
	}
	config, err := decodeDownloadIntentAcceptanceConfig([]byte(`{"start_episode":1}`))
	if err != nil || config == nil {
		t.Fatalf("合法配置 = %v, %v", config, err)
	}
}

func TestCompatibleSubscriptionEpisodeStatesBranches(t *testing.T) {
	pendingIndex := compatibleSubscriptionEpisodeStates([]string{"custom"}, subscription.SubscriptionEpisodePendingIndex)
	if len(pendingIndex) == 0 || pendingIndex[0] != "custom" {
		t.Fatalf("待入库兼容状态 = %v", pendingIndex)
	}

	indexed := compatibleSubscriptionEpisodeStates(nil, subscription.SubscriptionEpisodeIndexed)
	if len(indexed) != 1 || indexed[0] != string(subscription.SubscriptionEpisodeIndexed) {
		t.Fatalf("已入库兼容状态 = %v", indexed)
	}

	deduped := compatibleSubscriptionEpisodeStates(
		[]string{"dup", "dup", string(subscription.SubscriptionEpisodeIndexed)},
		subscription.SubscriptionEpisodeIndexed,
	)
	seen := map[string]int{}
	for _, state := range deduped {
		seen[state]++
	}
	if seen["dup"] != 1 || seen[string(subscription.SubscriptionEpisodeIndexed)] != 1 {
		t.Fatalf("兼容状态未去重: %v", deduped)
	}
}

func TestNormalizeDownloadReconciliationTargetRejects(t *testing.T) {
	valid := downloader.ReconciliationTarget{
		ID:           uuid.New(),
		Username:     "alice",
		DownloaderID: uuid.New().String(),
		State:        downloader.IntentSubmitting,
		TorrentHash:  "ABCDEF0123456789ABCDEF0123456789ABCDEF01",
		UpdatedAt:    time.Now().UTC(),
	}
	if _, err := normalizeDownloadReconciliationTarget(valid); err != nil {
		t.Fatalf("合法目标 error = %v", err)
	}

	cases := []struct {
		name   string
		mutate func(target *downloader.ReconciliationTarget)
	}{
		{name: "下载器标识非法", mutate: func(target *downloader.ReconciliationTarget) { target.DownloaderID = "not-a-uuid" }},
		{name: "状态不支持", mutate: func(target *downloader.ReconciliationTarget) { target.State = downloader.IntentState("unknown") }},
		{name: "计划态带哈希", mutate: func(target *downloader.ReconciliationTarget) { target.State = downloader.IntentPlanned }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			target := valid
			tc.mutate(&target)
			if _, err := normalizeDownloadReconciliationTarget(target); err == nil {
				t.Fatal("应被拒绝")
			}
		})
	}
}

func TestDecodeNotificationClearBeforeRejects(t *testing.T) {
	if _, err := decodeNotificationClearBefore([]byte("1 2")); err == nil {
		t.Fatal("尾随内容应报错")
	}
	// 解码后的时间戳会被归一到非负数，因此 Valid() 在这条路径上恒为真：
	// 172-174 的拒绝分支结构性不可达。
	if _, err := decodeNotificationClearBefore([]byte("-1")); err != nil {
		t.Fatalf("负时间戳会被归零 error = %v", err)
	}
	if _, err := decodeNotificationClearBefore([]byte("1700000000")); err != nil {
		t.Fatalf("合法时间戳 error = %v", err)
	}
}

func TestSanitizeOrganizationErrorFallback(t *testing.T) {
	if got := sanitizeOrganizationError("   "); got != "organization operation failed" {
		t.Fatalf("空白错误 = %q", got)
	}
	if got := sanitizeOrganizationError("normal failure"); got != "normal failure" {
		t.Fatalf("普通错误 = %q", got)
	}
}

func TestTVSubmissionReplayStateRejectsOtherExpected(t *testing.T) {
	if tvSubmissionReplayState(subscription.Active, subscription.Completed) {
		t.Fatal("非重放期望态不应判定为重放")
	}
	if !tvSubmissionReplayState(subscription.Indexed, subscription.Active) {
		t.Fatal("后续态应判定为重放")
	}
}

func TestAddSiteTimingSampleClampsSamples(t *testing.T) {
	samples, average := addSiteTimingSample(map[string]int{"2026-11-20 10:00:00": 0}, "2026-11-20 10:00:00", 4)
	if len(samples) == 0 {
		t.Fatal("样本不应为空")
	}
	if average < 1 {
		t.Fatalf("平均值 = %d", average)
	}
	negative, _ := addSiteTimingSample(nil, "2026-11-20 10:00:00", 0)
	if len(negative) != 1 {
		t.Fatalf("零秒样本 = %v", negative)
	}
}

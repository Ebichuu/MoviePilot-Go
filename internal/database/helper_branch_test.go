package database

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"testing"
	"time"
)

// 本文件覆盖持久化错误转换与若干纯函数辅助分支：这些分支不依赖真实数据库，
// 但直接决定了上层能否区分「上下文取消」与「持久化失败」。

func TestContextErrorHelpers(t *testing.T) {
	t.Parallel()
	helpers := map[string]func(context.Context) error{
		"downloadIntentPlanContextError":          downloadIntentPlanContextError,
		"downloadIntentSuccessInputContextError":  downloadIntentSuccessInputContextError,
		"organizationSuccessSnapshotContextError": organizationSuccessSnapshotContextError,
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	for name, helper := range helpers {
		if err := helper(nil); err != nil {
			t.Fatalf("%s(nil) = %v, want nil", name, err)
		}
		if err := helper(context.Background()); err != nil {
			t.Fatalf("%s(有效上下文) = %v, want nil", name, err)
		}
		if err := helper(canceled); !errors.Is(err, context.Canceled) {
			t.Fatalf("%s(取消上下文) = %v, want context.Canceled", name, err)
		}
	}
}

func TestPersistenceErrorHelpers(t *testing.T) {
	t.Parallel()
	helpers := map[string]func(context.Context) error{
		"downloadIntentPlanPersistenceError":                downloadIntentPlanPersistenceError,
		"downloadIntentPersistenceError":                    downloadIntentPersistenceError,
		"mediaIndexPersistenceError":                        mediaIndexPersistenceError,
		"notificationHistoryPersistenceError":               notificationHistoryPersistenceError,
		"notificationPersistenceError":                      notificationPersistenceError,
		"organizationSuccessSnapshotPersistenceError":       organizationSuccessSnapshotPersistenceError,
		"organizationPersistenceError":                      organizationPersistenceError,
		"rssPersistenceError":                               rssPersistenceError,
		"scrapePersistenceError":                            scrapePersistenceError,
		"subscriptionDownloadStatePersistenceError":         subscriptionDownloadStatePersistenceError,
		"subscriptionEpisodePersistenceError":               subscriptionEpisodePersistenceError,
		"subscriptionPreciseSearchPersistenceError":         subscriptionPreciseSearchPersistenceError,
		"downloadIntentSubscriptionBindingPersistenceError": downloadIntentSubscriptionBindingPersistenceError,
		"subscriptionPublicMutationPersistenceError":        subscriptionPublicMutationPersistenceError,
		"subscriptionTargetPersistenceError":                subscriptionTargetPersistenceError,
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	for name, helper := range helpers {
		if err := helper(nil); err == nil {
			t.Fatalf("%s(nil) 应返回稳定错误", name)
		}
		if err := helper(context.Background()); err == nil {
			t.Fatalf("%s(有效上下文) 应返回稳定错误", name)
		}
		if err := helper(canceled); !errors.Is(err, context.Canceled) {
			t.Fatalf("%s(取消上下文) = %v, want context.Canceled", name, err)
		}
	}
	// 该辅助函数要求调用方保证非 nil 上下文。
	if err := v2DownloadFailurePersistenceError(context.Background()); err == nil {
		t.Fatal("v2DownloadFailurePersistenceError(有效上下文) 应返回稳定错误")
	}
	if err := v2DownloadFailurePersistenceError(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("v2DownloadFailurePersistenceError(取消上下文) = %v, want context.Canceled", err)
	}
}

func TestSubscriptionIntegerBranches(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		value  any
		want   int
		wantOK bool
	}{
		{name: "int", value: 7, want: 7, wantOK: true},
		{name: "int32", value: int32(8), want: 8, wantOK: true},
		{name: "int64", value: int64(9), want: 9, wantOK: true},
		{name: "整数值浮点", value: 10.0, want: 10, wantOK: true},
		{name: "非整数浮点", value: 10.5, want: 10},
		{name: "JSON 数字", value: json.Number("11"), want: 11, wantOK: true},
		{name: "非法 JSON 数字", value: json.Number("1e400")},
		{name: "字符串", value: "12"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, ok := subscriptionInteger(tt.value)
			if got != tt.want || ok != tt.wantOK {
				t.Fatalf("subscriptionInteger(%#v) = (%d, %v), want (%d, %v)", tt.value, got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

func TestSitePriorityBranches(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		config map[string]any
		want   int
	}{
		{name: "缺少优先级", config: nil},
		{name: "int", config: map[string]any{"pri": 3}, want: 3},
		{name: "int64", config: map[string]any{"pri": int64(4)}, want: 4},
		{name: "float64", config: map[string]any{"pri": 5.9}, want: 5},
		{name: "JSON 数字", config: map[string]any{"pri": json.Number("6")}, want: 6},
		{name: "非法 JSON 数字", config: map[string]any{"pri": json.Number("x")}},
		{name: "字符串", config: map[string]any{"pri": " 7 "}, want: 7},
		{name: "非法字符串", config: map[string]any{"pri": "high"}},
		{name: "布尔值", config: map[string]any{"pri": true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := sitePriority(Site{Config: tt.config}); got != tt.want {
				t.Fatalf("sitePriority(%#v) = %d, want %d", tt.config, got, tt.want)
			}
		})
	}
}

func TestMapSubscriptionPublicTargetErrorBranches(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want error
	}{
		{name: "目标冲突", err: ErrSubscriptionTargetConflict, want: ErrSubscriptionPublicMutationConflict},
		{name: "目标缺失", err: ErrSubscriptionTargetNotFound, want: ErrSubscriptionNotFound},
		{name: "目标非法", err: ErrInvalidSubscriptionTarget, want: ErrInvalidSubscriptionPublicMutation},
		{name: "其它错误原样透传", err: ErrSubscriptionPublicMutationPersistence, want: ErrSubscriptionPublicMutationPersistence},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := mapSubscriptionPublicTargetError(tt.err); !errors.Is(got, tt.want) {
				t.Fatalf("mapSubscriptionPublicTargetError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestNormalizeNotificationClearTimestampBranches(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		value any
		want  int64
	}{
		{name: "JSON 整数", value: json.Number("1700000000"), want: 1700000000},
		{name: "JSON 小数", value: json.Number("1700000000.5"), want: 1700000000},
		{name: "JSON 溢出", value: json.Number("1e400")},
		{name: "字符串", value: " 1700000001 ", want: 1700000001},
		{name: "非法字符串", value: "昨天"},
		{name: "布尔真", value: true, want: 1},
		{name: "布尔假", value: false},
		{name: "浮点", value: 1700000002.0, want: 1700000002},
		{name: "非有限浮点", value: math.Inf(1)},
		{name: "NaN", value: math.NaN()},
		{name: "int64", value: int64(1700000003), want: 1700000003},
		{name: "零值", value: int64(0)},
		{name: "负数", value: int64(-1)},
		{name: "不支持的类型", value: struct{}{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := normalizeNotificationClearTimestamp(tt.value); got != tt.want {
				t.Fatalf("normalizeNotificationClearTimestamp(%#v) = %d, want %d", tt.value, got, tt.want)
			}
		})
	}
}

func TestPoolAndStatsNilSafety(t *testing.T) {
	t.Parallel()
	var store *Store
	if store.Pool() != nil {
		t.Fatal("nil Store.Pool() 应返回 nil")
	}
	if (&Store{}).Pool() != nil {
		t.Fatal("空 db 的 Store.Pool() 应返回 nil")
	}
	var db *DB
	if db.Stats() != nil {
		t.Fatal("nil DB.Stats() 应返回 nil")
	}
	if (&DB{}).Stats() != nil {
		t.Fatal("空连接池的 DB.Stats() 应返回 nil")
	}
}

func TestFormatSiteStatisticTimeBranches(t *testing.T) {
	t.Parallel()
	value := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	shanghai, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Skipf("缺少时区数据库: %v", err)
	}
	if got, want := formatSiteStatisticTime(value, shanghai), "2026-11-19 20:00:00"; got != want {
		t.Fatalf("formatSiteStatisticTime(上海) = %q, want %q", got, want)
	}
	if got := formatSiteStatisticTime(value, nil); got == "" {
		t.Fatal("nil 时区应回退到本地时区")
	}
}

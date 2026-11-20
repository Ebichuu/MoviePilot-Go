package jobs

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

// TestSanitizedContextErrorHelpers 统一覆盖各队列的上下文错误归一化辅助：
// 上下文优先、err 包装的取消/超时、以及无匹配时返回 nil。
func TestSanitizedContextErrorHelpers(t *testing.T) {
	helpers := []struct {
		name string
		fn   func(context.Context, error) error
	}{
		{name: "scrape", fn: sanitizedScrapeContextError},
		{name: "notification", fn: sanitizedNotificationContextError},
		{name: "download", fn: sanitizedDownloadContextError},
		{name: "media_server_sync", fn: sanitizedMediaServerSyncContextError},
		{name: "organization", fn: sanitizedOrganizationContextError},
		{name: "rss", fn: sanitizedRSSContextError},
		{name: "site_userdata", fn: sanitizedSiteUserDataContextError},
	}
	for _, helper := range helpers {
		t.Run(helper.name, func(t *testing.T) {
			if got := helper.fn(nil, nil); got != nil {
				t.Fatalf("nil 上下文与 nil 错误 = %v", got)
			}
			if got := helper.fn(context.Background(), nil); got != nil {
				t.Fatalf("无错误 = %v", got)
			}
			canceled, cancel := context.WithCancel(context.Background())
			cancel()
			if got := helper.fn(canceled, errors.New("无关错误")); !errors.Is(got, context.Canceled) {
				t.Fatalf("上下文优先 = %v", got)
			}
			wrapped := errors.New("下载失败")
			if got := helper.fn(context.Background(), errors.Join(wrapped, context.Canceled)); !errors.Is(got, context.Canceled) {
				t.Fatalf("包装取消 = %v", got)
			}
			if got := helper.fn(context.Background(), context.DeadlineExceeded); !errors.Is(got, context.DeadlineExceeded) {
				t.Fatalf("超时 = %v", got)
			}
			if got := helper.fn(context.Background(), wrapped); got != nil {
				t.Fatalf("无匹配应返回 nil，实际 %v", got)
			}
		})
	}
}

// TestSanitizedDownloadErrorFallback 覆盖下载队列带回退错误的归一化。
func TestSanitizedDownloadErrorFallback(t *testing.T) {
	fallback := errors.New("回退错误")
	if got := sanitizedDownloadError(context.Background(), errors.New("任意错误"), fallback); !errors.Is(got, fallback) {
		t.Fatalf("回退 = %v", got)
	}
	if got := sanitizedDownloadError(context.Background(), context.Canceled, fallback); !errors.Is(got, context.Canceled) {
		t.Fatalf("取消 = %v", got)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if got := sanitizedDownloadError(canceled, errors.New("任意错误"), fallback); !errors.Is(got, context.Canceled) {
		t.Fatalf("上下文优先 = %v", got)
	}
}

// TestEnqueueDownloadTxGuards 覆盖事务入队方法在未启用时不触库的闸门。
func TestEnqueueDownloadTxGuards(t *testing.T) {
	args := DownloadArgs{Username: "alice", IntentID: uuid.New()}
	ctx := context.Background()

	if err := (&Manager{}).EnqueueDownloadTx(nil, nil, args); !errors.Is(err, ErrDownloadEnqueue) {
		t.Fatalf("nil 上下文 error = %v", err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := (&Manager{}).EnqueueDownloadTx(canceled, nil, args); !errors.Is(err, context.Canceled) {
		t.Fatalf("已取消上下文 error = %v", err)
	}
	if err := (&Manager{}).EnqueueDownloadTx(ctx, nil, args); !errors.Is(err, ErrDownloadEnqueue) {
		t.Fatalf("nil 事务 error = %v", err)
	}
}

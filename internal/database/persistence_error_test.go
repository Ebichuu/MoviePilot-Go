package database

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/moviepilot-go/moviepilot-go/internal/mediaindex"
	"github.com/moviepilot-go/moviepilot-go/internal/notificationhistory"
)

func cancelledContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

func expiredContext() context.Context {
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	_ = cancel
	return ctx
}

func TestMediaIndexPersistenceError(t *testing.T) {
	// 没有上下文错误时才回落到持久化错误。
	if err := mediaIndexPersistenceError(nil); !errors.Is(err, mediaindex.ErrPersistence) {
		t.Fatalf("nil ctx 应返回 ErrPersistence，实际 %v", err)
	}
	if err := mediaIndexPersistenceError(context.Background()); !errors.Is(err, mediaindex.ErrPersistence) {
		t.Fatalf("健康 ctx 应返回 ErrPersistence，实际 %v", err)
	}
	// 取消/超时必须原样上报，否则调用方会把「客户端走了」当成写库失败。
	if err := mediaIndexPersistenceError(cancelledContext()); !errors.Is(err, context.Canceled) {
		t.Fatalf("取消应返回 context.Canceled，实际 %v", err)
	}
	if err := mediaIndexPersistenceError(expiredContext()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("超时应返回 context.DeadlineExceeded，实际 %v", err)
	}
}

func TestNotificationHistoryPersistenceError(t *testing.T) {
	if err := notificationHistoryPersistenceError(nil); !errors.Is(err, notificationhistory.ErrPersistence) {
		t.Fatalf("nil ctx 应返回 ErrPersistence，实际 %v", err)
	}
	if err := notificationHistoryPersistenceError(cancelledContext()); !errors.Is(err, context.Canceled) {
		t.Fatalf("取消应返回 context.Canceled，实际 %v", err)
	}
}

func TestDownloadReconciliationQueryError(t *testing.T) {
	if err := downloadReconciliationQueryError(nil); !errors.Is(err, ErrDownloadReconciliationQuery) {
		t.Fatalf("nil ctx 应返回 ErrDownloadReconciliationQuery，实际 %v", err)
	}
	if err := downloadReconciliationQueryError(expiredContext()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("超时应返回 context.DeadlineExceeded，实际 %v", err)
	}
}

func TestValidMediaIndexLease(t *testing.T) {
	cases := []struct {
		lease time.Duration
		want  bool
	}{
		{minMediaIndexLease - time.Second, false},
		{minMediaIndexLease, true},
		{maxMediaIndexLease, true},
		{maxMediaIndexLease + time.Second, false},
		{0, false},
	}
	for _, testCase := range cases {
		if got := validMediaIndexLease(testCase.lease); got != testCase.want {
			t.Fatalf("validMediaIndexLease(%v) = %v，期望 %v", testCase.lease, got, testCase.want)
		}
	}
}

func TestNormalizeMediaIndexError(t *testing.T) {
	if got := normalizeMediaIndexError("  boom  "); got != "boom" {
		t.Fatalf("应去空格，实际 %q", got)
	}
	if got := normalizeMediaIndexError(""); got != "" {
		t.Fatalf("空串应保持空，实际 %q", got)
	}
	// 超长错误必须被截断：它会写进数据库列，不能让一条日志撑爆字段。
	long := strings.Repeat("x", maxMediaIndexError+50)
	got := normalizeMediaIndexError(long)
	if len(got) != maxMediaIndexError {
		t.Fatalf("应截断到 %d，实际 %d", maxMediaIndexError, len(got))
	}
	if strings.TrimSpace(got) == "" {
		t.Fatal("截断后不应变成空白")
	}
}

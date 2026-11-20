package jobs

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestManagerEnqueueGuards 覆盖各类入队方法的守卫顺序：参数校验 → 空上下文 →
// 已取消上下文 → 未启用/未初始化的管理器。
func TestManagerEnqueueGuards(t *testing.T) {
	ctx := context.Background()
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	now := time.Now().UTC()
	disabled := &Manager{}

	t.Run("RSS 轮询", func(t *testing.T) {
		valid := RSSPollArgs{SiteID: 7, Cutoff: now}
		if err := disabled.EnqueueRSSPoll(ctx, RSSPollArgs{}); !errors.Is(err, ErrInvalidRSSPollArgs) {
			t.Fatalf("零值参数 error = %v", err)
		}
		if err := disabled.EnqueueRSSPoll(nil, valid); !errors.Is(err, ErrRSSPollExecution) {
			t.Fatalf("nil ctx error = %v", err)
		}
		if err := disabled.EnqueueRSSPoll(canceled, valid); !errors.Is(err, context.Canceled) {
			t.Fatalf("取消上下文 error = %v", err)
		}
		if err := disabled.EnqueueRSSPoll(ctx, valid); !errors.Is(err, ErrRSSPollHandlerUnavailable) {
			t.Fatalf("未启用 error = %v", err)
		}
	})

	t.Run("RSS 扫描", func(t *testing.T) {
		if err := disabled.EnqueueRSSSweep(ctx, time.Time{}); !errors.Is(err, ErrInvalidRSSSweepArgs) {
			t.Fatalf("零值 cutoff error = %v", err)
		}
		if err := disabled.EnqueueRSSSweep(nil, now); !errors.Is(err, ErrRSSSweepExecution) {
			t.Fatalf("nil ctx error = %v", err)
		}
		if err := disabled.EnqueueRSSSweep(canceled, now); !errors.Is(err, context.Canceled) {
			t.Fatalf("取消上下文 error = %v", err)
		}
		if err := disabled.EnqueueRSSSweep(ctx, now); !errors.Is(err, ErrRSSSweepHandlerUnavailable) {
			t.Fatalf("未启用 error = %v", err)
		}
	})

	t.Run("RSS 匹配", func(t *testing.T) {
		if err := disabled.EnqueueRSSMatch(ctx, time.Time{}, nil); !errors.Is(err, ErrInvalidRSSMatchArgs) {
			t.Fatalf("零值 cutoff error = %v", err)
		}
		if err := disabled.EnqueueRSSMatch(nil, now, nil); !errors.Is(err, ErrRSSMatchExecution) {
			t.Fatalf("nil ctx error = %v", err)
		}
		if err := disabled.EnqueueRSSMatch(canceled, now, nil); !errors.Is(err, context.Canceled) {
			t.Fatalf("取消上下文 error = %v", err)
		}
		if err := disabled.EnqueueRSSMatch(ctx, now, []int{1, 2}); !errors.Is(err, ErrRSSMatchHandlerUnavailable) {
			t.Fatalf("未启用 error = %v", err)
		}
	})

	t.Run("整理扫描", func(t *testing.T) {
		if err := disabled.EnqueueOrganizationSweep(nil); !errors.Is(err, ErrOrganizationSweepExecution) {
			t.Fatalf("nil ctx error = %v", err)
		}
		if err := disabled.EnqueueOrganizationSweep(canceled); !errors.Is(err, context.Canceled) {
			t.Fatalf("取消上下文 error = %v", err)
		}
		if err := disabled.EnqueueOrganizationSweep(ctx); !errors.Is(err, ErrOrganizationSweepHandlerUnavailable) {
			t.Fatalf("未启用 error = %v", err)
		}
	})
}

// TestNewManagerRejectsMissingPool 覆盖 River 客户端创建前的连接池守卫。
func TestNewManagerRejectsMissingPool(t *testing.T) {
	if _, err := NewManager(nil, nil); err == nil {
		t.Fatal("NewManager(nil) 应返回错误")
	}
	if _, err := NewManagerWithConfig(nil, nil, ManagerConfig{}); err == nil {
		t.Fatal("NewManagerWithConfig(nil) 应返回错误")
	}
}

// TestProbeWorkerTimeout 覆盖探针任务的自定义超时。
func TestProbeWorkerTimeout(t *testing.T) {
	if got := (&probeWorker{}).Timeout(nil); got != 10*time.Second {
		t.Fatalf("Timeout() = %v, want 10s", got)
	}
	if kind := (ProbeArgs{}).Kind(); kind != "system_probe" {
		t.Fatalf("ProbeArgs.Kind() = %q", kind)
	}
}

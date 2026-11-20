//go:build integration

package jobs

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"
)

// TestProbeWorkerAndEnqueueProbeIntegration 覆盖 River 探针链路：
// probeWorker 真实 Ping 隔离 schema、Manager 创建、探针任务的一分钟唯一入队。
func TestProbeWorkerAndEnqueueProbeIntegration(t *testing.T) {
	pool := newRSSRiverIntegrationPool(t)
	ctx := t.Context()

	worker := &probeWorker{pool: pool}
	if err := worker.Work(ctx, nil); err != nil {
		t.Fatalf("probeWorker.Work() error = %v", err)
	}
	if got := worker.Timeout(nil); got != 10*time.Second {
		t.Fatalf("probeWorker.Timeout() = %v, want 10s", got)
	}

	// 连接池关闭后探针报告数据库错误，验证它确实依赖 Ping。
	closedPool := newRSSRiverIntegrationPool(t)
	closedPool.Close()
	if err := (&probeWorker{pool: closedPool}).Work(ctx, nil); err == nil {
		t.Fatal("连接池关闭后探针应报错")
	}

	manager, err := NewManager(pool, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	if manager == nil || manager.client == nil {
		t.Fatal("NewManager() 未创建 River 客户端")
	}

	for range 2 {
		if err := manager.EnqueueProbe(ctx); err != nil {
			t.Fatalf("EnqueueProbe() error = %v", err)
		}
	}
	var count int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM river_job WHERE kind='system_probe'`).Scan(&count); err != nil {
		t.Fatalf("统计探针任务: %v", err)
	}
	if count != 1 {
		t.Fatalf("探针任务行数 = %d, want 1（一分钟唯一窗口）", count)
	}

	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := manager.EnqueueProbe(canceled); err == nil {
		t.Fatal("取消上下文入队探针应报错")
	}
}

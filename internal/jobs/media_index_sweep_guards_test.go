package jobs

import (
	"context"
	"errors"
	"testing"

	"github.com/riverqueue/river"
)

// 本文件覆盖媒体入库确认恢复 worker 与手动入队的执行闸门：
// nil/已取消上下文、缺失 handler、非法参数、handler 失败（含上下文取消优先）。

func TestMediaIndexSweepWorkerGuards(t *testing.T) {
	ctx := context.Background()
	ok := mediaIndexSweepHandlerFunc(func(context.Context) error { return nil })

	if err := (&mediaIndexSweepWorker{handler: ok}).Work(nil, nil); !errors.Is(err, ErrMediaIndexSweepExecution) {
		t.Fatalf("nil 上下文 error = %v", err)
	}

	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := (&mediaIndexSweepWorker{handler: ok}).Work(canceled, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("已取消上下文 error = %v", err)
	}

	if err := (&mediaIndexSweepWorker{}).Work(ctx, nil); !errors.Is(err, ErrMediaIndexSweepHandlerUnavailable) {
		t.Fatalf("缺失 handler error = %v", err)
	}
	var nilWorker *mediaIndexSweepWorker
	if err := nilWorker.Work(ctx, nil); !errors.Is(err, ErrMediaIndexSweepHandlerUnavailable) {
		t.Fatalf("nil worker error = %v", err)
	}

	invalid := &river.Job[MediaIndexSweepArgs]{Args: MediaIndexSweepArgs{Key: "非法"}}
	if err := (&mediaIndexSweepWorker{handler: ok}).Work(ctx, invalid); err == nil {
		t.Fatal("非法参数应返回错误")
	}

	validJob := &river.Job[MediaIndexSweepArgs]{Args: MediaIndexSweepArgs{Key: mediaIndexSweepKey}}
	failing := &mediaIndexSweepWorker{handler: fakeMediaIndexSweep{err: errors.New("扫描失败")}}
	if err := failing.Work(ctx, validJob); !errors.Is(err, ErrMediaIndexSweepExecution) {
		t.Fatalf("handler 失败 error = %v", err)
	}

	canceledCtx, cancelLater := context.WithCancel(ctx)
	defer cancelLater()
	canceling := &mediaIndexSweepWorker{handler: mediaIndexSweepHandlerFunc(func(context.Context) error {
		cancelLater()
		return errors.New("扫描失败")
	})}
	if err := canceling.Work(canceledCtx, validJob); !errors.Is(err, context.Canceled) {
		t.Fatalf("取消时的 handler 失败 error = %v", err)
	}
}

func TestEnqueueMediaIndexSweepGuards(t *testing.T) {
	if err := (&Manager{}).EnqueueMediaIndexSweep(nil); !errors.Is(err, ErrMediaIndexSweepExecution) {
		t.Fatalf("nil 上下文 error = %v", err)
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := (&Manager{}).EnqueueMediaIndexSweep(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("已取消上下文 error = %v", err)
	}

	if err := (&Manager{}).EnqueueMediaIndexSweep(context.Background()); !errors.Is(err, ErrMediaIndexSweepHandlerUnavailable) {
		t.Fatalf("未启用 error = %v", err)
	}
	var nilManager *Manager
	if err := nilManager.EnqueueMediaIndexSweep(context.Background()); !errors.Is(err, ErrMediaIndexSweepHandlerUnavailable) {
		t.Fatalf("nil manager error = %v", err)
	}
}

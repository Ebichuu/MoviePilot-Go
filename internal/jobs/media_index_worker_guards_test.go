package jobs

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
)

type mediaIndexGuardHandler func(context.Context, MediaIndexArgs) error

func (f mediaIndexGuardHandler) HandleMediaIndex(ctx context.Context, args MediaIndexArgs) error {
	return f(ctx, args)
}

// TestMediaIndexWorkerGuards 覆盖媒体入库确认 worker 的执行闸门与
// handler 失败时的上下文错误优先。
func TestMediaIndexWorkerGuards(t *testing.T) {
	ctx := context.Background()
	ok := mediaIndexGuardHandler(func(context.Context, MediaIndexArgs) error { return nil })
	validJob := &river.Job[MediaIndexArgs]{Args: MediaIndexArgs{RequestID: uuid.New()}}

	if err := (&mediaIndexWorker{handler: ok}).Work(nil, nil); !errors.Is(err, ErrMediaIndexExecution) {
		t.Fatalf("nil 上下文 error = %v", err)
	}

	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := (&mediaIndexWorker{handler: ok}).Work(canceled, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("已取消上下文 error = %v", err)
	}

	if err := (&mediaIndexWorker{}).Work(ctx, nil); !errors.Is(err, ErrMediaIndexHandlerUnavailable) {
		t.Fatalf("缺失 handler error = %v", err)
	}
	var nilWorker *mediaIndexWorker
	if err := nilWorker.Work(ctx, nil); !errors.Is(err, ErrMediaIndexHandlerUnavailable) {
		t.Fatalf("nil worker error = %v", err)
	}

	invalid := &river.Job[MediaIndexArgs]{Args: MediaIndexArgs{}}
	if err := (&mediaIndexWorker{handler: ok}).Work(ctx, invalid); err == nil {
		t.Fatal("非法参数应返回错误")
	}

	failing := &mediaIndexWorker{handler: mediaIndexGuardHandler(func(context.Context, MediaIndexArgs) error {
		return errors.New("确认失败")
	})}
	if err := failing.Work(ctx, validJob); !errors.Is(err, ErrMediaIndexExecution) {
		t.Fatalf("handler 失败 error = %v", err)
	}

	handlerCanceled, cancelLater := context.WithCancel(ctx)
	defer cancelLater()
	canceling := &mediaIndexWorker{handler: mediaIndexGuardHandler(func(context.Context, MediaIndexArgs) error {
		cancelLater()
		return errors.New("确认失败")
	})}
	if err := canceling.Work(handlerCanceled, validJob); !errors.Is(err, context.Canceled) {
		t.Fatalf("取消时的 handler 失败 error = %v", err)
	}
}

// TestSanitizedMediaIndexContextError 覆盖上下文错误的归一化顺序。
func TestSanitizedMediaIndexContextError(t *testing.T) {
	if got := sanitizedMediaIndexContextError(nil, nil); got != nil {
		t.Fatalf("无错误应为 nil，实际 %v", got)
	}
	if got := sanitizedMediaIndexContextError(context.Background(), context.Canceled); !errors.Is(got, context.Canceled) {
		t.Fatalf("取消归一化 = %v", got)
	}
	if got := sanitizedMediaIndexContextError(context.Background(), context.DeadlineExceeded); !errors.Is(got, context.DeadlineExceeded) {
		t.Fatalf("超时归一化 = %v", got)
	}
}

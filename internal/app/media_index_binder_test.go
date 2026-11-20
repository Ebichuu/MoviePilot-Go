package app

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/moviepilot-go/moviepilot-go/internal/jobs"
)

type mediaIndexServiceFake struct {
	requests []uuid.UUID
	err      error
}

func (f *mediaIndexServiceFake) Handle(_ context.Context, requestID uuid.UUID) error {
	f.requests = append(f.requests, requestID)
	return f.err
}

type mediaIndexSweepServiceFake struct {
	calls int
	err   error
}

func (f *mediaIndexSweepServiceFake) Handle(context.Context) error {
	f.calls++
	return f.err
}

func TestMediaIndexHandlerBinderBindsOnce(t *testing.T) {
	t.Parallel()

	var nilBinder *mediaIndexHandlerBinder
	if err := nilBinder.Bind(&mediaIndexServiceFake{}); !errors.Is(err, jobs.ErrMediaIndexHandlerUnavailable) {
		t.Fatalf("nil binder Bind error = %v", err)
	}
	binder := &mediaIndexHandlerBinder{}
	if err := binder.Bind(nil); !errors.Is(err, jobs.ErrMediaIndexHandlerUnavailable) {
		t.Fatalf("nil handler Bind error = %v", err)
	}

	first := &mediaIndexServiceFake{}
	if err := binder.Bind(first); err != nil {
		t.Fatalf("Bind() error = %v", err)
	}
	// 构造环只允许绑定一次：第二次必须拒绝，且不能悄悄替换掉已经跑起来的处理器。
	second := &mediaIndexServiceFake{}
	if err := binder.Bind(second); !errors.Is(err, errMediaIndexHandlerAlreadyBound) {
		t.Fatalf("重复 Bind error = %v", err)
	}

	requestID := uuid.New()
	if err := binder.HandleMediaIndex(t.Context(), jobs.MediaIndexArgs{RequestID: requestID}); err != nil {
		t.Fatalf("HandleMediaIndex() error = %v", err)
	}
	if len(first.requests) != 1 || first.requests[0] != requestID {
		t.Fatalf("首个处理器收到 = %#v", first.requests)
	}
	if len(second.requests) != 0 {
		t.Fatalf("第二个处理器不该被调用: %#v", second.requests)
	}
}

func TestMediaIndexHandlerUnbound(t *testing.T) {
	t.Parallel()

	var nilBinder *mediaIndexHandlerBinder
	if err := nilBinder.HandleMediaIndex(t.Context(), jobs.MediaIndexArgs{RequestID: uuid.New()}); !errors.Is(err, jobs.ErrMediaIndexHandlerUnavailable) {
		t.Fatalf("nil binder error = %v", err)
	}
	if err := (&mediaIndexHandlerBinder{}).HandleMediaIndex(t.Context(), jobs.MediaIndexArgs{RequestID: uuid.New()}); !errors.Is(err, jobs.ErrMediaIndexHandlerUnavailable) {
		t.Fatalf("未绑定 error = %v", err)
	}

	// 处理器错误原样上抛，River 才能按错误类型决定重试。
	boom := errors.New("确认失败")
	failing := &mediaIndexHandlerBinder{}
	if err := failing.Bind(&mediaIndexServiceFake{err: boom}); err != nil {
		t.Fatalf("Bind() error = %v", err)
	}
	if err := failing.HandleMediaIndex(t.Context(), jobs.MediaIndexArgs{RequestID: uuid.New()}); !errors.Is(err, boom) {
		t.Fatalf("处理器错误 = %v", err)
	}
}

func TestMediaIndexSweepHandlerBinderBindsOnce(t *testing.T) {
	t.Parallel()

	var nilBinder *mediaIndexSweepHandlerBinder
	if err := nilBinder.Bind(&mediaIndexSweepServiceFake{}); !errors.Is(err, jobs.ErrMediaIndexSweepHandlerUnavailable) {
		t.Fatalf("nil binder Bind error = %v", err)
	}
	binder := &mediaIndexSweepHandlerBinder{}
	if err := binder.Bind(nil); !errors.Is(err, jobs.ErrMediaIndexSweepHandlerUnavailable) {
		t.Fatalf("nil handler Bind error = %v", err)
	}
	first := &mediaIndexSweepServiceFake{}
	if err := binder.Bind(first); err != nil {
		t.Fatalf("Bind() error = %v", err)
	}
	if err := binder.Bind(&mediaIndexSweepServiceFake{}); !errors.Is(err, errMediaIndexSweepHandlerAlreadyBound) {
		t.Fatalf("重复 Bind error = %v", err)
	}

	if err := binder.HandleMediaIndexSweep(t.Context()); err != nil {
		t.Fatalf("HandleMediaIndexSweep() error = %v", err)
	}
	if first.calls != 1 {
		t.Fatalf("恢复处理器调用次数 = %d", first.calls)
	}
}

func TestMediaIndexSweepHandlerUnbound(t *testing.T) {
	t.Parallel()

	var nilBinder *mediaIndexSweepHandlerBinder
	if err := nilBinder.HandleMediaIndexSweep(t.Context()); !errors.Is(err, jobs.ErrMediaIndexSweepHandlerUnavailable) {
		t.Fatalf("nil binder error = %v", err)
	}
	if err := (&mediaIndexSweepHandlerBinder{}).HandleMediaIndexSweep(t.Context()); !errors.Is(err, jobs.ErrMediaIndexSweepHandlerUnavailable) {
		t.Fatalf("未绑定 error = %v", err)
	}

	boom := errors.New("恢复扫描失败")
	binder := &mediaIndexSweepHandlerBinder{}
	if err := binder.Bind(&mediaIndexSweepServiceFake{err: boom}); err != nil {
		t.Fatalf("Bind() error = %v", err)
	}
	if err := binder.HandleMediaIndexSweep(t.Context()); !errors.Is(err, boom) {
		t.Fatalf("处理器错误 = %v", err)
	}
}

func TestMediaIndexEnqueuerGuards(t *testing.T) {
	t.Parallel()

	requestID := uuid.New()
	// 没有 River 管理器时如实报「处理器未配置」，不能静默丢掉确认请求；
	// 这个检查先于参数校验，缺管理器时也不会把请求当成合法任务。
	if err := (mediaIndexEnqueuer{}).Enqueue(t.Context(), requestID); !errors.Is(err, jobs.ErrMediaIndexHandlerUnavailable) {
		t.Fatalf("nil manager error = %v", err)
	}
	if err := (mediaIndexEnqueuer{}).Enqueue(t.Context(), uuid.Nil); !errors.Is(err, jobs.ErrMediaIndexHandlerUnavailable) {
		t.Fatalf("nil manager + 空 ID error = %v", err)
	}

	// 有管理器（但未接 River）时，参数与上下文校验仍然生效。
	zeroManager := mediaIndexEnqueuer{manager: &jobs.Manager{}}
	if err := zeroManager.Enqueue(t.Context(), uuid.Nil); !errors.Is(err, jobs.ErrInvalidMediaIndexArgs) {
		t.Fatalf("空请求 ID error = %v", err)
	}
	if err := zeroManager.Enqueue(nil, requestID); !errors.Is(err, jobs.ErrMediaIndexEnqueue) {
		t.Fatalf("nil ctx error = %v", err)
	}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if err := zeroManager.Enqueue(cancelled, requestID); !errors.Is(err, context.Canceled) {
		t.Fatalf("已取消 error = %v", err)
	}
	if err := zeroManager.Enqueue(t.Context(), requestID); !errors.Is(err, jobs.ErrMediaIndexHandlerUnavailable) {
		t.Fatalf("未接 River error = %v", err)
	}
}

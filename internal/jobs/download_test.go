package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

func TestDownloadArgsKind(t *testing.T) {
	t.Parallel()

	if kind := (DownloadArgs{}).Kind(); kind != "download_intent" {
		t.Fatalf("Kind() = %q, want download_intent", kind)
	}
}

func TestDownloadArgsJSONContainsOnlySafeReferences(t *testing.T) {
	t.Parallel()

	args := DownloadArgs{Username: "fixture-user", IntentID: uuid.MustParse("3cbad8fb-b6ea-4a79-bf41-995621164e48")}
	encoded, err := json.Marshal(args)
	if err != nil {
		t.Fatal("编码下载任务参数失败")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal("解析下载任务参数失败")
	}
	if len(fields) != 2 || fields["username"] == nil || fields["intent_id"] == nil {
		t.Fatal("下载任务参数包含非安全引用或缺少必要引用")
	}
	for _, forbidden := range []string{"url", "cookie", "passkey", "metainfo"} {
		if strings.Contains(strings.ToLower(string(encoded)), forbidden) {
			t.Fatal("下载任务参数包含禁止持久化的敏感字段")
		}
	}
}

func TestDownloadArgsValidate(t *testing.T) {
	t.Parallel()

	validID := uuid.MustParse("c92beaa5-1bc1-43ef-9b81-28228d529d90")
	tests := []struct {
		name    string
		args    DownloadArgs
		wantErr bool
	}{
		{
			name: "有效安全引用",
			args: DownloadArgs{Username: "fixture-user", IntentID: validID},
		},
		{
			name: "允许待规范化用户名",
			args: DownloadArgs{Username: " fixture-user ", IntentID: validID},
		},
		{
			name:    "拒绝空用户名",
			args:    DownloadArgs{Username: " \t ", IntentID: validID},
			wantErr: true,
		},
		{
			name:    "拒绝空意图 UUID",
			args:    DownloadArgs{Username: "fixture-user", IntentID: uuid.Nil},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := tt.args.Validate()
			if tt.wantErr {
				if !errors.Is(err, ErrInvalidDownloadArgs) {
					t.Fatal("无效下载任务参数未返回稳定错误")
				}
				return
			}
			if err != nil {
				t.Fatal("有效下载任务参数被拒绝")
			}
		})
	}
}

func TestDownloadArgsInsertOpts(t *testing.T) {
	t.Parallel()

	opts := (DownloadArgs{}).InsertOpts()
	if opts.Queue != "download" {
		t.Fatalf("Queue = %q, want download", opts.Queue)
	}
	if opts.MaxAttempts != downloadMaxAttempts || opts.MaxAttempts <= 1 {
		t.Fatal("下载任务未配置有限重试")
	}
	if !opts.UniqueOpts.ByArgs || opts.UniqueOpts.ByQueue || opts.UniqueOpts.ByPeriod != 0 {
		t.Fatal("下载任务唯一选项不符合按意图参数全局唯一约束")
	}
}

func TestRegisterDownloadWorkerBoundsConcurrency(t *testing.T) {
	t.Parallel()

	handler := downloadHandlerFunc(func(context.Context, DownloadArgs) error { return nil })
	tests := []struct {
		name       string
		config     ManagerConfig
		wantWorker int
		wantErr    bool
	}{
		{
			name: "默认构造器不启用下载队列",
		},
		{
			name:    "无处理器时拒绝并发配置",
			config:  ManagerConfig{DownloadMaxWorkers: 1},
			wantErr: true,
		},
		{
			name:       "处理器使用默认并发",
			config:     ManagerConfig{DownloadHandler: handler},
			wantWorker: downloadDefaultMaxWorkers,
		},
		{
			name:       "保留上限内并发",
			config:     ManagerConfig{DownloadHandler: handler, DownloadMaxWorkers: 3},
			wantWorker: 3,
		},
		{
			name:       "超过硬上限时钳制",
			config:     ManagerConfig{DownloadHandler: handler, DownloadMaxWorkers: 100},
			wantWorker: downloadMaxWorkersLimit,
		},
		{
			name:    "拒绝负并发",
			config:  ManagerConfig{DownloadHandler: handler, DownloadMaxWorkers: -1},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			maxWorkers, err := registerDownloadWorker(river.NewWorkers(), tt.config)
			if tt.wantErr {
				if err == nil {
					t.Fatal("无效下载 worker 配置未被拒绝")
				}
				return
			}
			if err != nil {
				t.Fatal("有效下载 worker 配置被拒绝")
			}
			if maxWorkers != tt.wantWorker {
				t.Fatalf("MaxWorkers = %d, want %d", maxWorkers, tt.wantWorker)
			}
		})
	}
}

func TestDownloadWorkerForwardsSafeReferences(t *testing.T) {
	t.Parallel()

	type contextKey struct{}
	intentID := uuid.MustParse("416d2475-bb69-41d3-b7fd-0e37cf3daa36")
	ctx := context.WithValue(context.Background(), contextKey{}, "context-fixture")
	var receivedArgs DownloadArgs
	var receivedContextValue any
	worker := &downloadWorker{handler: downloadHandlerFunc(func(handlerCtx context.Context, args DownloadArgs) error {
		receivedArgs = args
		receivedContextValue = handlerCtx.Value(contextKey{})
		return nil
	})}
	job := &river.Job[DownloadArgs]{Args: DownloadArgs{Username: " fixture-user ", IntentID: intentID}}

	if err := worker.Work(ctx, job); err != nil {
		t.Fatal("下载 worker 转发失败")
	}
	if receivedArgs.Username != "fixture-user" || receivedArgs.IntentID != intentID {
		t.Fatal("下载 worker 未转发规范化安全引用")
	}
	if receivedContextValue != "context-fixture" {
		t.Fatal("下载 worker 未转发调用上下文")
	}
	if timeout := worker.Timeout(job); timeout != downloadJobTimeout || timeout <= 0 {
		t.Fatal("下载 worker 未配置独立超时")
	}
}

func TestDownloadWorkerRejectsInvalidArgsBeforeHandler(t *testing.T) {
	t.Parallel()

	validID := uuid.MustParse("ebf52e9d-2572-4228-8863-ae29e84be8a0")
	tests := []struct {
		name string
		job  *river.Job[DownloadArgs]
	}{
		{name: "空任务"},
		{name: "空用户名", job: &river.Job[DownloadArgs]{Args: DownloadArgs{IntentID: validID}}},
		{name: "空意图 UUID", job: &river.Job[DownloadArgs]{Args: DownloadArgs{Username: "fixture-user"}}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			called := false
			worker := &downloadWorker{handler: downloadHandlerFunc(func(context.Context, DownloadArgs) error {
				called = true
				return nil
			})}
			err := worker.Work(context.Background(), tt.job)
			if !errors.Is(err, ErrInvalidDownloadArgs) {
				t.Fatal("worker 未拒绝无效安全引用")
			}
			if called {
				t.Fatal("无效任务仍调用了下载处理器")
			}
		})
	}
}

func TestDownloadWorkerCancellation(t *testing.T) {
	t.Parallel()

	const sensitiveFailure = "cookie=must-not-appear url=https://secret.example/passkey"
	args := DownloadArgs{
		Username: "fixture-user",
		IntentID: uuid.MustParse("d34b50f8-81ed-4fd3-b0de-c36168d2f41e"),
	}
	tests := []struct {
		name            string
		cancelBeforeRun bool
		cancelInHandler bool
		wantHandlerCall bool
	}{
		{name: "预取消上下文", cancelBeforeRun: true},
		{name: "处理器期间取消", cancelInHandler: true, wantHandlerCall: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			called := false
			worker := &downloadWorker{handler: downloadHandlerFunc(func(handlerCtx context.Context, _ DownloadArgs) error {
				called = true
				if tt.cancelInHandler {
					cancel()
				}
				<-handlerCtx.Done()
				return fmt.Errorf("%s: %w", sensitiveFailure, handlerCtx.Err())
			})}
			if tt.cancelBeforeRun {
				cancel()
			}

			err := worker.Work(ctx, &river.Job[DownloadArgs]{Args: args})
			if !errors.Is(err, context.Canceled) {
				t.Fatal("下载 worker 未传播 context 取消")
			}
			if called != tt.wantHandlerCall {
				t.Fatal("下载 worker 取消时的处理器调用不符合预期")
			}
			for _, sensitiveValue := range []string{"must-not-appear", "https://secret.example", "passkey"} {
				if strings.Contains(err.Error(), sensitiveValue) {
					t.Fatal("context 取消错误泄露了处理器敏感信息")
				}
			}
		})
	}
}

func TestDownloadWorkerSanitizesRetryableFailure(t *testing.T) {
	t.Parallel()

	const sensitiveFailure = "url=https://secret.example cookie=secret passkey=secret metainfo=secret"
	worker := &downloadWorker{handler: downloadHandlerFunc(func(context.Context, DownloadArgs) error {
		return errors.New(sensitiveFailure)
	})}
	job := &river.Job[DownloadArgs]{Args: DownloadArgs{
		Username: "fixture-user",
		IntentID: uuid.MustParse("4a73cbf4-55a9-4354-a44b-b1888ca7e3f3"),
	}}

	err := worker.Work(context.Background(), job)
	if !errors.Is(err, ErrDownloadExecution) {
		t.Fatal("处理器失败未返回可重试的稳定错误")
	}
	for _, sensitiveValue := range []string{"https://secret.example", "cookie=secret", "passkey=secret", "metainfo=secret"} {
		if strings.Contains(err.Error(), sensitiveValue) {
			t.Fatal("下载 worker 错误泄露了处理器敏感信息")
		}
	}
}

func TestDownloadWorkerAppliesStructuredResult(t *testing.T) {
	t.Parallel()

	args := DownloadArgs{
		Username: "fixture-user",
		IntentID: uuid.MustParse("908c6b86-376f-47a2-8307-8512854f75af"),
	}
	tests := []struct {
		name             string
		result           DownloadResult
		handlerErr       error
		wantErr          error
		wantSnooze       time.Duration
		wantCancel       bool
		forbiddenInError string
	}{
		{name: "完成", result: CompleteDownload()},
		{
			name:             "重试错误脱敏",
			result:           RetryDownload(),
			handlerErr:       errors.New("cookie=secret-passkey"),
			wantErr:          ErrDownloadExecution,
			forbiddenInError: "secret-passkey",
		},
		{name: "显式延后对账", result: SnoozeDownload(2 * time.Minute), wantSnooze: 2 * time.Minute},
		{name: "零时长使用默认延后", result: SnoozeDownload(0), wantSnooze: downloadDefaultSnooze},
		{name: "拒绝负延后", result: SnoozeDownload(-time.Second), wantErr: ErrDownloadExecution},
		{name: "拒绝过长延后", result: SnoozeDownload(downloadMaxSnooze + time.Second), wantErr: ErrDownloadExecution},
		{name: "业务取消", result: CancelDownload(), wantCancel: true},
		{name: "拒绝未知动作", result: DownloadResult{Action: DownloadAction(255)}, wantErr: ErrDownloadExecution},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			worker := &downloadWorker{handler: downloadResultHandlerFunc(func(context.Context, DownloadArgs) (DownloadResult, error) {
				return tt.result, tt.handlerErr
			})}
			err := worker.Work(t.Context(), &river.Job[DownloadArgs]{Args: args})
			switch {
			case tt.wantSnooze > 0:
				var snoozeErr *river.JobSnoozeError
				if !errors.As(err, &snoozeErr) || snoozeErr.Duration != tt.wantSnooze {
					t.Fatalf("Work() error = %v, want snooze %s", err, tt.wantSnooze)
				}
			case tt.wantCancel:
				var cancelErr *river.JobCancelError
				if !errors.As(err, &cancelErr) || !errors.Is(err, ErrDownloadCancelled) {
					t.Fatalf("Work() error = %v, want business cancel", err)
				}
			case tt.wantErr != nil:
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("Work() error = %v, want %v", err, tt.wantErr)
				}
			default:
				if err != nil {
					t.Fatalf("Work() error = %v", err)
				}
			}
			if tt.forbiddenInError != "" && err != nil && strings.Contains(err.Error(), tt.forbiddenInError) {
				t.Fatal("结构化结果错误泄露处理器输入")
			}
		})
	}
}

func TestDownloadWorkerRecoversOnlyAfterFinalRetry(t *testing.T) {
	t.Parallel()

	args := DownloadArgs{
		Username: " fixture-user ",
		IntentID: uuid.MustParse("b474f22a-3660-414f-916a-c83f099dfc55"),
	}
	tests := []struct {
		name         string
		attempt      int
		wantRecovery bool
		wantCancel   bool
	}{
		{
			name:    "非最终尝试保留 River 重试",
			attempt: downloadMaxAttempts - 1,
		},
		{
			name:         "最终尝试恢复持久意图并取消任务",
			attempt:      downloadMaxAttempts,
			wantRecovery: true,
			wantCancel:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var recoveryCalls int
			var recoveredArgs DownloadArgs
			handler := &downloadRetryExhaustedHandler{
				handle: func(context.Context, DownloadArgs) (DownloadResult, error) {
					return RetryDownload(), errors.New("可重试下载失败")
				},
				recover: func(_ context.Context, got DownloadArgs) error {
					recoveryCalls++
					recoveredArgs = got
					return nil
				},
			}
			worker := &downloadWorker{handler: handler}
			job := &river.Job[DownloadArgs]{
				JobRow: &rivertype.JobRow{
					Attempt:     tt.attempt,
					MaxAttempts: downloadMaxAttempts,
				},
				Args: args,
			}

			err := worker.Work(t.Context(), job)
			if tt.wantCancel {
				var cancelErr *river.JobCancelError
				if !errors.As(err, &cancelErr) || !errors.Is(err, ErrDownloadCancelled) {
					t.Fatalf("Work() error = %v, want River cancellation", err)
				}
			} else if !errors.Is(err, ErrDownloadExecution) {
				t.Fatalf("Work() error = %v, want retryable %v", err, ErrDownloadExecution)
			}
			if got := recoveryCalls; got != boolToInt(tt.wantRecovery) {
				t.Fatalf("recovery calls = %d, want %d", got, boolToInt(tt.wantRecovery))
			}
			if tt.wantRecovery && (recoveredArgs.Username != "fixture-user" || recoveredArgs.IntentID != args.IntentID) {
				t.Fatalf("recovery args = %+v, want normalized safe references", recoveredArgs)
			}
		})
	}
}

func TestDownloadWorkerPreservesRetryRecoveryFailure(t *testing.T) {
	t.Parallel()

	recoveryErr := errors.New("恢复下载意图失败")
	handler := &downloadRetryExhaustedHandler{
		handle: func(context.Context, DownloadArgs) (DownloadResult, error) {
			return RetryDownload(), errors.New("下载资源解析失败")
		},
		recover: func(context.Context, DownloadArgs) error {
			return recoveryErr
		},
	}
	worker := &downloadWorker{handler: handler}
	job := &river.Job[DownloadArgs]{
		JobRow: &rivertype.JobRow{
			Attempt:     downloadMaxAttempts,
			MaxAttempts: downloadMaxAttempts,
		},
		Args: DownloadArgs{
			Username: "fixture-user",
			IntentID: uuid.MustParse("35199628-b421-4387-ab6c-e24b85191af6"),
		},
	}

	err := worker.Work(t.Context(), job)
	if !errors.Is(err, ErrDownloadExecution) {
		t.Fatalf("Work() error = %v, want stable %v", err, ErrDownloadExecution)
	}
	if !errors.Is(err, recoveryErr) {
		t.Fatalf("Work() error = %v, want recovery error in chain", err)
	}
	if strings.Contains(err.Error(), recoveryErr.Error()) {
		t.Fatal("下载任务错误文案泄露了底层恢复失败详情")
	}
	var cancelErr *river.JobCancelError
	if errors.As(err, &cancelErr) {
		t.Fatal("恢复失败时任务不应取消")
	}
}

func TestDownloadQueueTimingMaintainsLeaseSafetyMargin(t *testing.T) {
	t.Parallel()

	if downloadRescueAfter <= downloadJobTimeout {
		t.Fatalf("RescueStuckJobsAfter = %s, worker timeout = %s", downloadRescueAfter, downloadJobTimeout)
	}
	if margin := downloadRescueAfter - downloadJobTimeout; margin < 5*time.Minute {
		t.Fatalf("下载任务救援安全余量仅 %s", margin)
	}
}

func TestManagerEnqueueDownloadValidatesWithoutDatabase(t *testing.T) {
	t.Parallel()

	manager := &Manager{}
	validArgs := DownloadArgs{
		Username: "fixture-user",
		IntentID: uuid.MustParse("85265f4a-16da-4b30-8546-2edcd0ba259a"),
	}
	tests := []struct {
		name    string
		ctx     context.Context
		args    DownloadArgs
		wantErr error
	}{
		{name: "无效参数优先拒绝", ctx: context.Background(), args: DownloadArgs{}, wantErr: ErrInvalidDownloadArgs},
		{name: "未配置处理器", ctx: context.Background(), args: validArgs, wantErr: ErrDownloadHandlerUnavailable},
		{name: "已取消上下文", ctx: canceledDownloadContext(), args: validArgs, wantErr: context.Canceled},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if err := manager.EnqueueDownload(tt.ctx, tt.args); !errors.Is(err, tt.wantErr) {
				t.Fatal("下载任务入队前置校验结果不符合预期")
			}
		})
	}
}

func TestManagerEnqueueDownloadTxValidatesWithoutDatabase(t *testing.T) {
	t.Parallel()

	manager := &Manager{}
	validArgs := DownloadArgs{
		Username: "fixture-user",
		IntentID: uuid.MustParse("8e8cbe1d-0aa6-443a-8e39-fdf409b87764"),
	}
	tests := []struct {
		name    string
		ctx     context.Context
		args    DownloadArgs
		wantErr error
	}{
		{name: "无效参数优先拒绝", ctx: context.Background(), args: DownloadArgs{}, wantErr: ErrInvalidDownloadArgs},
		{name: "空事务", ctx: context.Background(), args: validArgs, wantErr: ErrDownloadEnqueue},
		{name: "已取消上下文", ctx: canceledDownloadContext(), args: validArgs, wantErr: context.Canceled},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if err := manager.EnqueueDownloadTx(tt.ctx, nil, tt.args); !errors.Is(err, tt.wantErr) {
				t.Fatalf("EnqueueDownloadTx() error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

type downloadHandlerFunc func(context.Context, DownloadArgs) error

func (f downloadHandlerFunc) HandleDownload(ctx context.Context, args DownloadArgs) (DownloadResult, error) {
	return CompleteDownload(), f(ctx, args)
}

type downloadResultHandlerFunc func(context.Context, DownloadArgs) (DownloadResult, error)

func (f downloadResultHandlerFunc) HandleDownload(ctx context.Context, args DownloadArgs) (DownloadResult, error) {
	return f(ctx, args)
}

type downloadRetryExhaustedHandler struct {
	handle  func(context.Context, DownloadArgs) (DownloadResult, error)
	recover func(context.Context, DownloadArgs) error
}

func (h *downloadRetryExhaustedHandler) HandleDownload(ctx context.Context, args DownloadArgs) (DownloadResult, error) {
	return h.handle(ctx, args)
}

func (h *downloadRetryExhaustedHandler) HandleDownloadRetryExhausted(ctx context.Context, args DownloadArgs) error {
	return h.recover(ctx, args)
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func canceledDownloadContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

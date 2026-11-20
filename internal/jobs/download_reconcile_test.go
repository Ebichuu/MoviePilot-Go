package jobs

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

func TestDownloadReconcileSweepArgsContract(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 11, 10, 0, 0, 0, time.UTC)
	tests := []struct {
		name    string
		args    DownloadReconcileSweepArgs
		wantErr error
	}{
		{name: "有效快照", args: DownloadReconcileSweepArgs{Key: downloadReconcileKey, Cutoff: now}},
		{name: "拒绝空键", args: DownloadReconcileSweepArgs{Cutoff: now}, wantErr: ErrInvalidDownloadReconcileArgs},
		{name: "拒绝未知键", args: DownloadReconcileSweepArgs{Key: "other", Cutoff: now}, wantErr: ErrInvalidDownloadReconcileArgs},
		{name: "拒绝空截止时间", args: DownloadReconcileSweepArgs{Key: downloadReconcileKey}, wantErr: ErrInvalidDownloadReconcileArgs},
		{name: "拒绝未来截止时间", args: DownloadReconcileSweepArgs{Key: downloadReconcileKey, Cutoff: now.Add(time.Minute + time.Nanosecond)}, wantErr: ErrInvalidDownloadReconcileArgs},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if err := tt.args.Validate(now); !errors.Is(err, tt.wantErr) {
				t.Fatalf("Validate() error = %v, want %v", err, tt.wantErr)
			}
		})
	}
	if kind := (DownloadReconcileSweepArgs{}).Kind(); kind != downloadReconcileKind {
		t.Fatalf("Kind() = %q", kind)
	}
}

func TestDownloadReconcileSweepUniqueStatesExcludeHistory(t *testing.T) {
	t.Parallel()

	opts := (DownloadReconcileSweepArgs{}).InsertOpts()
	if opts.Queue != queueDownloadReconcile || opts.MaxAttempts != downloadReconcileMaxAttempts || !opts.UniqueOpts.ByArgs {
		t.Fatalf("InsertOpts() = %#v", opts)
	}
	for _, state := range []rivertype.JobState{
		rivertype.JobStateAvailable,
		rivertype.JobStatePending,
		rivertype.JobStateRetryable,
		rivertype.JobStateRunning,
		rivertype.JobStateScheduled,
	} {
		if !slices.Contains(opts.UniqueOpts.ByState, state) {
			t.Fatalf("活跃唯一状态缺少 %s", state)
		}
	}
	for _, state := range []rivertype.JobState{
		rivertype.JobStateCompleted,
		rivertype.JobStateCancelled,
		rivertype.JobStateDiscarded,
	} {
		if slices.Contains(opts.UniqueOpts.ByState, state) {
			t.Fatalf("历史状态 %s 不应阻塞下一轮扫描", state)
		}
	}
}

func TestDownloadReconcileWorkerForwardsFixedCutoff(t *testing.T) {
	t.Parallel()

	cutoff := time.Date(2026, 8, 11, 9, 59, 0, 0, time.FixedZone("fixture", 8*60*60))
	now := cutoff.Add(time.Minute).UTC()
	var received time.Time
	worker := &downloadReconcileWorker{
		handler: downloadReconcileHandlerFunc(func(_ context.Context, got time.Time) error {
			received = got
			return nil
		}),
		now: func() time.Time { return now },
	}
	err := worker.Work(t.Context(), &river.Job[DownloadReconcileSweepArgs]{
		Args: DownloadReconcileSweepArgs{Key: downloadReconcileKey, Cutoff: cutoff},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !received.Equal(cutoff) || received.Location() != time.UTC {
		t.Fatalf("handler cutoff = %v", received)
	}
}

func TestDownloadReconcileWorkerSanitizesFailures(t *testing.T) {
	t.Parallel()

	const sensitiveMarker = "upstream-sensitive-marker"
	cutoff := time.Now().Add(-time.Minute).UTC()
	tests := []struct {
		name    string
		worker  *downloadReconcileWorker
		job     *river.Job[DownloadReconcileSweepArgs]
		wantErr error
	}{
		{name: "缺少处理器", worker: &downloadReconcileWorker{}, job: &river.Job[DownloadReconcileSweepArgs]{Args: DownloadReconcileSweepArgs{Key: downloadReconcileKey, Cutoff: cutoff}}, wantErr: ErrDownloadReconcileHandlerUnavailable},
		{name: "空任务", worker: &downloadReconcileWorker{handler: downloadReconcileHandlerFunc(func(context.Context, time.Time) error { return nil })}, wantErr: ErrInvalidDownloadReconcileArgs},
		{name: "业务错误脱敏", worker: &downloadReconcileWorker{handler: downloadReconcileHandlerFunc(func(context.Context, time.Time) error { return errors.New(sensitiveMarker) })}, job: &river.Job[DownloadReconcileSweepArgs]{Args: DownloadReconcileSweepArgs{Key: downloadReconcileKey, Cutoff: cutoff}}, wantErr: ErrDownloadReconcileExecution},
		{name: "透传取消", worker: &downloadReconcileWorker{handler: downloadReconcileHandlerFunc(func(context.Context, time.Time) error { return context.Canceled })}, job: &river.Job[DownloadReconcileSweepArgs]{Args: DownloadReconcileSweepArgs{Key: downloadReconcileKey, Cutoff: cutoff}}, wantErr: context.Canceled},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := tt.worker.Work(t.Context(), tt.job)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Work() error = %v, want %v", err, tt.wantErr)
			}
			if err != nil && strings.Contains(err.Error(), sensitiveMarker) {
				t.Fatal("River 错误泄露下载器地址或凭据")
			}
		})
	}
}

func TestDownloadReconcileWorkerRegistrationAndTiming(t *testing.T) {
	t.Parallel()

	workers := river.NewWorkers()
	enabled, err := registerDownloadReconcileWorker(workers, nil)
	if err != nil || enabled {
		t.Fatalf("nil registration = %v, %v", enabled, err)
	}
	enabled, err = registerDownloadReconcileWorker(workers, downloadReconcileHandlerFunc(func(context.Context, time.Time) error { return nil }))
	if err != nil || !enabled {
		t.Fatalf("handler registration = %v, %v", enabled, err)
	}
	if downloadReconcileInterval < time.Minute {
		t.Fatalf("periodic interval = %s", downloadReconcileInterval)
	}
	worker := &downloadReconcileWorker{}
	if got := worker.Timeout(nil); got != downloadReconcileJobTimeout || got < time.Minute {
		t.Fatalf("reconcile timeout = %s", got)
	}
	if downloadRescueAfter <= downloadReconcileJobTimeout {
		t.Fatalf("rescue timeout = %s, reconcile timeout = %s", downloadRescueAfter, downloadReconcileJobTimeout)
	}
	args, opts := downloadReconcilePeriodicArgs()
	sweep, ok := args.(DownloadReconcileSweepArgs)
	if !ok || sweep.Key != downloadReconcileKey || sweep.Cutoff.IsZero() || opts == nil || opts.Queue != queueDownloadReconcile {
		t.Fatalf("periodic constructor = %#v, %#v", args, opts)
	}
}

type downloadReconcileHandlerFunc func(context.Context, time.Time) error

func (f downloadReconcileHandlerFunc) HandleDownloadReconciliation(ctx context.Context, cutoff time.Time) error {
	return f(ctx, cutoff)
}

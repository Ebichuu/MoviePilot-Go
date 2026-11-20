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

func TestSubscriptionSearchSweepArgsContract(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name    string
		args    SubscriptionSearchSweepArgs
		wantErr error
	}{
		{name: "有效快照", args: SubscriptionSearchSweepArgs{Key: subscriptionSearchKey, Cutoff: now}},
		{name: "空键", args: SubscriptionSearchSweepArgs{Cutoff: now}, wantErr: ErrInvalidSubscriptionSearchArgs},
		{name: "未知键", args: SubscriptionSearchSweepArgs{Key: "other", Cutoff: now}, wantErr: ErrInvalidSubscriptionSearchArgs},
		{name: "空截止时间", args: SubscriptionSearchSweepArgs{Key: subscriptionSearchKey}, wantErr: ErrInvalidSubscriptionSearchArgs},
		{name: "未来截止时间", args: SubscriptionSearchSweepArgs{Key: subscriptionSearchKey, Cutoff: now.Add(time.Minute + time.Nanosecond)}, wantErr: ErrInvalidSubscriptionSearchArgs},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if err := tt.args.Validate(now); !errors.Is(err, tt.wantErr) {
				t.Fatalf("Validate() error = %v, want %v", err, tt.wantErr)
			}
		})
	}
	if kind := (SubscriptionSearchSweepArgs{}).Kind(); kind != subscriptionSearchKind {
		t.Fatalf("Kind() = %q", kind)
	}
}

func TestSubscriptionSearchSweepUniqueStatesExcludeHistory(t *testing.T) {
	t.Parallel()
	opts := (SubscriptionSearchSweepArgs{}).InsertOpts()
	if opts.Queue != queueSubscriptionSearch || opts.MaxAttempts != subscriptionSearchMaxAttempts || !opts.UniqueOpts.ByArgs {
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
			t.Fatalf("历史状态 %s 不应阻塞下一轮", state)
		}
	}
}

func TestSubscriptionSearchWorkerForwardsFixedCutoff(t *testing.T) {
	t.Parallel()
	cutoff := time.Date(2026, 8, 11, 11, 59, 0, 0, time.FixedZone("fixture", 8*60*60))
	var received time.Time
	worker := &subscriptionSearchWorker{
		handler: subscriptionSearchHandlerFunc(func(_ context.Context, got time.Time) error {
			received = got
			return nil
		}),
		now: func() time.Time { return cutoff.Add(time.Minute).UTC() },
	}
	err := worker.Work(t.Context(), &river.Job[SubscriptionSearchSweepArgs]{
		Args: SubscriptionSearchSweepArgs{Key: subscriptionSearchKey, Cutoff: cutoff},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !received.Equal(cutoff) || received.Location() != time.UTC {
		t.Fatalf("handler cutoff = %v", received)
	}
}

func TestSubscriptionSearchWorkerSanitizesFailures(t *testing.T) {
	t.Parallel()
	const sensitiveMarker = "sensitive-site-cookie"
	cutoff := time.Now().Add(-time.Minute).UTC()
	tests := []struct {
		name    string
		worker  *subscriptionSearchWorker
		job     *river.Job[SubscriptionSearchSweepArgs]
		wantErr error
	}{
		{name: "缺少处理器", worker: &subscriptionSearchWorker{}, job: &river.Job[SubscriptionSearchSweepArgs]{Args: SubscriptionSearchSweepArgs{Key: subscriptionSearchKey, Cutoff: cutoff}}, wantErr: ErrSubscriptionSearchUnavailable},
		{name: "空任务", worker: &subscriptionSearchWorker{handler: subscriptionSearchHandlerFunc(func(context.Context, time.Time) error { return nil })}, wantErr: ErrInvalidSubscriptionSearchArgs},
		{name: "业务错误脱敏", worker: &subscriptionSearchWorker{handler: subscriptionSearchHandlerFunc(func(context.Context, time.Time) error { return errors.New(sensitiveMarker) })}, job: &river.Job[SubscriptionSearchSweepArgs]{Args: SubscriptionSearchSweepArgs{Key: subscriptionSearchKey, Cutoff: cutoff}}, wantErr: ErrSubscriptionSearchExecution},
		{name: "透传取消", worker: &subscriptionSearchWorker{handler: subscriptionSearchHandlerFunc(func(context.Context, time.Time) error { return context.Canceled })}, job: &river.Job[SubscriptionSearchSweepArgs]{Args: SubscriptionSearchSweepArgs{Key: subscriptionSearchKey, Cutoff: cutoff}}, wantErr: context.Canceled},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := tt.worker.Work(t.Context(), tt.job)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Work() error = %v, want %v", err, tt.wantErr)
			}
			if err != nil && strings.Contains(err.Error(), sensitiveMarker) {
				t.Fatal("River 错误泄露了站点凭据或地址")
			}
		})
	}
}

func TestSubscriptionSearchWorkerRegistrationAndTiming(t *testing.T) {
	t.Parallel()
	workers := river.NewWorkers()
	enabled, err := registerSubscriptionSearchWorker(workers, nil)
	if err != nil || enabled {
		t.Fatalf("nil registration = %v, %v", enabled, err)
	}
	enabled, err = registerSubscriptionSearchWorker(workers, subscriptionSearchHandlerFunc(func(context.Context, time.Time) error { return nil }))
	if err != nil || !enabled {
		t.Fatalf("handler registration = %v, %v", enabled, err)
	}
	if subscriptionSearchInterval < time.Minute {
		t.Fatalf("periodic interval = %s", subscriptionSearchInterval)
	}
	if got := (&subscriptionSearchWorker{}).Timeout(nil); got != subscriptionSearchJobTimeout || got < time.Minute {
		t.Fatalf("search timeout = %s", got)
	}
	args, opts := subscriptionSearchPeriodicArgs()
	sweep, ok := args.(SubscriptionSearchSweepArgs)
	if !ok || sweep.Key != subscriptionSearchKey || sweep.Cutoff.IsZero() || opts == nil || opts.Queue != queueSubscriptionSearch {
		t.Fatalf("periodic constructor = %#v, %#v", args, opts)
	}
}

type subscriptionSearchHandlerFunc func(context.Context, time.Time) error

func (f subscriptionSearchHandlerFunc) HandleSubscriptionSearch(ctx context.Context, cutoff time.Time) error {
	return f(ctx, cutoff)
}

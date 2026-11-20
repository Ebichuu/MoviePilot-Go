package jobs

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/riverqueue/river"
)

func TestRSSArgsKindInsertOptsAndValidation(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.August, 12, 9, 0, 0, 0, time.UTC)
	t.Run("sweep", func(t *testing.T) {
		t.Parallel()
		tests := []struct {
			name    string
			args    RSSSweepArgs
			wantErr bool
		}{
			{name: "有效", args: RSSSweepArgs{Key: rssSweepKey, Cutoff: now}},
			{name: "允许一分钟时钟漂移", args: RSSSweepArgs{Key: rssSweepKey, Cutoff: now.Add(time.Minute)}},
			{name: "空 key", args: RSSSweepArgs{Cutoff: now}, wantErr: true},
			{name: "非法 key", args: RSSSweepArgs{Key: "other", Cutoff: now}, wantErr: true},
			{name: "空 cutoff", args: RSSSweepArgs{Key: rssSweepKey}, wantErr: true},
			{name: "未来 cutoff", args: RSSSweepArgs{Key: rssSweepKey, Cutoff: now.Add(time.Minute + time.Nanosecond)}, wantErr: true},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()
				err := tt.args.Validate(now)
				if tt.wantErr {
					if !errors.Is(err, ErrInvalidRSSSweepArgs) {
						t.Fatalf("Validate() error = %v, want ErrInvalidRSSSweepArgs", err)
					}
					return
				}
				if err != nil {
					t.Fatalf("Validate() error = %v", err)
				}
			})
		}
		if got := (RSSSweepArgs{}).Kind(); got != rssSweepKind {
			t.Fatalf("Kind() = %q, want %q", got, rssSweepKind)
		}
		opts := (RSSSweepArgs{}).InsertOpts()
		if opts.Queue != queueRSSSweep || opts.MaxAttempts != rssSweepMaxAttempts || !opts.UniqueOpts.ByArgs {
			t.Fatalf("InsertOpts() = %#v, want bounded unique sweep options", opts)
		}
	})

	t.Run("poll", func(t *testing.T) {
		t.Parallel()
		tests := []struct {
			name    string
			args    RSSPollArgs
			wantErr bool
		}{
			{name: "有效", args: RSSPollArgs{SiteID: 7, Cutoff: now}},
			{name: "允许一分钟时钟漂移", args: RSSPollArgs{SiteID: 7, Cutoff: now.Add(time.Minute)}},
			{name: "零 site id", args: RSSPollArgs{Cutoff: now}, wantErr: true},
			{name: "负 site id", args: RSSPollArgs{SiteID: -1, Cutoff: now}, wantErr: true},
			{name: "空 cutoff", args: RSSPollArgs{SiteID: 7}, wantErr: true},
			{name: "未来 cutoff", args: RSSPollArgs{SiteID: 7, Cutoff: now.Add(time.Minute + time.Nanosecond)}, wantErr: true},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()
				err := tt.args.Validate(now)
				if tt.wantErr {
					if !errors.Is(err, ErrInvalidRSSPollArgs) {
						t.Fatalf("Validate() error = %v, want ErrInvalidRSSPollArgs", err)
					}
					return
				}
				if err != nil {
					t.Fatalf("Validate() error = %v", err)
				}
			})
		}
		if got := (RSSPollArgs{}).Kind(); got != rssPollKind {
			t.Fatalf("Kind() = %q, want %q", got, rssPollKind)
		}
		opts := (RSSPollArgs{}).InsertOpts()
		if opts.Queue != queueRSSPoll || opts.MaxAttempts != rssPollMaxAttempts || !opts.UniqueOpts.ByArgs {
			t.Fatalf("InsertOpts() = %#v, want bounded unique poll options", opts)
		}
	})

	t.Run("match", func(t *testing.T) {
		t.Parallel()
		atLimit := make([]int, RSSSweepPageSize)
		for index := range atLimit {
			atLimit[index] = index + 1
		}
		overLimit := append(append([]int(nil), atLimit...), RSSSweepPageSize+1)
		tests := []struct {
			name    string
			args    RSSMatchArgs
			wantErr bool
		}{
			{name: "有效", args: RSSMatchArgs{Key: rssMatchKey, Cutoff: now, ExpectedSiteIDs: []int{1, 7, 9}}},
			{name: "允许空屏障", args: RSSMatchArgs{Key: rssMatchKey, Cutoff: now}},
			{name: "允许扫描页上限", args: RSSMatchArgs{Key: rssMatchKey, Cutoff: now, ExpectedSiteIDs: atLimit}},
			{name: "允许一分钟时钟漂移", args: RSSMatchArgs{Key: rssMatchKey, Cutoff: now.Add(time.Minute), ExpectedSiteIDs: []int{7}}},
			{name: "空 key", args: RSSMatchArgs{Cutoff: now}, wantErr: true},
			{name: "非法 key", args: RSSMatchArgs{Key: "other", Cutoff: now}, wantErr: true},
			{name: "key 不允许空白变体", args: RSSMatchArgs{Key: " " + rssMatchKey + " ", Cutoff: now}, wantErr: true},
			{name: "空 cutoff", args: RSSMatchArgs{Key: rssMatchKey}, wantErr: true},
			{name: "未来 cutoff", args: RSSMatchArgs{Key: rssMatchKey, Cutoff: now.Add(time.Minute + time.Nanosecond)}, wantErr: true},
			{name: "站点为零", args: RSSMatchArgs{Key: rssMatchKey, Cutoff: now, ExpectedSiteIDs: []int{0}}, wantErr: true},
			{name: "站点为负", args: RSSMatchArgs{Key: rssMatchKey, Cutoff: now, ExpectedSiteIDs: []int{-1}}, wantErr: true},
			{name: "站点重复", args: RSSMatchArgs{Key: rssMatchKey, Cutoff: now, ExpectedSiteIDs: []int{1, 1}}, wantErr: true},
			{name: "站点未排序", args: RSSMatchArgs{Key: rssMatchKey, Cutoff: now, ExpectedSiteIDs: []int{2, 1}}, wantErr: true},
			{name: "站点数量超过扫描页", args: RSSMatchArgs{Key: rssMatchKey, Cutoff: now, ExpectedSiteIDs: overLimit}, wantErr: true},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()
				err := tt.args.Validate(now)
				if tt.wantErr {
					if !errors.Is(err, ErrInvalidRSSMatchArgs) {
						t.Fatalf("Validate() error = %v, want ErrInvalidRSSMatchArgs", err)
					}
					return
				}
				if err != nil {
					t.Fatalf("Validate() error = %v", err)
				}
			})
		}
		if got := (RSSMatchArgs{}).Kind(); got != rssMatchKind {
			t.Fatalf("Kind() = %q, want %q", got, rssMatchKind)
		}
		opts := (RSSMatchArgs{}).InsertOpts()
		if opts.Queue != queueRSSMatch || opts.MaxAttempts != rssMatchMaxAttempts || !opts.UniqueOpts.ByArgs {
			t.Fatalf("InsertOpts() = %#v, want bounded unique match options", opts)
		}
	})
}

func TestRSSMatchArgsUniqueFieldsPreserveEachCutoff(t *testing.T) {
	t.Parallel()

	typeOfArgs := reflect.TypeFor[RSSMatchArgs]()
	for _, fieldName := range []string{"Key", "Cutoff"} {
		field, ok := typeOfArgs.FieldByName(fieldName)
		if !ok {
			t.Fatalf("RSSMatchArgs.%s field not found", fieldName)
		}
		if got := field.Tag.Get("river"); got != "unique" {
			t.Fatalf("RSSMatchArgs.%s river tag = %q, want unique", fieldName, got)
		}
	}
	field, ok := typeOfArgs.FieldByName("ExpectedSiteIDs")
	if !ok {
		t.Fatal("RSSMatchArgs.ExpectedSiteIDs field not found")
	}
	if got := field.Tag.Get("river"); got == "unique" {
		t.Fatalf("RSSMatchArgs.ExpectedSiteIDs river tag = %q, want excluded from round identity", got)
	}
}

func TestRSSPollWorkerSnoozesBusyLease(t *testing.T) {
	t.Parallel()

	args := RSSPollArgs{SiteID: 7, Cutoff: time.Now().UTC()}
	called := false
	worker := &rssPollWorker{
		handler: rssPollHandlerFunc(func(context.Context, RSSPollArgs) error {
			called = true
			return ErrRSSLeaseBusy
		}),
		now: func() time.Time { return args.Cutoff },
	}
	err := worker.Work(context.Background(), &river.Job[RSSPollArgs]{Args: args})
	var snoozeErr *river.JobSnoozeError
	if !errors.As(err, &snoozeErr) || snoozeErr.Duration != rssPollLeaseSnooze {
		t.Fatalf("Work() error = %v, want %s snooze", err, rssPollLeaseSnooze)
	}
	if !called {
		t.Fatal("busy lease was not reported by handler")
	}
}

func TestRSSPollWorkerSanitizesFailuresAndCancellation(t *testing.T) {
	t.Parallel()

	args := RSSPollArgs{SiteID: 8, Cutoff: time.Now().UTC()}
	const sensitive = "url=https://secret.example cookie=secret passkey=secret"
	tests := []struct {
		name       string
		handlerErr error
		cancel     bool
		wantErr    error
	}{
		{
			name:       "已记录站点失败正常完成",
			handlerErr: errors.Join(ErrRSSPollFailureRecorded, errors.New(sensitive)),
		},
		{name: "持久化错误", handlerErr: errors.New(sensitive), wantErr: ErrRSSPollExecution},
		{name: "处理期间取消", cancel: true, wantErr: context.Canceled},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			worker := &rssPollWorker{
				handler: rssPollHandlerFunc(func(handlerCtx context.Context, _ RSSPollArgs) error {
					if tt.cancel {
						cancel()
						<-handlerCtx.Done()
						return errors.New("内部错误: " + sensitive)
					}
					return tt.handlerErr
				}),
				now: func() time.Time { return args.Cutoff },
			}
			err := worker.Work(ctx, &river.Job[RSSPollArgs]{Args: args})
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Work() error = %v, want %v", err, tt.wantErr)
			}
			if err != nil && (strings.Contains(err.Error(), "secret.example") || strings.Contains(err.Error(), "passkey")) {
				t.Fatal("RSS worker error exposed sensitive handler details")
			}
		})
	}
}

func TestRSSSweepWorkerForwardsUTCAndBoundsErrors(t *testing.T) {
	t.Parallel()

	cutoff := time.Date(2026, time.August, 12, 9, 0, 0, 0, time.FixedZone("CST", 8*60*60))
	var received time.Time
	worker := &rssSweepWorker{
		handler: rssSweepHandlerFunc(func(_ context.Context, got time.Time, _ RSSSweepState) (RSSSweepState, error) {
			received = got
			return RSSSweepState{Initialized: true, SiteIDs: []int{}}, nil
		}),
		now:          func() time.Time { return cutoff },
		recordOutput: func(context.Context, RSSSweepState) error { return nil },
	}
	if err := worker.Work(context.Background(), &river.Job[RSSSweepArgs]{Args: RSSSweepArgs{Key: rssSweepKey, Cutoff: cutoff}}); err != nil {
		t.Fatalf("Work() error = %v", err)
	}
	if !received.Equal(cutoff.UTC()) || received.Location() != time.UTC {
		t.Fatalf("sweep cutoff = %v, want UTC %v", received.Location(), cutoff.UTC())
	}
	if got := worker.Timeout(nil); got != rssSweepJobTimeout {
		t.Fatalf("Timeout() = %s, want %s", got, rssSweepJobTimeout)
	}

	worker.handler = rssSweepHandlerFunc(func(context.Context, time.Time, RSSSweepState) (RSSSweepState, error) {
		return RSSSweepState{}, errors.New("url=https://secret.example cookie=secret")
	})
	err := worker.Work(context.Background(), &river.Job[RSSSweepArgs]{Args: RSSSweepArgs{Key: rssSweepKey, Cutoff: cutoff}})
	if !errors.Is(err, ErrRSSSweepExecution) || strings.Contains(err.Error(), "secret.example") {
		t.Fatalf("sweep failure = %v, want sanitized execution error", err)
	}
}

func TestRSSMatchWorkerSnoozesPendingBarrier(t *testing.T) {
	t.Parallel()

	args := RSSMatchArgs{Key: rssMatchKey, Cutoff: time.Now().UTC(), ExpectedSiteIDs: []int{3, 8}}
	called := false
	worker := &rssMatchWorker{
		handler: rssMatchHandlerFunc(func(context.Context, RSSMatchArgs) error {
			called = true
			return ErrRSSMatchBarrierPending
		}),
		now: func() time.Time { return args.Cutoff },
	}
	err := worker.Work(context.Background(), &river.Job[RSSMatchArgs]{Args: args})
	var snoozeErr *river.JobSnoozeError
	if !errors.As(err, &snoozeErr) || snoozeErr.Duration != rssMatchBarrierSnooze {
		t.Fatalf("Work() error = %v, want %s snooze", err, rssMatchBarrierSnooze)
	}
	if !called {
		t.Fatal("pending barrier was not reported by handler")
	}
}

func TestRSSMatchWorkerForwardsUTCAndSanitizesFailures(t *testing.T) {
	t.Parallel()

	cutoff := time.Date(2026, time.August, 12, 9, 0, 0, 0, time.FixedZone("CST", 8*60*60))
	var received RSSMatchArgs
	worker := &rssMatchWorker{
		handler: rssMatchHandlerFunc(func(_ context.Context, args RSSMatchArgs) error {
			received = args
			return nil
		}),
		now: func() time.Time { return cutoff },
	}
	args := RSSMatchArgs{Key: rssMatchKey, Cutoff: cutoff, ExpectedSiteIDs: []int{2, 5}}
	if err := worker.Work(context.Background(), &river.Job[RSSMatchArgs]{Args: args}); err != nil {
		t.Fatalf("Work() error = %v", err)
	}
	if !received.Cutoff.Equal(cutoff.UTC()) || received.Cutoff.Location() != time.UTC {
		t.Fatalf("match cutoff = %v, want UTC %v", received.Cutoff, cutoff.UTC())
	}
	if len(received.ExpectedSiteIDs) != 2 || received.ExpectedSiteIDs[0] != 2 || received.ExpectedSiteIDs[1] != 5 {
		t.Fatalf("ExpectedSiteIDs = %v, want [2 5]", received.ExpectedSiteIDs)
	}
	if got := worker.Timeout(nil); got != rssMatchJobTimeout {
		t.Fatalf("Timeout() = %s, want %s", got, rssMatchJobTimeout)
	}

	worker.handler = rssMatchHandlerFunc(func(context.Context, RSSMatchArgs) error {
		return errors.New("url=https://secret.example cookie=secret passkey=secret")
	})
	err := worker.Work(context.Background(), &river.Job[RSSMatchArgs]{Args: args})
	if !errors.Is(err, ErrRSSMatchExecution) || strings.Contains(err.Error(), "secret.example") || strings.Contains(err.Error(), "passkey") {
		t.Fatalf("match failure = %v, want sanitized execution error", err)
	}
}

func TestRegisterRSSMatchWorker(t *testing.T) {
	t.Parallel()

	workers := river.NewWorkers()
	enabled, err := registerRSSMatchWorker(workers, nil)
	if err != nil || enabled {
		t.Fatalf("registerRSSMatchWorker(nil) = %t, %v; want false, nil", enabled, err)
	}
	enabled, err = registerRSSMatchWorker(workers, rssMatchHandlerFunc(func(context.Context, RSSMatchArgs) error { return nil }))
	if err != nil || !enabled {
		t.Fatalf("registerRSSMatchWorker(handler) = %t, %v; want true, nil", enabled, err)
	}
}

func TestNormalizeRSSSweepInterval(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		configured time.Duration
		want       time.Duration
	}{
		{name: "negative uses default", configured: -time.Minute, want: 30 * time.Minute},
		{name: "zero uses default", want: 30 * time.Minute},
		{name: "below minimum", configured: time.Minute, want: 5 * time.Minute},
		{name: "minimum", configured: 5 * time.Minute, want: 5 * time.Minute},
		{name: "configured", configured: 45 * time.Minute, want: 45 * time.Minute},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := normalizeRSSSweepInterval(tt.configured); got != tt.want {
				t.Fatalf("normalizeRSSSweepInterval(%s) = %s, want %s", tt.configured, got, tt.want)
			}
		})
	}
}

func TestRSSSweepPeriodicJobOptionsDoNotRunOnStart(t *testing.T) {
	t.Parallel()

	opts := rssSweepPeriodicJobOptions()
	if opts == nil {
		t.Fatal("rssSweepPeriodicJobOptions() = nil")
	}
	if opts.ID != rssSweepPeriodicID {
		t.Fatalf("periodic job ID = %q, want %q", opts.ID, rssSweepPeriodicID)
	}
	if opts.RunOnStart {
		t.Fatal("RSS periodic job must wait for its first interval")
	}
}

func TestRSSSweepRuntimePeriodicJobOptionsHaveNoIDAndDoNotRunOnStart(t *testing.T) {
	t.Parallel()

	opts := rssSweepRuntimePeriodicJobOptions()
	if opts == nil {
		t.Fatal("rssSweepRuntimePeriodicJobOptions() = nil")
	}
	if opts.ID != "" {
		t.Fatalf("runtime periodic job ID = %q, want empty", opts.ID)
	}
	if opts.RunOnStart {
		t.Fatal("runtime RSS periodic job must wait for its first interval")
	}
}

func TestManagerEnqueueRSSMatchBuildsAndValidatesInternalKey(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	manager := &Manager{}
	if err := manager.EnqueueRSSMatch(context.Background(), now, []int{2, 1}); !errors.Is(err, ErrInvalidRSSMatchArgs) {
		t.Fatalf("EnqueueRSSMatch(unsorted) error = %v, want ErrInvalidRSSMatchArgs", err)
	}
	if err := manager.EnqueueRSSMatch(context.Background(), now, []int{1, 2}); !errors.Is(err, ErrRSSMatchHandlerUnavailable) {
		t.Fatalf("EnqueueRSSMatch(valid) error = %v, want ErrRSSMatchHandlerUnavailable", err)
	}
}

func TestNormalizeRSSPollMaxWorkers(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   int
		want    int
		wantErr bool
	}{
		{name: "默认", want: rssPollDefaultMaxWorkers},
		{name: "自定义", input: 3, want: 3},
		{name: "硬上限", input: rssPollMaxWorkersLimit + 3, want: rssPollMaxWorkersLimit},
		{name: "负数", input: -1, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := normalizeRSSPollMaxWorkers(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatal("negative worker count was accepted")
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("normalizeRSSPollMaxWorkers(%d) = %d, %v; want %d", tt.input, got, err, tt.want)
			}
		})
	}
}

type rssSweepHandlerFunc func(context.Context, time.Time, RSSSweepState) (RSSSweepState, error)

func (f rssSweepHandlerFunc) HandleRSSSweep(ctx context.Context, cutoff time.Time, state RSSSweepState) (RSSSweepState, error) {
	return f(ctx, cutoff, state)
}

type rssPollHandlerFunc func(context.Context, RSSPollArgs) error

func (f rssPollHandlerFunc) HandleRSSPoll(ctx context.Context, args RSSPollArgs) error {
	return f(ctx, args)
}

type rssMatchHandlerFunc func(context.Context, RSSMatchArgs) error

func (f rssMatchHandlerFunc) HandleRSSMatch(ctx context.Context, args RSSMatchArgs) error {
	return f(ctx, args)
}

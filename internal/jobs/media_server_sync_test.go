package jobs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"github.com/riverqueue/river"
)

func TestMediaServerSyncArgsValidation(t *testing.T) {
	t.Parallel()
	if kind := (MediaServerSyncArgs{}).Kind(); kind != mediaServerSyncKind {
		t.Fatalf("Kind() = %q, want %q", kind, mediaServerSyncKind)
	}
	for _, args := range []MediaServerSyncArgs{
		{Key: mediaServerSyncKey},
		{Key: mediaServerSyncKey, Server: "home"},
	} {
		if err := args.Validate(); err != nil {
			t.Fatalf("Validate(%#v) error = %v", args, err)
		}
	}
	for name, args := range map[string]MediaServerSyncArgs{
		"empty key":      {Server: "home"},
		"unknown key":    {Key: "other"},
		"control server": {Key: mediaServerSyncKey, Server: "ho\nme"},
		"long server":    {Key: mediaServerSyncKey, Server: string(make([]byte, 256))},
	} {
		if err := args.Validate(); !errors.Is(err, ErrInvalidMediaServerSyncArgs) {
			t.Errorf("%s: Validate() error = %v, want %v", name, err, ErrInvalidMediaServerSyncArgs)
		}
	}
	if normalized := (MediaServerSyncArgs{Key: " run ", Server: " home "}).Normalize(); normalized.Key != mediaServerSyncKey || normalized.Server != "home" {
		t.Fatalf("Normalize() = %#v", normalized)
	}
	opts := MediaServerSyncArgs{Key: mediaServerSyncKey, Server: "home"}.InsertOpts()
	if opts.Queue != queueMediaServerSync || len(opts.UniqueOpts.ByState) != 5 {
		t.Fatalf("InsertOpts() = %#v", opts)
	}
}

type mediaServerSyncHandlerFunc func(context.Context, string) error

func (f mediaServerSyncHandlerFunc) HandleMediaServerSync(ctx context.Context, server string) error {
	return f(ctx, server)
}

func TestMediaServerSyncWorkerBoundaries(t *testing.T) {
	t.Parallel()
	worker := &mediaServerSyncWorker{handler: mediaServerSyncHandlerFunc(func(context.Context, string) error { return nil })}
	if timeout := worker.Timeout(nil); timeout != mediaServerSyncTimeout {
		t.Fatalf("Timeout() = %v, want %v", timeout, mediaServerSyncTimeout)
	}
	enabled, err := registerMediaServerSyncWorker(river.NewWorkers(), worker.handler)
	if err != nil || !enabled {
		t.Fatalf("registerMediaServerSyncWorker() = (%v, %v), want (true, nil)", enabled, err)
	}
	enabled, err = registerMediaServerSyncWorker(river.NewWorkers(), nil)
	if err != nil || enabled {
		t.Fatalf("registerMediaServerSyncWorker(nil) = (%v, %v), want (false, nil)", enabled, err)
	}

	validJob := func(server string) *river.Job[MediaServerSyncArgs] {
		return &river.Job[MediaServerSyncArgs]{Args: MediaServerSyncArgs{Key: mediaServerSyncKey, Server: server}}
	}
	businessWorker := &mediaServerSyncWorker{handler: mediaServerSyncHandlerFunc(func(context.Context, string) error {
		return errors.New("upstream detail that must not leak")
	})}
	cancelWorker := &mediaServerSyncWorker{handler: mediaServerSyncHandlerFunc(func(context.Context, string) error {
		return context.Canceled
	})}
	tests := []struct {
		name       string
		worker     *mediaServerSyncWorker
		job        *river.Job[MediaServerSyncArgs]
		wantErr    error
		wantCancel error
	}{
		{name: "nil context", worker: worker, wantErr: ErrMediaServerSyncExecution},
		{name: "missing handler", worker: &mediaServerSyncWorker{}, job: validJob("home"),
			wantErr: ErrMediaServerSyncHandlerUnavailable},
		{name: "nil job", worker: worker, wantCancel: ErrInvalidMediaServerSyncArgs},
		{name: "invalid args", worker: worker,
			job:        &river.Job[MediaServerSyncArgs]{Args: MediaServerSyncArgs{Key: "bad"}},
			wantCancel: ErrInvalidMediaServerSyncArgs},
		{name: "business failure is sanitized", worker: businessWorker, job: validJob("home"),
			wantErr: ErrMediaServerSyncExecution},
		{name: "cancellation is propagated", worker: cancelWorker, job: validJob("home"),
			wantErr: context.Canceled},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if tt.name == "nil context" {
				if err := tt.worker.Work(nil, tt.job); !errors.Is(err, tt.wantErr) {
					t.Fatalf("Work(nil) error = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if tt.wantCancel != nil {
				var cancelErr *river.JobCancelError
				if err := tt.worker.Work(t.Context(), tt.job); !errors.As(err, &cancelErr) || !errors.Is(err, tt.wantCancel) {
					t.Fatalf("Work() error = %v, want a cancelled job wrapping %v", err, tt.wantCancel)
				}
				return
			}
			if err := tt.worker.Work(t.Context(), tt.job); !errors.Is(err, tt.wantErr) {
				t.Fatalf("Work() error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestMediaServerSyncWorkerPassesServerSelector(t *testing.T) {
	t.Parallel()
	var got string
	worker := &mediaServerSyncWorker{handler: mediaServerSyncHandlerFunc(func(_ context.Context, server string) error {
		got = server
		return nil
	})}
	job := &river.Job[MediaServerSyncArgs]{Args: MediaServerSyncArgs{Key: mediaServerSyncKey, Server: " home "}}
	if err := worker.Work(t.Context(), job); err != nil {
		t.Fatalf("Work() error = %v", err)
	}
	if got != "home" {
		t.Fatalf("handler server = %q, want the normalized selector", got)
	}
}

func TestMediaServerSyncPeriodicJobMirrorsV2Schedule(t *testing.T) {
	t.Parallel()
	if DefaultMediaServerSyncInterval != 6*time.Hour {
		t.Fatalf("DefaultMediaServerSyncInterval = %v, want V2's 6h default", DefaultMediaServerSyncInterval)
	}
	if MediaServerSyncFirstRunDelay != 10*time.Minute {
		t.Fatalf("MediaServerSyncFirstRunDelay = %v, want V2's 10 minute first run", MediaServerSyncFirstRunDelay)
	}
	if job := mediaServerSyncPeriodicJob(MediaServerSyncSchedule{Server: "home", Interval: 2 * time.Hour}, 1); job == nil {
		t.Fatal("mediaServerSyncPeriodicJob() = nil")
	}
	sum := sha256.Sum256([]byte("home"))
	want := mediaServerSyncPeriodicPrefix + hex.EncodeToString(sum[:])[:12] + "-1"
	if got := mediaServerSyncPeriodicJobID("home", 1); got != want {
		t.Fatalf("mediaServerSyncPeriodicJobID(home, 1) = %q, want V2's digest id plus generation %q", got, want)
	}
	if mediaServerSyncPeriodicJobID(" home ", 1) != want {
		t.Fatal("the schedule identity must ignore surrounding whitespace")
	}
	if mediaServerSyncPeriodicJobID("home", 2) == want {
		t.Fatal("每次注册必须换用新 ID，否则 River 会拒绝同名服务器的重建")
	}
}

func TestManagerSetMediaServerSyncSchedulesReplacesServerSchedules(t *testing.T) {
	t.Parallel()
	bundle := newFakePeriodicJobBundle(0)
	manager := &Manager{mediaServerSyncEnabled: true, mediaServerPeriodicJobs: bundle}

	if err := manager.SetMediaServerSyncSchedules([]MediaServerSyncSchedule{
		{Server: "home", Interval: 2 * time.Hour},
		{Server: " remote ", Interval: 6 * time.Hour},
		{Server: "remote", Interval: 30 * time.Minute},
	}); err != nil {
		t.Fatalf("SetMediaServerSyncSchedules() error = %v", err)
	}
	active, removed := bundle.snapshot()
	if len(active) != 3 || active[1] == nil || active[2] == nil {
		t.Fatalf("active jobs = %v, want the seeded job plus handles 1 and 2", active)
	}
	if len(removed) != 0 {
		t.Fatalf("removed handles = %v, want none on the first registration", removed)
	}
	if len(manager.mediaServerPeriodicHandles) != 2 {
		t.Fatalf("registered handles = %v, want two", manager.mediaServerPeriodicHandles)
	}

	if err := manager.SetMediaServerSyncSchedules([]MediaServerSyncSchedule{
		{Server: "home", Interval: time.Hour},
	}); err != nil {
		t.Fatalf("SetMediaServerSyncSchedules() error = %v", err)
	}
	active, removed = bundle.snapshot()
	if len(active) != 2 || active[3] == nil {
		t.Fatalf("active jobs = %v, want the seeded job plus handle 3", active)
	}
	if len(removed) != 2 || removed[0] != 1 || removed[1] != 2 {
		t.Fatalf("removed handles = %v, want [1 2]", removed)
	}
}

func TestManagerSetMediaServerSyncSchedulesKeepsScheduleOnFailure(t *testing.T) {
	t.Parallel()
	addErr := errFakePeriodicAdd
	bundle := newFakePeriodicJobBundle(0)
	manager := &Manager{mediaServerSyncEnabled: true, mediaServerPeriodicJobs: bundle}
	if err := manager.SetMediaServerSyncSchedules([]MediaServerSyncSchedule{
		{Server: "home", Interval: time.Hour},
	}); err != nil {
		t.Fatalf("SetMediaServerSyncSchedules() error = %v", err)
	}

	// 第一次注册已用掉 1 次 add，第二次注册的「other」是第 3 次 add。
	bundle.failAddAt = 3
	err := manager.SetMediaServerSyncSchedules([]MediaServerSyncSchedule{
		{Server: "home", Interval: 2 * time.Hour},
		{Server: "other", Interval: 2 * time.Hour},
	})
	if !errors.Is(err, addErr) {
		t.Fatalf("SetMediaServerSyncSchedules() error = %v, want %v", err, addErr)
	}
	if len(manager.mediaServerPeriodicHandles) != 1 {
		t.Fatalf("registered handles = %v, want the rolled back home schedule", manager.mediaServerPeriodicHandles)
	}
	if len(manager.mediaServerPeriodicSchedules) != 1 ||
		manager.mediaServerPeriodicSchedules[0].Server != "home" ||
		manager.mediaServerPeriodicSchedules[0].Interval != time.Hour {
		t.Fatalf("restored schedules = %v, want the previous home schedule", manager.mediaServerPeriodicSchedules)
	}
	active, _ := bundle.snapshot()
	if len(active) != 2 {
		t.Fatalf("active jobs = %v, want the seeded rss job plus the rolled back schedule", active)
	}
}

// TestManagerSetMediaServerSyncSchedulesReplacesSameServer 锁定「同名服务器改间隔」的
// 重建路径：周期任务 ID 由服务器名决定，必须先注销旧任务再注册，否则 River 会以
// 「同一 ID 已注册」失败（保存媒体服务器设置时的实际故障）。
func TestManagerSetMediaServerSyncSchedulesReplacesSameServer(t *testing.T) {
	t.Parallel()
	bundle := newFakePeriodicJobBundle(0)
	manager := &Manager{mediaServerSyncEnabled: true, mediaServerPeriodicJobs: bundle}
	if err := manager.SetMediaServerSyncSchedules([]MediaServerSyncSchedule{
		{Server: "home", Interval: time.Hour},
	}); err != nil {
		t.Fatalf("SetMediaServerSyncSchedules() error = %v", err)
	}

	if err := manager.SetMediaServerSyncSchedules([]MediaServerSyncSchedule{
		{Server: "home", Interval: 2 * time.Hour},
	}); err != nil {
		t.Fatalf("SetMediaServerSyncSchedules() error = %v", err)
	}
	if len(manager.mediaServerPeriodicHandles) != 1 {
		t.Fatalf("registered handles = %v, want one", manager.mediaServerPeriodicHandles)
	}
	if manager.mediaServerPeriodicSchedules[0].Interval != 2*time.Hour {
		t.Fatalf("schedules = %v, want the new interval", manager.mediaServerPeriodicSchedules)
	}
	active, removed := bundle.snapshot()
	if len(active) != 2 {
		t.Fatalf("active jobs = %v, want the seeded rss job plus the replacement", active)
	}
	if len(removed) != 1 || removed[0] != 1 {
		t.Fatalf("removed handles = %v, want the previous home handle", removed)
	}
}

func TestManagerSetMediaServerSyncSchedulesRejectsInvalidSchedules(t *testing.T) {
	t.Parallel()
	bundle := newFakePeriodicJobBundle(0)
	manager := &Manager{mediaServerSyncEnabled: true, mediaServerPeriodicJobs: bundle}
	for name, schedules := range map[string][]MediaServerSyncSchedule{
		"blank server":           {{Server: "  ", Interval: time.Hour}},
		"non positive interval":  {{Server: "home", Interval: 0}},
		"negative interval":      {{Server: "home", Interval: -time.Hour}},
		"control character name": {{Server: "ho\nme", Interval: time.Hour}},
	} {
		if err := manager.SetMediaServerSyncSchedules(schedules); !errors.Is(err, ErrInvalidMediaServerSyncArgs) {
			t.Errorf("%s: error = %v, want %v", name, err, ErrInvalidMediaServerSyncArgs)
		}
	}
	active, removed := bundle.snapshot()
	if len(active) != 1 || len(removed) != 0 {
		t.Fatalf("invalid schedules changed the active jobs: %v / %v", active, removed)
	}
}

func TestManagerSetMediaServerSyncSchedulesUnavailable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		manager *Manager
	}{
		{name: "nil manager"},
		{name: "disabled manager", manager: &Manager{}},
		{name: "missing bundle", manager: &Manager{mediaServerSyncEnabled: true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if err := tt.manager.SetMediaServerSyncSchedules(nil); !errors.Is(err, ErrMediaServerSyncHandlerUnavailable) {
				t.Fatalf("SetMediaServerSyncSchedules() error = %v, want %v", err, ErrMediaServerSyncHandlerUnavailable)
			}
		})
	}
}

func TestManagerEnqueueMediaServerSyncBoundaries(t *testing.T) {
	t.Parallel()
	var manager *Manager
	if err := manager.EnqueueMediaServerSync(t.Context(), ""); !errors.Is(err, ErrMediaServerSyncHandlerUnavailable) {
		t.Fatalf("EnqueueMediaServerSync() error = %v, want %v", err, ErrMediaServerSyncHandlerUnavailable)
	}
	if err := (&Manager{mediaServerSyncEnabled: true}).EnqueueMediaServerSync(t.Context(), "home"); !errors.Is(err, ErrMediaServerSyncHandlerUnavailable) {
		t.Fatalf("EnqueueMediaServerSync() error = %v, want %v", err, ErrMediaServerSyncHandlerUnavailable)
	}
	if err := (&Manager{mediaServerSyncEnabled: true}).EnqueueMediaServerSyncFirstRun(t.Context(), "home"); !errors.Is(err, ErrMediaServerSyncHandlerUnavailable) {
		t.Fatalf("EnqueueMediaServerSyncFirstRun() error = %v, want %v", err, ErrMediaServerSyncHandlerUnavailable)
	}
	if err := (&Manager{}).EnqueueMediaServerSync(nil, "home"); !errors.Is(err, ErrMediaServerSyncExecution) {
		t.Fatalf("EnqueueMediaServerSync(nil) error = %v, want %v", err, ErrMediaServerSyncExecution)
	}
	if err := (&Manager{}).EnqueueMediaServerSync(t.Context(), "ho\nme"); !errors.Is(err, ErrInvalidMediaServerSyncArgs) {
		t.Fatalf("EnqueueMediaServerSync() error = %v, want %v", err, ErrInvalidMediaServerSyncArgs)
	}
}

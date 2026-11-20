package app

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/moviepilot-go/moviepilot-go/internal/jobs"
	"github.com/moviepilot-go/moviepilot-go/internal/mediaconfig"
)

func TestMediaServerRuntimeSyncSchedulesFollowsV2Rules(t *testing.T) {
	t.Parallel()
	servers := []mediaconfig.Server{
		mediaServerScheduleTestServer(0, "home", 0, true),
		mediaServerScheduleTestServer(1, "remote", 2, true),
		mediaServerScheduleTestServer(2, "disabled", 5, false),
	}
	runtime := newMediaServerRuntimeCompletionPlanTest(t, servers)

	schedules := runtime.SyncSchedules(jobs.DefaultMediaServerSyncInterval)
	want := []struct {
		server   string
		interval time.Duration
	}{
		{server: "home", interval: 6 * time.Hour},
		{server: "remote", interval: 2 * time.Hour},
	}
	if len(schedules) != len(want) {
		t.Fatalf("SyncSchedules() = %#v, want %d entries", schedules, len(want))
	}
	for index, schedule := range schedules {
		if schedule.Server != want[index].server || schedule.Interval != want[index].interval {
			t.Fatalf("schedule %d = %#v, want %#v", index, schedule, want[index])
		}
	}

	// A non-positive default skips servers that have no own interval, which is
	// V2's "no effective interval" branch.
	withoutDefault := runtime.SyncSchedules(0)
	if len(withoutDefault) != 1 || withoutDefault[0].Server != "remote" {
		t.Fatalf("SyncSchedules(0) = %#v, want only the server with its own interval", withoutDefault)
	}
	if len((&mediaServerRuntime{}).SyncSchedules(time.Hour)) != 0 {
		t.Fatal("an empty runtime must not return schedules")
	}
}

func mediaServerScheduleTestServer(index int, name string, syncInterval int, enabled bool) mediaconfig.Server {
	server := mediaServerRuntimeTestServer(nil)
	server.ID = fmt.Sprintf("3a46de31-c7bc-42d3-b65f-5691e9478c%02d", index)
	server.Name = name
	server.Enabled = enabled
	server.SyncInterval = syncInterval
	server.Config.Host = fmt.Sprintf("https://%s.emby.example.test", name)
	return server
}

type mediaServerSyncSchedulerFake struct {
	schedules   [][]jobs.MediaServerSyncSchedule
	firstRuns   []string
	setErr      error
	firstRunErr error
}

func (f *mediaServerSyncSchedulerFake) SetMediaServerSyncSchedules(schedules []jobs.MediaServerSyncSchedule) error {
	f.schedules = append(f.schedules, append([]jobs.MediaServerSyncSchedule(nil), schedules...))
	return f.setErr
}

func (f *mediaServerSyncSchedulerFake) EnqueueMediaServerSyncFirstRun(_ context.Context, server string) error {
	f.firstRuns = append(f.firstRuns, server)
	return f.firstRunErr
}

func TestApplyMediaServerSyncSchedulesPushesSnapshot(t *testing.T) {
	t.Parallel()
	runtime := newMediaServerRuntimeCompletionPlanTest(t, []mediaconfig.Server{
		mediaServerScheduleTestServer(0, "home", 0, true),
		mediaServerScheduleTestServer(1, "remote", 2, true),
	})
	scheduler := &mediaServerSyncSchedulerFake{}
	if err := applyMediaServerSyncSchedules(t.Context(), runtime, scheduler); err != nil {
		t.Fatalf("applyMediaServerSyncSchedules() error = %v", err)
	}
	if len(scheduler.schedules) != 1 || len(scheduler.schedules[0]) != 2 {
		t.Fatalf("pushed schedules = %#v", scheduler.schedules)
	}
	if scheduler.schedules[0][0].Server != "home" || scheduler.schedules[0][0].Interval != 6*time.Hour ||
		scheduler.schedules[0][1].Server != "remote" || scheduler.schedules[0][1].Interval != 2*time.Hour {
		t.Fatalf("pushed schedules = %#v", scheduler.schedules[0])
	}
	if len(scheduler.firstRuns) != 2 || scheduler.firstRuns[0] != "home" || scheduler.firstRuns[1] != "remote" {
		t.Fatalf("first runs = %#v, want one delayed run per enabled server", scheduler.firstRuns)
	}
}

func TestApplyMediaServerSyncSchedulesBoundaries(t *testing.T) {
	t.Parallel()
	runtime := newMediaServerRuntimeCompletionPlanTest(t, nil)

	if err := applyMediaServerSyncSchedules(t.Context(), nil, &mediaServerSyncSchedulerFake{}); !errors.Is(err, jobs.ErrMediaServerSyncHandlerUnavailable) {
		t.Fatalf("nil runtime error = %v, want %v", err, jobs.ErrMediaServerSyncHandlerUnavailable)
	}
	if err := applyMediaServerSyncSchedules(t.Context(), runtime, nil); !errors.Is(err, jobs.ErrMediaServerSyncHandlerUnavailable) {
		t.Fatalf("nil scheduler error = %v, want %v", err, jobs.ErrMediaServerSyncHandlerUnavailable)
	}
	if err := applyMediaServerSyncSchedules(nil, runtime, &mediaServerSyncSchedulerFake{}); !errors.Is(err, jobs.ErrMediaServerSyncExecution) {
		t.Fatalf("nil context error = %v, want %v", err, jobs.ErrMediaServerSyncExecution)
	}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if err := applyMediaServerSyncSchedules(cancelled, runtime, &mediaServerSyncSchedulerFake{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled context error = %v, want context.Canceled", err)
	}

	setErr := errors.New("set failed")
	if err := applyMediaServerSyncSchedules(t.Context(), runtime, &mediaServerSyncSchedulerFake{setErr: setErr}); !errors.Is(err, setErr) {
		t.Fatalf("set failure error = %v, want %v", err, setErr)
	}
	firstRunErr := errors.New("first run failed")
	withServer := newMediaServerRuntimeCompletionPlanTest(t, []mediaconfig.Server{
		mediaServerScheduleTestServer(0, "home", 0, true),
	})
	if err := applyMediaServerSyncSchedules(t.Context(), withServer,
		&mediaServerSyncSchedulerFake{firstRunErr: firstRunErr}); !errors.Is(err, firstRunErr) {
		t.Fatalf("first run failure error = %v, want %v", err, firstRunErr)
	}
}

func TestMediaServerSyncHandlerBinderDelegatesSelector(t *testing.T) {
	t.Parallel()
	var gotServer string
	service := mediaServerSyncServiceFunc(func(_ context.Context, server string) error {
		gotServer = server
		return nil
	})
	binder := &mediaServerSyncHandlerBinder{}
	if err := binder.Bind(service); err != nil {
		t.Fatalf("Bind() error = %v", err)
	}
	if err := binder.Bind(service); !errors.Is(err, errMediaServerSyncHandlerAlreadyBound) {
		t.Fatalf("second Bind() error = %v, want %v", err, errMediaServerSyncHandlerAlreadyBound)
	}
	if err := binder.HandleMediaServerSync(t.Context(), "home"); err != nil {
		t.Fatalf("HandleMediaServerSync() error = %v", err)
	}
	if gotServer != "home" {
		t.Fatalf("delegated selector = %q, want home", gotServer)
	}
	if err := (&mediaServerSyncHandlerBinder{}).HandleMediaServerSync(t.Context(), ""); !errors.Is(err, jobs.ErrMediaServerSyncHandlerUnavailable) {
		t.Fatalf("unbound handler error = %v, want %v", err, jobs.ErrMediaServerSyncHandlerUnavailable)
	}
	var nilBinder *mediaServerSyncHandlerBinder
	if err := nilBinder.HandleMediaServerSync(t.Context(), ""); !errors.Is(err, jobs.ErrMediaServerSyncHandlerUnavailable) {
		t.Fatalf("nil binder error = %v, want %v", err, jobs.ErrMediaServerSyncHandlerUnavailable)
	}
	if err := nilBinder.Bind(service); !errors.Is(err, jobs.ErrMediaServerSyncHandlerUnavailable) {
		t.Fatalf("nil binder Bind() error = %v, want %v", err, jobs.ErrMediaServerSyncHandlerUnavailable)
	}
}

type mediaServerSyncServiceFunc func(context.Context, string) error

func (f mediaServerSyncServiceFunc) SyncMediaServerItems(ctx context.Context, server string) error {
	return f(ctx, server)
}

func TestMediaServerRuntimeSyncMediaServerItemsSelectorTargetsOneServer(t *testing.T) {
	t.Parallel()
	host, _, _ := newMediaServerSyncFixture(t, false)
	store := &mediaServerItemStoreFake{}
	servers := []mediaconfig.Server{
		mediaServerScheduleTestServer(0, "home", 0, true),
		mediaServerScheduleTestServer(1, "remote", 0, true),
	}
	servers[0].Config.Host = host
	servers[1].Config.Host = "https://emby.example.test"
	runtime := newMediaServerRuntimeCompletionPlanTest(t, servers)
	runtime.now = func() time.Time { return time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC) }
	if err := runtime.SetItemCache(store, &mediaServerItemLookupFake{}, "admin"); err != nil {
		t.Fatalf("SetItemCache() error = %v", err)
	}

	if err := runtime.SyncMediaServerItems(t.Context(), "home"); err != nil {
		t.Fatalf("SyncMediaServerItems(home) error = %v", err)
	}
	if len(store.stale()) != 1 || store.stale()[0].server != "home" {
		t.Fatalf("stale calls = %#v, want only the selected server", store.stale())
	}
	if upserts := store.upserted(); len(upserts) != 3 {
		t.Fatalf("upserts = %d, want only the selected server's items", len(upserts))
	}
	// V2 still reconciles the cache against every enabled server name.
	excluded := store.excludedServers()
	if len(excluded) != 1 || len(excluded[0]) != 2 ||
		excluded[0][0] != "home" || excluded[0][1] != "remote" {
		t.Fatalf("excluded servers = %#v, want every enabled server name", excluded)
	}

	// An unknown selector performs no per-server work, like V2's
	// "未启用或不存在" branch, but keeps the excluded-server reconciliation.
	before := len(store.upserted())
	if err := runtime.SyncMediaServerItems(t.Context(), "absent"); err != nil {
		t.Fatalf("SyncMediaServerItems(absent) error = %v", err)
	}
	if len(store.upserted()) != before {
		t.Fatalf("upserts = %d, want no change for an unknown selector", len(store.upserted()))
	}
	if len(store.excludedServers()) != 2 {
		t.Fatalf("excluded calls = %#v, want one per run", store.excludedServers())
	}
	// A server ID selects the same server as its name.
	if err := runtime.SyncMediaServerItems(t.Context(), servers[0].ID); err != nil {
		t.Fatalf("SyncMediaServerItems(id) error = %v", err)
	}
	if len(store.stale()) != 2 || store.stale()[1].server != "home" {
		t.Fatalf("stale calls = %#v, want the id selector to resolve to home", store.stale())
	}
}

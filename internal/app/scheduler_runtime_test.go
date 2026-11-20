package app

import (
	"context"
	"testing"
	"time"

	"github.com/moviepilot-go/moviepilot-go/internal/httpapi"
)

type fakeCookieCloudSyncer struct {
	called int
}

func (f *fakeCookieCloudSyncer) SyncCookieCloud(_ context.Context) (string, error) {
	f.called++
	return "synced", nil
}

type fakeDirMonitorRunner struct {
	triggered int
}

func (f *fakeDirMonitorRunner) TriggerScan(_ context.Context) error {
	f.triggered++
	return nil
}

func (f *fakeDirMonitorRunner) ScheduleInfo(_ time.Time, _ *time.Location) (httpapi.ScheduleInfo, bool) {
	return httpapi.ScheduleInfo{
		ID:       "directory_monitor",
		Name:     "目录监控整理",
		Provider: "系统",
		Status:   "等待",
		NextRun:  "1分钟后",
	}, true
}

func TestSchedulerRuntimeListAndRun(t *testing.T) {
	t.Parallel()

	cc := &fakeCookieCloudSyncer{}
	dm := &fakeDirMonitorRunner{}
	runtime := newSchedulerRuntime(nil, cc, nil, dm, time.UTC)

	schedules := runtime.ListSchedules(context.Background())
	if len(schedules) != 1 || schedules[0].ID != "directory_monitor" {
		t.Fatalf("ListSchedules() = %+v", schedules)
	}

	ok, err := runtime.RunScheduler(context.Background(), "cookiecloud")
	if err != nil || !ok || cc.called != 1 {
		t.Fatalf("cookiecloud run ok=%v err=%v called=%d", ok, err, cc.called)
	}

	ok, err = runtime.RunScheduler(context.Background(), "directory_monitor")
	if err != nil || !ok || dm.triggered != 1 {
		t.Fatalf("directory_monitor run ok=%v err=%v triggered=%d", ok, err, dm.triggered)
	}

	ok, err = runtime.RunScheduler(context.Background(), "unknown_job_xyz")
	if err != nil || ok {
		t.Fatalf("unknown job ok=%v err=%v, want false, nil", ok, err)
	}
}

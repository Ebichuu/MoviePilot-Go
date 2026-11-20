//go:build integration

package jobs

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

type rssScheduleIntegrationBundle struct {
	delegate *river.PeriodicJobBundle

	mu      sync.Mutex
	active  map[rivertype.PeriodicJobHandle]*river.PeriodicJob
	removed []rivertype.PeriodicJobHandle
}

func newRSSScheduleIntegrationBundle(
	delegate *river.PeriodicJobBundle,
	initialHandle rivertype.PeriodicJobHandle,
	initialJob *river.PeriodicJob,
) *rssScheduleIntegrationBundle {
	return &rssScheduleIntegrationBundle{
		delegate: delegate,
		active: map[rivertype.PeriodicJobHandle]*river.PeriodicJob{
			initialHandle: initialJob,
		},
	}
}

func (b *rssScheduleIntegrationBundle) AddSafely(job *river.PeriodicJob) (rivertype.PeriodicJobHandle, error) {
	handle, err := b.delegate.AddSafely(job)
	if err != nil {
		return 0, err
	}

	b.mu.Lock()
	b.active[handle] = job
	b.mu.Unlock()
	return handle, nil
}

func (b *rssScheduleIntegrationBundle) Remove(handle rivertype.PeriodicJobHandle) {
	b.delegate.Remove(handle)

	b.mu.Lock()
	delete(b.active, handle)
	b.removed = append(b.removed, handle)
	b.mu.Unlock()
}

func (b *rssScheduleIntegrationBundle) snapshot() (
	map[rivertype.PeriodicJobHandle]*river.PeriodicJob,
	[]rivertype.PeriodicJobHandle,
) {
	b.mu.Lock()
	defer b.mu.Unlock()

	active := make(map[rivertype.PeriodicJobHandle]*river.PeriodicJob, len(b.active))
	for handle, job := range b.active {
		active[handle] = job
	}
	return active, append([]rivertype.PeriodicJobHandle(nil), b.removed...)
}

type rssScheduleIntegrationHandler struct{}

func (rssScheduleIntegrationHandler) HandleRSSSweep(
	context.Context,
	time.Time,
	RSSSweepState,
) (RSSSweepState, error) {
	return RSSSweepState{Initialized: true, SiteIDs: []int{}}, nil
}

func TestManagerRSSScheduleHotUpdateWithPostgresRiver(t *testing.T) {
	pool := newRSSRiverIntegrationPool(t)
	initialInterval := 30 * time.Minute
	manager, err := NewManagerWithConfig(
		pool,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		ManagerConfig{
			RSSSweepHandler:  rssScheduleIntegrationHandler{},
			RSSSweepInterval: initialInterval,
		},
	)
	if err != nil {
		t.Fatalf("创建带 RSS 周期任务的 River manager: %v", err)
	}
	if !manager.rssPeriodicRegistered || manager.rssPeriodicJobs == nil {
		t.Fatal("初始 RSS 周期任务未注册")
	}

	initialHandle := manager.rssPeriodicHandle
	realBundle, ok := manager.rssPeriodicJobs.(*river.PeriodicJobBundle)
	if !ok {
		t.Fatalf("初始周期任务 bundle 类型 = %T, want *river.PeriodicJobBundle", manager.rssPeriodicJobs)
	}
	if duplicateHandle, duplicateErr := realBundle.AddSafely(rssSweepPeriodicJob(time.Hour)); duplicateErr == nil {
		realBundle.Remove(duplicateHandle)
		t.Fatal("初始固定 ID 周期任务未占用 River 注册表")
	}

	trackingBundle := newRSSScheduleIntegrationBundle(
		realBundle,
		initialHandle,
		rssSweepPeriodicJob(initialInterval),
	)
	manager.rssPeriodicJobs = trackingBundle

	if err := manager.Start(t.Context()); err != nil {
		t.Fatalf("启动 River manager: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := manager.Stop(stopCtx); err != nil {
			t.Errorf("停止 River manager: %v", err)
		}
	})

	assertRSSScheduleIntegrationJobCount(t, pool, 0)

	if err := manager.SetRSSSweepInterval(45 * time.Minute); err != nil {
		t.Fatalf("首次更新 RSS 周期: %v", err)
	}
	firstRuntimeHandle := manager.rssPeriodicHandle
	if firstRuntimeHandle == initialHandle {
		t.Fatalf("首次更新后句柄仍为 %d", initialHandle)
	}
	assertRSSScheduleIntegrationState(t, trackingBundle, firstRuntimeHandle, []rivertype.PeriodicJobHandle{initialHandle})

	previousHandle := firstRuntimeHandle
	removed := []rivertype.PeriodicJobHandle{initialHandle}
	for _, interval := range []time.Duration{5 * time.Minute, 30 * time.Minute, 2 * time.Hour} {
		if err := manager.SetRSSSweepInterval(interval); err != nil {
			t.Fatalf("连续更新 RSS 周期为 %s: %v", interval, err)
		}
		currentHandle := manager.rssPeriodicHandle
		if currentHandle == previousHandle {
			t.Fatalf("更新 RSS 周期为 %s 后复用了旧句柄 %d", interval, currentHandle)
		}
		removed = append(removed, previousHandle)
		assertRSSScheduleIntegrationState(t, trackingBundle, currentHandle, removed)
		previousHandle = currentHandle
	}

	assertRSSScheduleIntegrationJobCount(t, pool, 0)
}

func assertRSSScheduleIntegrationState(
	t *testing.T,
	bundle *rssScheduleIntegrationBundle,
	wantHandle rivertype.PeriodicJobHandle,
	wantRemoved []rivertype.PeriodicJobHandle,
) {
	t.Helper()

	active, removed := bundle.snapshot()
	if len(active) != 1 || active[wantHandle] == nil {
		t.Fatalf("活跃 RSS 周期任务 = %v, want only handle %d", active, wantHandle)
	}
	if len(removed) != len(wantRemoved) {
		t.Fatalf("已移除 RSS 周期句柄 = %v, want %v", removed, wantRemoved)
	}
	for index := range wantRemoved {
		if removed[index] != wantRemoved[index] {
			t.Fatalf("已移除 RSS 周期句柄 = %v, want %v", removed, wantRemoved)
		}
	}
}

func assertRSSScheduleIntegrationJobCount(t *testing.T, pool *pgxpool.Pool, want int) {
	t.Helper()

	var count int
	if err := pool.QueryRow(
		t.Context(),
		`SELECT count(*) FROM river_job WHERE kind=$1`,
		(RSSSweepArgs{}).Kind(),
	).Scan(&count); err != nil {
		t.Fatalf("统计 PostgreSQL 中的 RSS 周期任务: %v", err)
	}
	if count != want {
		t.Fatalf("PostgreSQL 中的 RSS 周期任务数 = %d, want %d", count, want)
	}
}

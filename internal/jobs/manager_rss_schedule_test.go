package jobs

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

var errFakePeriodicAdd = errors.New("periodic job add failed")

type fakePeriodicJobBundle struct {
	mu         sync.Mutex
	nextHandle rivertype.PeriodicJobHandle
	addErr     error
	// failAddAt 让第 N 次（1 起算）注册失败，用于验证「部分成功后再回滚」。
	failAddAt int
	addCount  int
	active    map[rivertype.PeriodicJobHandle]*river.PeriodicJob
	removed   []rivertype.PeriodicJobHandle
}

func newFakePeriodicJobBundle(initialHandle rivertype.PeriodicJobHandle) *fakePeriodicJobBundle {
	return &fakePeriodicJobBundle{
		nextHandle: initialHandle + 1,
		active: map[rivertype.PeriodicJobHandle]*river.PeriodicJob{
			initialHandle: rssSweepPeriodicJob(rssSweepInterval),
		},
	}
}

func (f *fakePeriodicJobBundle) AddSafely(job *river.PeriodicJob) (rivertype.PeriodicJobHandle, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.addCount++
	if f.addErr != nil {
		return 0, f.addErr
	}
	if f.failAddAt > 0 && f.addCount == f.failAddAt {
		return 0, errFakePeriodicAdd
	}
	handle := f.nextHandle
	f.nextHandle++
	f.active[handle] = job
	return handle, nil
}

func (f *fakePeriodicJobBundle) Remove(handle rivertype.PeriodicJobHandle) {
	f.mu.Lock()
	defer f.mu.Unlock()

	delete(f.active, handle)
	f.removed = append(f.removed, handle)
}

func (f *fakePeriodicJobBundle) snapshot() (map[rivertype.PeriodicJobHandle]*river.PeriodicJob, []rivertype.PeriodicJobHandle) {
	f.mu.Lock()
	defer f.mu.Unlock()

	active := make(map[rivertype.PeriodicJobHandle]*river.PeriodicJob, len(f.active))
	for handle, job := range f.active {
		active[handle] = job
	}
	return active, append([]rivertype.PeriodicJobHandle(nil), f.removed...)
}

func newRSSScheduleTestManager(bundle periodicJobBundle, handle rivertype.PeriodicJobHandle) *Manager {
	return &Manager{
		rssPeriodicJobs:       bundle,
		rssPeriodicHandle:     handle,
		rssPeriodicRegistered: true,
		rssSweepEnabled:       true,
	}
}

func TestManagerSetRSSSweepIntervalRemovesHandleZero(t *testing.T) {
	t.Parallel()

	bundle := newFakePeriodicJobBundle(0)
	manager := newRSSScheduleTestManager(bundle, 0)
	if err := manager.SetRSSSweepInterval(45 * time.Minute); err != nil {
		t.Fatalf("SetRSSSweepInterval() error = %v", err)
	}

	active, removed := bundle.snapshot()
	if len(active) != 1 || active[1] == nil {
		t.Fatalf("active jobs = %v, want only handle 1", active)
	}
	if len(removed) != 1 || removed[0] != 0 {
		t.Fatalf("removed handles = %v, want [0]", removed)
	}
	if manager.rssPeriodicHandle != 1 || !manager.rssPeriodicRegistered {
		t.Fatalf("manager schedule = handle %d registered %t, want handle 1 registered", manager.rssPeriodicHandle, manager.rssPeriodicRegistered)
	}
}

func TestManagerSetRSSSweepIntervalAddFailureKeepsOldSchedule(t *testing.T) {
	t.Parallel()

	addErr := errors.New("add failed")
	bundle := newFakePeriodicJobBundle(0)
	bundle.addErr = addErr
	manager := newRSSScheduleTestManager(bundle, 0)

	err := manager.SetRSSSweepInterval(45 * time.Minute)
	if !errors.Is(err, addErr) {
		t.Fatalf("SetRSSSweepInterval() error = %v, want %v", err, addErr)
	}
	active, removed := bundle.snapshot()
	if len(active) != 1 || active[0] == nil {
		t.Fatalf("active jobs = %v, want old handle 0", active)
	}
	if len(removed) != 0 {
		t.Fatalf("removed handles = %v, want none", removed)
	}
	if manager.rssPeriodicHandle != 0 || !manager.rssPeriodicRegistered {
		t.Fatalf("manager schedule changed after add failure: handle %d registered %t", manager.rssPeriodicHandle, manager.rssPeriodicRegistered)
	}
}

func TestManagerSetRSSSweepIntervalRepeatedlyKeepsOneActiveSchedule(t *testing.T) {
	t.Parallel()

	bundle := newFakePeriodicJobBundle(0)
	manager := newRSSScheduleTestManager(bundle, 0)
	for _, interval := range []time.Duration{5 * time.Minute, 30 * time.Minute, 2 * time.Hour} {
		if err := manager.SetRSSSweepInterval(interval); err != nil {
			t.Fatalf("SetRSSSweepInterval(%s) error = %v", interval, err)
		}
	}

	active, removed := bundle.snapshot()
	if len(active) != 1 || active[3] == nil {
		t.Fatalf("active jobs = %v, want only handle 3", active)
	}
	wantRemoved := []rivertype.PeriodicJobHandle{0, 1, 2}
	if len(removed) != len(wantRemoved) {
		t.Fatalf("removed handles = %v, want %v", removed, wantRemoved)
	}
	for i := range wantRemoved {
		if removed[i] != wantRemoved[i] {
			t.Fatalf("removed handles = %v, want %v", removed, wantRemoved)
		}
	}
}

func TestManagerSetRSSSweepIntervalConcurrentUpdatesKeepOneActiveSchedule(t *testing.T) {
	bundle := newFakePeriodicJobBundle(0)
	manager := newRSSScheduleTestManager(bundle, 0)

	const updateCount = 32
	var wg sync.WaitGroup
	errs := make(chan error, updateCount)
	for i := range updateCount {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- manager.SetRSSSweepInterval(time.Duration(i+5) * time.Minute)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent SetRSSSweepInterval() error = %v", err)
		}
	}

	active, removed := bundle.snapshot()
	if len(active) != 1 || active[updateCount] == nil {
		t.Fatalf("active jobs = %v, want only handle %d", active, updateCount)
	}
	if len(removed) != updateCount {
		t.Fatalf("removed handle count = %d, want %d", len(removed), updateCount)
	}
}

func TestManagerSetRSSSweepIntervalUnavailable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		manager *Manager
	}{
		{name: "nil manager"},
		{name: "disabled manager", manager: &Manager{}},
		{name: "missing bundle", manager: &Manager{rssSweepEnabled: true, rssPeriodicRegistered: true}},
		{name: "not registered", manager: &Manager{rssSweepEnabled: true, rssPeriodicJobs: newFakePeriodicJobBundle(0)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if err := tt.manager.SetRSSSweepInterval(time.Hour); !errors.Is(err, ErrRSSSweepHandlerUnavailable) {
				t.Fatalf("SetRSSSweepInterval() error = %v, want ErrRSSSweepHandlerUnavailable", err)
			}
		})
	}
}

func TestManagerEnsureRSSSweepScheduleRegistersMissingSchedule(t *testing.T) {
	t.Parallel()

	bundle := &fakePeriodicJobBundle{
		nextHandle: 1,
		active:     map[rivertype.PeriodicJobHandle]*river.PeriodicJob{},
	}
	manager := &Manager{rssPeriodicJobs: bundle, rssSweepEnabled: true}

	if err := manager.EnsureRSSSweepSchedule(5 * time.Minute); err != nil {
		t.Fatalf("EnsureRSSSweepSchedule() error = %v", err)
	}
	if !manager.rssPeriodicRegistered {
		t.Fatal("EnsureRSSSweepSchedule() 未标记周期任务已注册")
	}
	if active, _ := bundle.snapshot(); len(active) != 1 {
		t.Fatalf("active periodic jobs = %d, want 1", len(active))
	}

	// 重复调用必须只保留一个周期任务，否则 RSS 扫描会出现双倍节拍。
	if err := manager.EnsureRSSSweepSchedule(10 * time.Minute); err != nil {
		t.Fatalf("EnsureRSSSweepSchedule() 第二次 error = %v", err)
	}
	active, removed := bundle.snapshot()
	if len(active) != 1 {
		t.Fatalf("active periodic jobs after re-register = %d, want 1", len(active))
	}
	if len(removed) != 1 {
		t.Fatalf("removed handles = %v, want 1", removed)
	}
}

func TestManagerEnsureRSSSweepScheduleRejectsDisabledManager(t *testing.T) {
	t.Parallel()

	if err := (&Manager{}).EnsureRSSSweepSchedule(time.Minute); !errors.Is(err, ErrRSSSweepHandlerUnavailable) {
		t.Fatalf("EnsureRSSSweepSchedule() error = %v, want ErrRSSSweepHandlerUnavailable", err)
	}
	bundle := newFakePeriodicJobBundle(0)
	manager := &Manager{rssSweepEnabled: true, rssPeriodicJobs: bundle, rssPeriodicRegistered: true}
	bundle.addErr = errFakePeriodicAdd
	if err := manager.EnsureRSSSweepSchedule(time.Minute); !errors.Is(err, errFakePeriodicAdd) {
		t.Fatalf("EnsureRSSSweepSchedule() error = %v, want errFakePeriodicAdd", err)
	}
	if active, _ := bundle.snapshot(); len(active) != 1 {
		t.Fatalf("注册失败后 active periodic jobs = %d, want 1（保留原句柄）", len(active))
	}
}

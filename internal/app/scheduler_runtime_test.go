package app

import (
	"errors"
	"testing"

	"github.com/moviepilot-go/moviepilot-go/internal/jobs"
)

func TestNewSchedulerRuntimeRequiresManager(t *testing.T) {
	t.Parallel()

	if got := newSchedulerRuntime(nil); got != nil {
		t.Fatalf("nil manager = %#v", got)
	}
	manager := &jobs.Manager{}
	runtime := newSchedulerRuntime(manager)
	if runtime == nil || runtime.jobs != manager {
		t.Fatalf("runtime = %#v", runtime)
	}
}

func TestSchedulerRuntimeRunScheduler(t *testing.T) {
	t.Parallel()

	var nilRuntime *schedulerRuntime
	if ok, err := nilRuntime.RunScheduler(t.Context(), "probe"); ok || err != nil {
		t.Fatalf("nil runtime = (%t, %v)", ok, err)
	}
	if ok, err := (&schedulerRuntime{}).RunScheduler(t.Context(), "probe"); ok || err != nil {
		t.Fatalf("缺 jobs = (%t, %v)", ok, err)
	}

	runtime := newSchedulerRuntime(&jobs.Manager{})
	// 未映射的 jobid 一律 (false, nil)：HTTP 层据此回「后台服务不存在」，
	// 不能假装触发成功。
	for _, jobID := range []string{"", "   ", "unknown", "PROBE", "organization-sweep"} {
		if ok, err := runtime.RunScheduler(t.Context(), jobID); ok || err != nil {
			t.Fatalf("jobid=%q = (%t, %v)", jobID, ok, err)
		}
	}

	// 已映射的 jobid：下游没接好时如实返回错误，而不是报「不存在」。
	if ok, err := runtime.RunScheduler(t.Context(), " organize "); ok ||
		!errors.Is(err, jobs.ErrOrganizationSweepHandlerUnavailable) {
		t.Fatalf("organize = (%t, %v)", ok, err)
	}
}

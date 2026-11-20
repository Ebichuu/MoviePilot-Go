package app

import (
	"context"
	"strings"

	"github.com/moviepilot-go/moviepilot-go/internal/jobs"
)

// schedulerRuntime 把 V2 `GET /system/runscheduler?jobid=...` 的 jobid 映射到本项目
// 已有的一次性/周期任务。未映射的 jobid 返回 false，由 HTTP 层回「后台服务不存在」，
// 不假装触发成功。
type schedulerRuntime struct {
	jobs *jobs.Manager
}

func newSchedulerRuntime(manager *jobs.Manager) *schedulerRuntime {
	if manager == nil {
		return nil
	}
	return &schedulerRuntime{jobs: manager}
}

func (s *schedulerRuntime) RunScheduler(ctx context.Context, jobID string) (bool, error) {
	if s == nil || s.jobs == nil {
		return false, nil
	}
	switch strings.TrimSpace(jobID) {
	case "probe", "site_probe", "cookiecloud":
		if err := s.jobs.EnqueueProbe(ctx); err != nil {
			return false, err
		}
		return true, nil
	case "organization", "organize", "transfer_sweep":
		if err := s.jobs.EnqueueOrganizationSweep(ctx); err != nil {
			return false, err
		}
		return true, nil
	}
	return false, nil
}

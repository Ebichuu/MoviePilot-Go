package app

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/moviepilot-go/moviepilot-go/internal/httpapi"
	"github.com/moviepilot-go/moviepilot-go/internal/jobs"
)

type schedulerMediaServerLister interface {
	EnabledServerNames() []string
}

type directoryMonitorRunner interface {
	TriggerScan(context.Context) error
	ScheduleInfo(now time.Time, loc *time.Location) (httpapi.ScheduleInfo, bool)
}

// schedulerRuntime 把 River 周期任务投影到 V2 `/dashboard/schedule`，
// 并把 V2 `GET /system/runscheduler?jobid=...` 映射到对应的一次性/周期任务。
type schedulerRuntime struct {
	jobs         *jobs.Manager
	cookieCloud  httpapi.CookieCloudSyncer
	mediaServers schedulerMediaServerLister
	dirMonitor   directoryMonitorRunner
	location     *time.Location
}

func newSchedulerRuntime(
	manager *jobs.Manager,
	cookieCloud httpapi.CookieCloudSyncer,
	mediaServers schedulerMediaServerLister,
	dirMonitor directoryMonitorRunner,
	loc *time.Location,
) *schedulerRuntime {
	if manager == nil && cookieCloud == nil && dirMonitor == nil {
		return nil
	}
	if loc == nil {
		loc = time.Local
	}
	return &schedulerRuntime{
		jobs:         manager,
		cookieCloud:  cookieCloud,
		mediaServers: mediaServers,
		dirMonitor:   dirMonitor,
		location:     loc,
	}
}

// ListSchedules 投影 River 周期任务和目录监控为 V2 `ScheduleInfo` 列表。
func (s *schedulerRuntime) ListSchedules(_ context.Context) []httpapi.ScheduleInfo {
	if s == nil {
		return []httpapi.ScheduleInfo{}
	}
	now := time.Now()
	var items []httpapi.ScheduleInfo
	if s.jobs != nil {
		scheduled := s.jobs.ScheduledJobs(now)
		items = make([]httpapi.ScheduleInfo, 0, len(scheduled)+1)
		for _, job := range scheduled {
			items = append(items, httpapi.ScheduleInfo{
				ID:       job.ID,
				Name:     job.Name,
				Provider: job.Provider,
				Status:   job.Status,
				NextRun:  formatScheduleNextRun(now, job.NextRun, s.location),
			})
		}
	}
	if s.dirMonitor != nil {
		if info, ok := s.dirMonitor.ScheduleInfo(now, s.location); ok {
			items = append(items, info)
		}
	}
	if items == nil {
		return []httpapi.ScheduleInfo{}
	}
	return items
}

// RunScheduler 将 V2 常见 jobid 映射到真实后台任务；未映射的返回 false。
func (s *schedulerRuntime) RunScheduler(ctx context.Context, jobID string) (bool, error) {
	if s == nil {
		return false, nil
	}
	id := strings.TrimSpace(jobID)
	if id == "" {
		return false, nil
	}

	if strings.HasPrefix(id, "mediaserver_sync:") {
		server := strings.TrimSpace(strings.TrimPrefix(id, "mediaserver_sync:"))
		if server == "" || s.jobs == nil {
			return false, nil
		}
		return true, s.jobs.EnqueueMediaServerSync(ctx, server)
	}

	switch strings.ToLower(id) {
	case "probe", "site_probe":
		if s.jobs == nil {
			return false, nil
		}
		return true, s.jobs.EnqueueProbe(ctx)

	case "cookiecloud":
		if s.cookieCloud != nil {
			_, err := s.cookieCloud.SyncCookieCloud(ctx)
			return true, err
		}
		if s.jobs != nil {
			return true, s.jobs.EnqueueProbe(ctx)
		}
		return false, nil

	case "organization", "organize", "transfer", "transfer_sweep":
		if s.dirMonitor != nil {
			_ = s.dirMonitor.TriggerScan(ctx)
		}
		if s.jobs == nil {
			return s.dirMonitor != nil, nil
		}
		return true, s.jobs.EnqueueOrganizationSweep(ctx)

	case "directory_monitor", "monitor":
		if s.dirMonitor == nil {
			return false, nil
		}
		return true, s.dirMonitor.TriggerScan(ctx)

	case "subscribe_refresh", "rss", "rss_sweep":
		if s.jobs == nil {
			return false, nil
		}
		return true, s.jobs.EnqueueRSSSweep(ctx, time.Now().UTC())

	case "subscribe_search", "subscribe":
		if s.jobs == nil {
			return false, nil
		}
		return true, s.jobs.EnqueueSubscriptionSearchSweep(ctx)

	case "site_refresh", "site_userdata", "userdata_refresh", "sitedata_refresh":
		if s.jobs == nil {
			return false, nil
		}
		return true, s.jobs.EnqueueSiteUserDataRefresh(ctx)

	case "site_signin", "signin":
		if s.jobs == nil {
			return false, nil
		}
		return true, s.jobs.EnqueueSiteSigninSweep(ctx)

	case "mediaserver_sync":
		if s.jobs == nil || s.mediaServers == nil {
			return false, nil
		}
		servers := s.mediaServers.EnabledServerNames()
		if len(servers) == 0 {
			return false, nil
		}
		for _, server := range servers {
			if err := s.jobs.EnqueueMediaServerSync(ctx, server); err != nil {
				return true, err
			}
		}
		return true, nil

	case "download_reconcile", "reconcile":
		if s.jobs == nil {
			return false, nil
		}
		return true, s.jobs.EnqueueDownloadReconcile(ctx)

	case "media_index_sweep", "media_index":
		if s.jobs == nil {
			return false, nil
		}
		return true, s.jobs.EnqueueMediaIndexSweep(ctx)

	case "scrape", "scrape_sweep":
		if s.jobs == nil {
			return false, nil
		}
		return true, s.jobs.EnqueueScrapeSweep(ctx)

	case "notification_sweep":
		if s.jobs == nil {
			return false, nil
		}
		return true, s.jobs.EnqueueNotificationSweep(ctx)

	case "backup", "backup_sweep":
		if s.jobs == nil {
			return false, nil
		}
		return true, s.jobs.EnqueueBackupSweep(ctx, "", 7)

	case "cleanup", "cleanup_sweep", "clear_cache":
		if s.jobs == nil {
			return false, nil
		}
		return true, s.jobs.EnqueueCleanupSweep(ctx)
	}

	return false, nil
}

func formatScheduleNextRun(now, next time.Time, loc *time.Location) string {
	if next.IsZero() {
		return ""
	}
	diff := next.Sub(now)
	if diff <= 0 {
		return "即将执行"
	}
	if diff < time.Minute {
		return fmt.Sprintf("%d秒后", int(diff.Seconds()))
	}
	if diff < time.Hour {
		return fmt.Sprintf("%d分钟后", int(diff.Minutes()))
	}
	if loc == nil {
		loc = time.Local
	}
	return next.In(loc).Format("2006-01-02 15:04:05")
}

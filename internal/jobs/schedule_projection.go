package jobs

import (
	"sort"
	"time"
)

// ScheduledJob 是 River 周期任务的只读投影，供仪表盘 /dashboard/schedule 展示。
type ScheduledJob struct {
	ID       string
	Name     string
	Provider string
	Status   string
	Interval time.Duration
	NextRun  time.Time
}

// ScheduledJobs 返回当前已注册的 River 周期任务清单及下次预计触发时间。
func (m *Manager) ScheduledJobs(now time.Time) []ScheduledJob {
	if m == nil {
		return []ScheduledJob{}
	}
	if now.IsZero() {
		now = time.Now()
	}
	base := m.startedAt
	if base.IsZero() {
		base = now
	}

	nextRun := func(interval time.Duration) time.Time {
		if interval <= 0 {
			return now
		}
		if now.Before(base) {
			return base.Add(interval)
		}
		elapsed := now.Sub(base)
		steps := elapsed/interval + 1
		return base.Add(steps * interval)
	}

	result := make([]ScheduledJob, 0, 12)
	add := func(id, name, provider string, interval time.Duration) {
		result = append(result, ScheduledJob{
			ID:       id,
			Name:     name,
			Provider: provider,
			Status:   "等待",
			Interval: interval,
			NextRun:  nextRun(interval),
		})
	}

	if m.rssSweepEnabled {
		m.rssPeriodicMu.Lock()
		interval := m.rssPeriodicInterval
		m.rssPeriodicMu.Unlock()
		if interval <= 0 {
			interval = rssSweepInterval
		}
		add("subscribe_refresh", "订阅RSS刷新", "系统", interval)
	}
	if m.subscriptionSearchEnabled {
		add("subscribe_search", "订阅搜索", "系统", subscriptionSearchInterval)
	}
	if m.downloadReconcileEnabled {
		add("download_reconcile", "下载状态对账", "系统", downloadReconcileInterval)
	}
	if m.organizationSweepEnabled {
		add("transfer_sweep", "下载整理恢复", "系统", organizationSweepInterval)
	}
	if m.mediaIndexSweepEnabled {
		add("media_index_sweep", "媒体入库确认", "系统", mediaIndexSweepInterval)
	}
	if m.scrapeSweepEnabled {
		add("scrape_sweep", "元数据刮削恢复", "系统", scrapeSweepInterval)
	}
	if m.notificationSweepEnabled {
		add("notification_sweep", "通知漏投恢复", "系统", notificationSweepInterval)
	}
	if m.siteUserDataRefreshEnabled {
		add("site_refresh", "站点数据刷新", "系统", siteUserDataRefreshInterval)
	}
	if m.siteSigninSweepEnabled {
		m.siteSigninPeriodicMu.Lock()
		interval := m.siteSigninPeriodicInterval
		m.siteSigninPeriodicMu.Unlock()
		if interval <= 0 {
			interval = siteSigninSweepInterval
		}
		add("site_signin", "站点自动签到", "内置插件", interval)
	}
	if m.backupSweepEnabled {
		m.backupPeriodicMu.Lock()
		interval := m.backupPeriodicInterval
		m.backupPeriodicMu.Unlock()
		if interval <= 0 {
			interval = backupSweepInterval
		}
		add("backup_sweep", "系统自动备份", "内置插件", interval)
	}
	if m.cleanupSweepEnabled {
		add("cleanup_sweep", "过期数据清理", "系统", cleanupSweepInterval)
	}

	if m.mediaServerSyncEnabled {
		m.mediaServerPeriodicMu.Lock()
		schedules := append([]MediaServerSyncSchedule(nil), m.mediaServerPeriodicSchedules...)
		m.mediaServerPeriodicMu.Unlock()
		sort.Slice(schedules, func(i, j int) bool {
			return schedules[i].Server < schedules[j].Server
		})
		for _, sched := range schedules {
			interval := sched.Interval
			if interval <= 0 {
				interval = DefaultMediaServerSyncInterval
			}
			add("mediaserver_sync:"+sched.Server, "媒体库同步 - "+sched.Server, "系统", interval)
		}
	}

	return result
}

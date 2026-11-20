package jobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/riverqueue/river"
)

const (
	// siteSigninSweepKind 对应 V2 `scheduler.py` 的站点签到任务。
	siteSigninSweepKind = "site_signin_sweep"
	// queueSiteSigninSweep 单独一条队列，避免签到挤占搜索队列。
	queueSiteSigninSweep = "site_signin_sweep"
	// siteSigninSweepInterval：每天一次。
	siteSigninSweepInterval = 24 * time.Hour
	// siteSigninSweepPeriodicID：River 周期任务 ID。
	siteSigninSweepPeriodicID = "site_signin_sweep_periodic"
)

// SiteSigninResult 是单个站点的签到结果。
type SiteSigninResult struct {
	SiteID int
	Site   string
	// Status 只有 signed / skipped / failed 三种。
	Status string
	Reason string
}

// SiteSigninReport 是一轮签到的汇总。
type SiteSigninReport struct {
	Signed  int
	Skipped int
	Failed  int
	Items   []SiteSigninResult
}

// SiteSigninHandler 由 app 层实现：真正去登录站点的那部分逻辑依赖站点适配器与凭据。
// 返回报告而不是只返回 error，是为了让「有几个站点被跳过、为什么」能被看到。
type SiteSigninHandler interface {
	SigninEnabledSites(context.Context) (SiteSigninReport, error)
}

// SiteSigninSweepArgs 触发一轮站点签到。
type SiteSigninSweepArgs struct {
	Key string `json:"key"`
}

// Kind 实现 river.JobArgs。
func (SiteSigninSweepArgs) Kind() string { return siteSigninSweepKind }

type siteSigninSweepWorker struct {
	river.WorkerDefaults[SiteSigninSweepArgs]

	handler SiteSigninHandler
	logger  *slog.Logger
}

// Work 跑一轮签到。单个站点失败不影响其他站点，整轮只在通道级错误时返回 error。
func (w *siteSigninSweepWorker) Work(ctx context.Context, _ *river.Job[SiteSigninSweepArgs]) error {
	if w.handler == nil {
		return nil
	}
	report, err := w.handler.SigninEnabledSites(ctx)
	if err != nil {
		w.log().Warn("站点签到失败", "err", err)
		return err
	}
	w.log().Info("站点签到完成",
		"signed", report.Signed, "skipped", report.Skipped, "failed", report.Failed)
	for _, item := range report.Items {
		if item.Status == "failed" {
			w.log().Warn("站点签到未成功",
				"site_id", item.SiteID, "site", item.Site, "status", item.Status, "reason", item.Reason)
		}
	}
	return nil
}

func (w *siteSigninSweepWorker) log() *slog.Logger {
	if w.logger != nil {
		return w.logger
	}
	return slog.Default()
}

// registerSiteSigninSweepWorker 注册签到 worker；handler 为 nil 时不启用。
func registerSiteSigninSweepWorker(workers *river.Workers, handler SiteSigninHandler, logger *slog.Logger) (bool, error) {
	if handler == nil {
		return false, nil
	}
	river.AddWorker(workers, &siteSigninSweepWorker{handler: handler, logger: logger})
	return true, nil
}

// siteSigninSweepPeriodicJob 每天插入一次签到任务。
func siteSigninSweepPeriodicJob() *river.PeriodicJob {
	return river.NewPeriodicJob(
		river.PeriodicInterval(siteSigninSweepInterval),
		func() (river.JobArgs, *river.InsertOpts) {
			return SiteSigninSweepArgs{Key: "daily"}, nil
		},
		&river.PeriodicJobOpts{ID: siteSigninSweepPeriodicID},
	)
}

// 「能力未启用」与「入队失败」必须能被调用方区分：前者是配置问题，
// 后者是持久队列问题，混成一个错误会让排障无处下手。
var (
	ErrSiteSigninSweepUnavailable = errors.New("站点签到处理器未配置")
	ErrSiteSigninSweepExecution   = errors.New("执行站点签到任务失败")
)

// siteSigninSweepMinInterval 是插件配置允许的最短周期：再短就变成轮询站点，
// 既没必要也不礼貌。
const siteSigninSweepMinInterval = time.Hour

// siteSigninRuntimePeriodicJob 用于热更新，ID 留空由 River 生成：
// River 的 Remove 不释放 ID 唯一性，复用同名 ID 会被判重复（RSS 扫描同法）。
func siteSigninRuntimePeriodicJob(interval time.Duration) *river.PeriodicJob {
	return river.NewPeriodicJob(
		river.PeriodicInterval(normalizeSiteSigninSweepInterval(interval)),
		func() (river.JobArgs, *river.InsertOpts) {
			return SiteSigninSweepArgs{Key: "daily"}, nil
		},
		&river.PeriodicJobOpts{},
	)
}

func normalizeSiteSigninSweepInterval(configured time.Duration) time.Duration {
	if configured < siteSigninSweepMinInterval {
		return siteSigninSweepMinInterval
	}
	return configured
}

// SetSiteSigninSweepInterval 换一个签到周期：不重启 River 客户端，也不立即触发一轮。
func (m *Manager) SetSiteSigninSweepInterval(interval time.Duration) error {
	if m == nil || !m.siteSigninSweepEnabled || m.siteSigninPeriodicJobs == nil {
		return ErrSiteSigninSweepUnavailable
	}
	m.siteSigninPeriodicMu.Lock()
	defer m.siteSigninPeriodicMu.Unlock()
	newHandle, err := m.siteSigninPeriodicJobs.AddSafely(siteSigninRuntimePeriodicJob(interval))
	if err != nil {
		return fmt.Errorf("更新站点签到周期任务: %w", err)
	}
	m.siteSigninPeriodicJobs.Remove(m.siteSigninPeriodicHandle)
	m.siteSigninPeriodicHandle = newHandle
	m.siteSigninPeriodicInterval = normalizeSiteSigninSweepInterval(interval)
	return nil
}

// EnqueueSiteSigninSweep 立即插入一轮站点签到，供插件动作与运维手工触发使用。
// 不加唯一约束：手工触发要的是「点一次就有一轮」，不与当天的周期任务合并。
func (m *Manager) EnqueueSiteSigninSweep(ctx context.Context) error {
	if ctx == nil {
		return ErrSiteSigninSweepExecution
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if m == nil || m.client == nil || !m.siteSigninSweepEnabled {
		return ErrSiteSigninSweepUnavailable
	}
	opts := river.InsertOpts{Queue: queueSiteSigninSweep, MaxAttempts: 3}
	if _, err := m.client.Insert(ctx, SiteSigninSweepArgs{Key: "manual"}, &opts); err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return contextErr
		}
		return ErrSiteSigninSweepExecution
	}
	return nil
}

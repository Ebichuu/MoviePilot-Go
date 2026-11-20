package jobs

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

const (
	queueSiteUserDataRefresh      = "site_userdata_refresh"
	siteUserDataRefreshKind       = "site_userdata_refresh"
	siteUserDataRefreshPeriodicID = "site-userdata-refresh-v1"
	siteUserDataRefreshKey        = "all"
	siteUserDataRefreshInterval   = 24 * time.Hour
	siteUserDataRefreshTimeout    = 30 * time.Minute
)

var (
	ErrInvalidSiteUserDataRefreshArgs = errors.New("站点用户数据批量刷新任务参数无效")
	ErrSiteUserDataRefreshUnavailable = errors.New("站点用户数据批量刷新任务处理器未配置")
	ErrSiteUserDataRefreshExecution   = errors.New("执行站点用户数据批量刷新任务失败")
)

// SiteUserDataRefreshArgs 触发一次 V2 `refresh_userdatas` 批量刷新。
type SiteUserDataRefreshArgs struct {
	Key string `json:"key" river:"unique"`
}

func (SiteUserDataRefreshArgs) Kind() string { return siteUserDataRefreshKind }

func (SiteUserDataRefreshArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue:       queueSiteUserDataRefresh,
		MaxAttempts: 3,
		UniqueOpts: river.UniqueOpts{
			ByArgs: true,
			ByState: []rivertype.JobState{
				rivertype.JobStateAvailable,
				rivertype.JobStatePending,
				rivertype.JobStateRetryable,
				rivertype.JobStateRunning,
				rivertype.JobStateScheduled,
			},
		},
	}
}

func (a SiteUserDataRefreshArgs) Validate() error {
	if strings.TrimSpace(a.Key) != siteUserDataRefreshKey {
		return ErrInvalidSiteUserDataRefreshArgs
	}
	return nil
}

// SiteUserDataRefreshHandler 执行一次批量刷新。
type SiteUserDataRefreshHandler interface {
	HandleSiteUserDataRefresh(context.Context) error
}

type siteUserDataRefreshWorker struct {
	river.WorkerDefaults[SiteUserDataRefreshArgs]
	handler SiteUserDataRefreshHandler
}

func (w *siteUserDataRefreshWorker) Work(ctx context.Context, job *river.Job[SiteUserDataRefreshArgs]) error {
	if ctx == nil {
		return ErrSiteUserDataRefreshExecution
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if w == nil || w.handler == nil {
		return ErrSiteUserDataRefreshUnavailable
	}
	if job == nil {
		return river.JobCancel(ErrInvalidSiteUserDataRefreshArgs)
	}
	if err := job.Args.Validate(); err != nil {
		return river.JobCancel(err)
	}
	if err := w.handler.HandleSiteUserDataRefresh(ctx); err != nil {
		if contextErr := sanitizedSiteUserDataContextError(ctx, err); contextErr != nil {
			return contextErr
		}
		return ErrSiteUserDataRefreshExecution
	}
	return nil
}

func (*siteUserDataRefreshWorker) Timeout(*river.Job[SiteUserDataRefreshArgs]) time.Duration {
	return siteUserDataRefreshTimeout
}

func registerSiteUserDataRefreshWorker(workers *river.Workers, handler SiteUserDataRefreshHandler) (bool, error) {
	if handler == nil {
		return false, nil
	}
	if err := river.AddWorkerSafely(workers, &siteUserDataRefreshWorker{handler: handler}); err != nil {
		return false, errors.New("注册站点用户数据批量刷新 worker 失败")
	}
	return true, nil
}

func siteUserDataRefreshPeriodicJob() *river.PeriodicJob {
	return river.NewPeriodicJob(
		river.PeriodicInterval(siteUserDataRefreshInterval),
		siteUserDataRefreshPeriodicArgs,
		&river.PeriodicJobOpts{ID: siteUserDataRefreshPeriodicID, RunOnStart: false},
	)
}

func siteUserDataRefreshPeriodicArgs() (river.JobArgs, *river.InsertOpts) {
	args := SiteUserDataRefreshArgs{Key: siteUserDataRefreshKey}
	opts := args.InsertOpts()
	return args, &opts
}

// EnqueueSiteUserDataRefresh 触发一次即时的批量刷新。
func (m *Manager) EnqueueSiteUserDataRefresh(ctx context.Context) error {
	if ctx == nil {
		return ErrSiteUserDataRefreshExecution
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if m == nil || m.client == nil || !m.siteUserDataRefreshEnabled {
		return ErrSiteUserDataRefreshUnavailable
	}
	args := SiteUserDataRefreshArgs{Key: siteUserDataRefreshKey}
	opts := args.InsertOpts()
	if _, err := m.client.Insert(ctx, args, &opts); err != nil {
		if contextErr := sanitizedSiteUserDataContextError(ctx, err); contextErr != nil {
			return contextErr
		}
		return ErrSiteUserDataRefreshExecution
	}
	return nil
}

func sanitizedSiteUserDataContextError(ctx context.Context, err error) error {
	if ctx != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return nil
}

var _ river.JobArgsWithInsertOpts = SiteUserDataRefreshArgs{}
var _ river.Worker[SiteUserDataRefreshArgs] = (*siteUserDataRefreshWorker)(nil)

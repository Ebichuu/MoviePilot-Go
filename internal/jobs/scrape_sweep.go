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
	queueScrapeSweep      = "scrape_sweep"
	scrapeSweepKind       = "scrape_recovery_sweep"
	scrapeSweepPeriodicID = "scrape-recovery-v1"
	scrapeSweepKey        = "due"
	scrapeSweepInterval   = time.Minute
	scrapeSweepTimeout    = 2 * time.Minute
)

var (
	ErrInvalidScrapeSweepArgs        = errors.New("NFO 恢复任务参数无效")
	ErrScrapeSweepHandlerUnavailable = errors.New("NFO 恢复任务处理器未配置")
	ErrScrapeSweepExecution          = errors.New("执行 NFO 恢复任务失败")
)

type ScrapeSweepArgs struct {
	Key string `json:"key" river:"unique"`
}

func (ScrapeSweepArgs) Kind() string { return scrapeSweepKind }
func (ScrapeSweepArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: queueScrapeSweep, MaxAttempts: 5,
		UniqueOpts: river.UniqueOpts{ByArgs: true, ByState: []rivertype.JobState{
			rivertype.JobStateAvailable, rivertype.JobStatePending, rivertype.JobStateRetryable,
			rivertype.JobStateRunning, rivertype.JobStateScheduled,
		}}}
}
func (a ScrapeSweepArgs) Validate() error {
	if strings.TrimSpace(a.Key) != scrapeSweepKey {
		return ErrInvalidScrapeSweepArgs
	}
	return nil
}

type ScrapeSweepHandler interface{ HandleScrapeSweep(context.Context) error }
type scrapeSweepWorker struct {
	river.WorkerDefaults[ScrapeSweepArgs]
	handler ScrapeSweepHandler
}

func (w *scrapeSweepWorker) Work(ctx context.Context, job *river.Job[ScrapeSweepArgs]) error {
	if ctx == nil {
		return ErrScrapeSweepExecution
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if w == nil || w.handler == nil {
		return ErrScrapeSweepHandlerUnavailable
	}
	if job == nil {
		return river.JobCancel(ErrInvalidScrapeSweepArgs)
	}
	if err := job.Args.Validate(); err != nil {
		return river.JobCancel(err)
	}
	if err := w.handler.HandleScrapeSweep(ctx); err != nil {
		if contextErr := sanitizedScrapeContextError(ctx, err); contextErr != nil {
			return contextErr
		}
		return ErrScrapeSweepExecution
	}
	return nil
}
func (*scrapeSweepWorker) Timeout(*river.Job[ScrapeSweepArgs]) time.Duration {
	return scrapeSweepTimeout
}
func registerScrapeSweepWorker(workers *river.Workers, handler ScrapeSweepHandler) (bool, error) {
	if handler == nil {
		return false, nil
	}
	if err := river.AddWorkerSafely(workers, &scrapeSweepWorker{handler: handler}); err != nil {
		return false, errors.New("注册 NFO 恢复 worker 失败")
	}
	return true, nil
}
func scrapeSweepPeriodicJob() *river.PeriodicJob {
	return river.NewPeriodicJob(river.PeriodicInterval(scrapeSweepInterval), scrapeSweepPeriodicArgs, &river.PeriodicJobOpts{ID: scrapeSweepPeriodicID, RunOnStart: true})
}
func scrapeSweepPeriodicArgs() (river.JobArgs, *river.InsertOpts) {
	args := ScrapeSweepArgs{Key: scrapeSweepKey}
	opts := args.InsertOpts()
	return args, &opts
}
func (m *Manager) EnqueueScrapeSweep(ctx context.Context) error {
	if ctx == nil {
		return ErrScrapeSweepExecution
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if m == nil || m.client == nil || !m.scrapeSweepEnabled {
		return ErrScrapeSweepHandlerUnavailable
	}
	args := ScrapeSweepArgs{Key: scrapeSweepKey}
	opts := args.InsertOpts()
	if _, err := m.client.Insert(ctx, args, &opts); err != nil {
		if contextErr := sanitizedScrapeContextError(ctx, err); contextErr != nil {
			return contextErr
		}
		return ErrScrapeSweepExecution
	}
	return nil
}

var _ river.JobArgsWithInsertOpts = ScrapeSweepArgs{}
var _ river.Worker[ScrapeSweepArgs] = (*scrapeSweepWorker)(nil)

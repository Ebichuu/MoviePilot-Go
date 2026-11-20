package jobs

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

const (
	queueScrape             = "scrape"
	scrapeKind              = "scrape_request"
	scrapeDefaultMaxWorkers = 4
	scrapeMaxWorkersLimit   = 8
	scrapeMaxAttempts       = 5
	scrapeJobTimeout        = 90 * time.Second
)

var (
	ErrInvalidScrapeArgs        = errors.New("NFO 任务参数无效")
	ErrScrapeHandlerUnavailable = errors.New("NFO 任务处理器未配置")
	ErrScrapeEnqueue            = errors.New("插入 NFO 任务失败")
	ErrScrapeExecution          = errors.New("执行 NFO 任务失败")
)

// ScrapeArgs 只携带持久化 NFO 请求 UUID。
type ScrapeArgs struct {
	RequestID uuid.UUID `json:"request_id" river:"unique"`
}

func (ScrapeArgs) Kind() string { return scrapeKind }

func (ScrapeArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: queueScrape, MaxAttempts: scrapeMaxAttempts,
		UniqueOpts: river.UniqueOpts{ByArgs: true, ByState: []rivertype.JobState{
			rivertype.JobStateAvailable, rivertype.JobStatePending, rivertype.JobStateRetryable,
			rivertype.JobStateRunning, rivertype.JobStateScheduled,
		}}}
}

func (a ScrapeArgs) Validate() error {
	if a.RequestID == uuid.Nil {
		return ErrInvalidScrapeArgs
	}
	return nil
}

type ScrapeHandler interface {
	HandleScrape(context.Context, ScrapeArgs) error
}

type scrapeWorker struct {
	river.WorkerDefaults[ScrapeArgs]
	handler ScrapeHandler
}

func (w *scrapeWorker) Work(ctx context.Context, job *river.Job[ScrapeArgs]) error {
	if ctx == nil {
		return ErrScrapeExecution
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if w == nil || w.handler == nil {
		return ErrScrapeHandlerUnavailable
	}
	if job == nil {
		return river.JobCancel(ErrInvalidScrapeArgs)
	}
	if err := job.Args.Validate(); err != nil {
		return river.JobCancel(err)
	}
	if err := w.handler.HandleScrape(ctx, job.Args); err != nil {
		if contextErr := sanitizedScrapeContextError(ctx, err); contextErr != nil {
			return contextErr
		}
		return ErrScrapeExecution
	}
	return nil
}

func (*scrapeWorker) Timeout(*river.Job[ScrapeArgs]) time.Duration { return scrapeJobTimeout }

func registerScrapeWorker(workers *river.Workers, config ManagerConfig) (int, error) {
	if config.ScrapeHandler == nil {
		if config.ScrapeMaxWorkers != 0 {
			return 0, errors.New("NFO 队列并发配置缺少处理器")
		}
		return 0, nil
	}
	maxWorkers, err := normalizeScrapeMaxWorkers(config.ScrapeMaxWorkers)
	if err != nil {
		return 0, err
	}
	if err := river.AddWorkerSafely(workers, &scrapeWorker{handler: config.ScrapeHandler}); err != nil {
		return 0, errors.New("注册 NFO worker 失败")
	}
	return maxWorkers, nil
}

func normalizeScrapeMaxWorkers(requested int) (int, error) {
	if requested < 0 {
		return 0, errors.New("NFO 队列并发配置无效")
	}
	if requested == 0 {
		return scrapeDefaultMaxWorkers, nil
	}
	return min(requested, scrapeMaxWorkersLimit), nil
}

// EnqueueScrape 幂等插入一个活跃 NFO 请求任务。
func (m *Manager) EnqueueScrape(ctx context.Context, requestID uuid.UUID) error {
	args := ScrapeArgs{RequestID: requestID}
	if err := args.Validate(); err != nil {
		return err
	}
	if ctx == nil {
		return ErrScrapeEnqueue
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if m == nil || m.client == nil || !m.scrapeEnabled {
		return ErrScrapeHandlerUnavailable
	}
	opts := args.InsertOpts()
	if _, err := m.client.Insert(ctx, args, &opts); err != nil {
		if contextErr := sanitizedScrapeContextError(ctx, err); contextErr != nil {
			return contextErr
		}
		return ErrScrapeEnqueue
	}
	return nil
}

func sanitizedScrapeContextError(ctx context.Context, err error) error {
	if ctx != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return contextErr
		}
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return nil
}

var _ river.JobArgsWithInsertOpts = ScrapeArgs{}
var _ river.Worker[ScrapeArgs] = (*scrapeWorker)(nil)

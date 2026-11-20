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
	queueMediaIndex             = "media_index"
	mediaIndexKind              = "media_index_request"
	mediaIndexDefaultMaxWorkers = 4
	mediaIndexMaxWorkersLimit   = 8
	mediaIndexMaxAttempts       = 5
	mediaIndexJobTimeout        = 10 * time.Minute
)

var (
	ErrInvalidMediaIndexArgs        = errors.New("媒体入库确认任务参数无效")
	ErrMediaIndexHandlerUnavailable = errors.New("媒体入库确认任务处理器未配置")
	ErrMediaIndexEnqueue            = errors.New("插入媒体入库确认任务失败")
	ErrMediaIndexExecution          = errors.New("执行媒体入库确认任务失败")
)

// MediaIndexArgs 只携带 PostgreSQL 中持久请求的稳定标识。
type MediaIndexArgs struct {
	RequestID uuid.UUID `json:"request_id" river:"unique"`
}

func (MediaIndexArgs) Kind() string { return mediaIndexKind }

func (MediaIndexArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue:       queueMediaIndex,
		MaxAttempts: mediaIndexMaxAttempts,
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

func (a MediaIndexArgs) Validate() error {
	if a.RequestID == uuid.Nil {
		return ErrInvalidMediaIndexArgs
	}
	return nil
}

// MediaIndexHandler 执行一次有界刷新或确认，不在 Worker 内长期轮询。
type MediaIndexHandler interface {
	HandleMediaIndex(context.Context, MediaIndexArgs) error
}

type mediaIndexWorker struct {
	river.WorkerDefaults[MediaIndexArgs]
	handler MediaIndexHandler
}

func (w *mediaIndexWorker) Work(ctx context.Context, job *river.Job[MediaIndexArgs]) error {
	if ctx == nil {
		return ErrMediaIndexExecution
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if w == nil || w.handler == nil {
		return ErrMediaIndexHandlerUnavailable
	}
	if job == nil {
		return river.JobCancel(ErrInvalidMediaIndexArgs)
	}
	if err := job.Args.Validate(); err != nil {
		return river.JobCancel(err)
	}
	if err := w.handler.HandleMediaIndex(ctx, job.Args); err != nil {
		if contextErr := sanitizedMediaIndexContextError(ctx, err); contextErr != nil {
			return contextErr
		}
		return ErrMediaIndexExecution
	}
	return nil
}

func (*mediaIndexWorker) Timeout(*river.Job[MediaIndexArgs]) time.Duration {
	return mediaIndexJobTimeout
}

func registerMediaIndexWorker(workers *river.Workers, config ManagerConfig) (int, error) {
	if config.MediaIndexHandler == nil {
		if config.MediaIndexMaxWorkers != 0 {
			return 0, errors.New("媒体入库确认队列并发配置缺少处理器")
		}
		return 0, nil
	}
	maxWorkers, err := normalizeMediaIndexMaxWorkers(config.MediaIndexMaxWorkers)
	if err != nil {
		return 0, err
	}
	if err := river.AddWorkerSafely(workers, &mediaIndexWorker{handler: config.MediaIndexHandler}); err != nil {
		return 0, errors.New("注册媒体入库确认 worker 失败")
	}
	return maxWorkers, nil
}

func normalizeMediaIndexMaxWorkers(requested int) (int, error) {
	if requested < 0 {
		return 0, errors.New("媒体入库确认队列并发配置无效")
	}
	if requested == 0 {
		return mediaIndexDefaultMaxWorkers, nil
	}
	return min(requested, mediaIndexMaxWorkersLimit), nil
}

// EnqueueMediaIndex 幂等插入一个活跃的媒体入库确认任务。
func (m *Manager) EnqueueMediaIndex(ctx context.Context, args MediaIndexArgs) error {
	if err := args.Validate(); err != nil {
		return err
	}
	if ctx == nil {
		return ErrMediaIndexEnqueue
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if m == nil || m.client == nil || !m.mediaIndexEnabled {
		return ErrMediaIndexHandlerUnavailable
	}
	opts := args.InsertOpts()
	if _, err := m.client.Insert(ctx, args, &opts); err != nil {
		if contextErr := sanitizedMediaIndexContextError(ctx, err); contextErr != nil {
			return contextErr
		}
		return ErrMediaIndexEnqueue
	}
	return nil
}

func sanitizedMediaIndexContextError(ctx context.Context, err error) error {
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

var _ river.JobArgsWithInsertOpts = MediaIndexArgs{}
var _ river.Worker[MediaIndexArgs] = (*mediaIndexWorker)(nil)

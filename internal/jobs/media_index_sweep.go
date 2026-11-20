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
	queueMediaIndexSweep      = "media_index_sweep"
	mediaIndexSweepKind       = "media_index_recovery_sweep"
	mediaIndexSweepPeriodicID = "media-index-recovery-v1"
	mediaIndexSweepKey        = "due"
	mediaIndexSweepInterval   = time.Minute
	mediaIndexSweepTimeout    = 2 * time.Minute
)

var (
	ErrInvalidMediaIndexSweepArgs        = errors.New("媒体入库确认恢复任务参数无效")
	ErrMediaIndexSweepHandlerUnavailable = errors.New("媒体入库确认恢复任务处理器未配置")
	ErrMediaIndexSweepExecution          = errors.New("执行媒体入库确认恢复任务失败")
)

type MediaIndexSweepArgs struct {
	Key string `json:"key" river:"unique"`
}

func (MediaIndexSweepArgs) Kind() string { return mediaIndexSweepKind }

func (MediaIndexSweepArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue:       queueMediaIndexSweep,
		MaxAttempts: 5,
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

func (a MediaIndexSweepArgs) Validate() error {
	if strings.TrimSpace(a.Key) != mediaIndexSweepKey {
		return ErrInvalidMediaIndexSweepArgs
	}
	return nil
}

type MediaIndexSweepHandler interface {
	HandleMediaIndexSweep(context.Context) error
}

type mediaIndexSweepWorker struct {
	river.WorkerDefaults[MediaIndexSweepArgs]
	handler MediaIndexSweepHandler
}

func (w *mediaIndexSweepWorker) Work(ctx context.Context, job *river.Job[MediaIndexSweepArgs]) error {
	if ctx == nil {
		return ErrMediaIndexSweepExecution
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if w == nil || w.handler == nil {
		return ErrMediaIndexSweepHandlerUnavailable
	}
	if job == nil {
		return river.JobCancel(ErrInvalidMediaIndexSweepArgs)
	}
	if err := job.Args.Validate(); err != nil {
		return river.JobCancel(err)
	}
	if err := w.handler.HandleMediaIndexSweep(ctx); err != nil {
		if contextErr := sanitizedMediaIndexContextError(ctx, err); contextErr != nil {
			return contextErr
		}
		return ErrMediaIndexSweepExecution
	}
	return nil
}

func (*mediaIndexSweepWorker) Timeout(*river.Job[MediaIndexSweepArgs]) time.Duration {
	return mediaIndexSweepTimeout
}

func registerMediaIndexSweepWorker(workers *river.Workers, handler MediaIndexSweepHandler) (bool, error) {
	if handler == nil {
		return false, nil
	}
	if err := river.AddWorkerSafely(workers, &mediaIndexSweepWorker{handler: handler}); err != nil {
		return false, errors.New("注册媒体入库确认恢复 worker 失败")
	}
	return true, nil
}

func mediaIndexSweepPeriodicJob() *river.PeriodicJob {
	return river.NewPeriodicJob(
		river.PeriodicInterval(mediaIndexSweepInterval),
		mediaIndexSweepPeriodicArgs,
		&river.PeriodicJobOpts{ID: mediaIndexSweepPeriodicID, RunOnStart: true},
	)
}

func mediaIndexSweepPeriodicArgs() (river.JobArgs, *river.InsertOpts) {
	args := MediaIndexSweepArgs{Key: mediaIndexSweepKey}
	opts := args.InsertOpts()
	return args, &opts
}

// EnqueueMediaIndexSweep 触发一次即时的有界恢复扫描。
func (m *Manager) EnqueueMediaIndexSweep(ctx context.Context) error {
	if ctx == nil {
		return ErrMediaIndexSweepExecution
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if m == nil || m.client == nil || !m.mediaIndexSweepEnabled {
		return ErrMediaIndexSweepHandlerUnavailable
	}
	args := MediaIndexSweepArgs{Key: mediaIndexSweepKey}
	opts := args.InsertOpts()
	if _, err := m.client.Insert(ctx, args, &opts); err != nil {
		if contextErr := sanitizedMediaIndexContextError(ctx, err); contextErr != nil {
			return contextErr
		}
		return ErrMediaIndexSweepExecution
	}
	return nil
}

var _ river.JobArgsWithInsertOpts = MediaIndexSweepArgs{}
var _ river.Worker[MediaIndexSweepArgs] = (*mediaIndexSweepWorker)(nil)

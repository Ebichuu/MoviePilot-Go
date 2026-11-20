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
	queueDownloadReconcile       = "download_reconcile"
	downloadReconcileKind        = "download_reconcile_sweep"
	downloadReconcilePeriodicID  = "download-reconcile-v1"
	downloadReconcileKey         = "active"
	downloadReconcileInterval    = time.Minute
	downloadReconcileJobTimeout  = 10 * time.Minute
	downloadReconcileMaxAttempts = 5
)

var (
	// ErrInvalidDownloadReconcileArgs indicates malformed persistent sweep arguments.
	ErrInvalidDownloadReconcileArgs = errors.New("下载对账任务参数无效")
	// ErrDownloadReconcileHandlerUnavailable indicates missing sweep dependencies.
	ErrDownloadReconcileHandlerUnavailable = errors.New("下载对账任务处理器未配置")
	// ErrDownloadReconcileExecution is the stable River-visible sweep failure.
	ErrDownloadReconcileExecution = errors.New("执行下载对账任务失败")
)

// DownloadReconcileSweepArgs identifies one fixed reconciliation snapshot.
// Only Key participates in uniqueness; Cutoff is the snapshot boundary.
type DownloadReconcileSweepArgs struct {
	Key    string    `json:"key" river:"unique"`
	Cutoff time.Time `json:"cutoff"`
}

// Kind returns the stable River job kind.
func (DownloadReconcileSweepArgs) Kind() string { return downloadReconcileKind }

// InsertOpts keeps at most one active sweep while allowing a new sweep after
// the previous job has completed and remains retained in River.
func (DownloadReconcileSweepArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue:       queueDownloadReconcile,
		MaxAttempts: downloadReconcileMaxAttempts,
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

// Validate rejects stale or malformed scheduler input before business work.
func (a DownloadReconcileSweepArgs) Validate(now time.Time) error {
	if strings.TrimSpace(a.Key) != downloadReconcileKey || a.Cutoff.IsZero() {
		return ErrInvalidDownloadReconcileArgs
	}
	if a.Cutoff.After(now.Add(time.Minute)) {
		return ErrInvalidDownloadReconcileArgs
	}
	return nil
}

// DownloadReconcileHandler executes one fixed-cutoff, bounded-page sweep.
type DownloadReconcileHandler interface {
	HandleDownloadReconciliation(ctx context.Context, cutoff time.Time) error
}

type downloadReconcileWorker struct {
	river.WorkerDefaults[DownloadReconcileSweepArgs]
	handler DownloadReconcileHandler
	now     func() time.Time
}

func (w *downloadReconcileWorker) Work(ctx context.Context, job *river.Job[DownloadReconcileSweepArgs]) error {
	if ctx == nil {
		return ErrDownloadReconcileExecution
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if w == nil || w.handler == nil {
		return ErrDownloadReconcileHandlerUnavailable
	}
	if job == nil {
		return ErrInvalidDownloadReconcileArgs
	}
	now := time.Now().UTC()
	if w.now != nil {
		now = w.now().UTC()
	}
	if err := job.Args.Validate(now); err != nil {
		return err
	}
	if err := w.handler.HandleDownloadReconciliation(ctx, job.Args.Cutoff.UTC()); err != nil {
		if contextErr := sanitizedDownloadContextError(ctx, err); contextErr != nil {
			return contextErr
		}
		return ErrDownloadReconcileExecution
	}
	return nil
}

func (*downloadReconcileWorker) Timeout(*river.Job[DownloadReconcileSweepArgs]) time.Duration {
	return downloadReconcileJobTimeout
}

func registerDownloadReconcileWorker(
	workers *river.Workers,
	handler DownloadReconcileHandler,
) (bool, error) {
	if handler == nil {
		return false, nil
	}
	if err := river.AddWorkerSafely(workers, &downloadReconcileWorker{handler: handler}); err != nil {
		return false, errors.New("注册下载对账任务 worker 失败")
	}
	return true, nil
}

func downloadReconcilePeriodicJob() *river.PeriodicJob {
	return river.NewPeriodicJob(
		river.PeriodicInterval(downloadReconcileInterval),
		downloadReconcilePeriodicArgs,
		&river.PeriodicJobOpts{ID: downloadReconcilePeriodicID, RunOnStart: true},
	)
}

func downloadReconcilePeriodicArgs() (river.JobArgs, *river.InsertOpts) {
	args := DownloadReconcileSweepArgs{Key: downloadReconcileKey, Cutoff: time.Now().UTC()}
	opts := args.InsertOpts()
	return args, &opts
}

// EnqueueDownloadReconcile 立即插入一轮下载状态对账任务。
func (m *Manager) EnqueueDownloadReconcile(ctx context.Context) error {
	if ctx == nil {
		return ErrDownloadReconcileExecution
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if m == nil || m.client == nil || !m.downloadReconcileEnabled {
		return ErrDownloadReconcileExecution
	}
	args := DownloadReconcileSweepArgs{Key: downloadReconcileKey, Cutoff: time.Now().UTC()}
	opts := args.InsertOpts()
	if _, err := m.client.Insert(ctx, args, &opts); err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return contextErr
		}
		return ErrDownloadReconcileExecution
	}
	return nil
}

var _ river.JobArgsWithInsertOpts = DownloadReconcileSweepArgs{}
var _ river.Worker[DownloadReconcileSweepArgs] = (*downloadReconcileWorker)(nil)

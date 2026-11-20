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
	queueNotificationSweep      = "notification_sweep"
	notificationSweepKind       = "notification_recovery_sweep"
	notificationSweepPeriodicID = "notification-recovery-v1"
	notificationSweepKey        = "due"
	notificationSweepInterval   = time.Minute
	notificationSweepTimeout    = 2 * time.Minute
)

var (
	ErrInvalidNotificationSweepArgs = errors.New("通知恢复任务参数无效")
	ErrNotificationSweepUnavailable = errors.New("通知恢复任务处理器未配置")
	ErrNotificationSweepExecution   = errors.New("执行通知恢复任务失败")
)

type NotificationSweepArgs struct {
	Key string `json:"key" river:"unique"`
}

func (NotificationSweepArgs) Kind() string { return notificationSweepKind }
func (NotificationSweepArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: queueNotificationSweep, MaxAttempts: 5, UniqueOpts: river.UniqueOpts{ByArgs: true, ByState: []rivertype.JobState{rivertype.JobStateAvailable, rivertype.JobStatePending, rivertype.JobStateRetryable, rivertype.JobStateRunning, rivertype.JobStateScheduled}}}
}
func (a NotificationSweepArgs) Validate() error {
	if strings.TrimSpace(a.Key) != notificationSweepKey {
		return ErrInvalidNotificationSweepArgs
	}
	return nil
}

type NotificationSweepHandler interface{ HandleNotificationSweep(context.Context) error }
type notificationSweepWorker struct {
	river.WorkerDefaults[NotificationSweepArgs]
	handler NotificationSweepHandler
}

func (w *notificationSweepWorker) Work(ctx context.Context, job *river.Job[NotificationSweepArgs]) error {
	if ctx == nil {
		return ErrNotificationSweepExecution
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if w == nil || w.handler == nil {
		return ErrNotificationSweepUnavailable
	}
	if job == nil {
		return river.JobCancel(ErrInvalidNotificationSweepArgs)
	}
	if err := job.Args.Validate(); err != nil {
		return river.JobCancel(err)
	}
	if err := w.handler.HandleNotificationSweep(ctx); err != nil {
		if contextErr := sanitizedNotificationContextError(ctx, err); contextErr != nil {
			return contextErr
		}
		return ErrNotificationSweepExecution
	}
	return nil
}
func (*notificationSweepWorker) Timeout(*river.Job[NotificationSweepArgs]) time.Duration {
	return notificationSweepTimeout
}
func registerNotificationSweepWorker(workers *river.Workers, handler NotificationSweepHandler) (bool, error) {
	if handler == nil {
		return false, nil
	}
	if err := river.AddWorkerSafely(workers, &notificationSweepWorker{handler: handler}); err != nil {
		return false, errors.New("注册通知恢复 worker 失败")
	}
	return true, nil
}
func notificationSweepPeriodicJob() *river.PeriodicJob {
	return river.NewPeriodicJob(river.PeriodicInterval(notificationSweepInterval), notificationSweepPeriodicArgs, &river.PeriodicJobOpts{ID: notificationSweepPeriodicID, RunOnStart: true})
}
func notificationSweepPeriodicArgs() (river.JobArgs, *river.InsertOpts) {
	args := NotificationSweepArgs{Key: notificationSweepKey}
	opts := args.InsertOpts()
	return args, &opts
}
func (m *Manager) EnqueueNotificationSweep(ctx context.Context) error {
	if ctx == nil {
		return ErrNotificationSweepExecution
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if m == nil || m.client == nil || !m.notificationSweepEnabled {
		return ErrNotificationSweepUnavailable
	}
	args := NotificationSweepArgs{Key: notificationSweepKey}
	opts := args.InsertOpts()
	if _, err := m.client.Insert(ctx, args, &opts); err != nil {
		if contextErr := sanitizedNotificationContextError(ctx, err); contextErr != nil {
			return contextErr
		}
		return ErrNotificationSweepExecution
	}
	return nil
}

var _ river.JobArgsWithInsertOpts = NotificationSweepArgs{}
var _ river.Worker[NotificationSweepArgs] = (*notificationSweepWorker)(nil)

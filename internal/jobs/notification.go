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
	queueNotification             = "notification"
	notificationKind              = "notification_request"
	notificationDefaultMaxWorkers = 2
	notificationMaxWorkersLimit   = 8
	notificationMaxAttempts       = 5
	notificationJobTimeout        = 90 * time.Second
)

var (
	ErrInvalidNotificationArgs        = errors.New("通知任务参数无效")
	ErrNotificationHandlerUnavailable = errors.New("通知任务处理器未配置")
	ErrNotificationEnqueue            = errors.New("插入通知任务失败")
	ErrNotificationExecution          = errors.New("执行通知任务失败")
)

type NotificationArgs struct {
	RequestID uuid.UUID `json:"request_id" river:"unique"`
}

func (NotificationArgs) Kind() string { return notificationKind }
func (NotificationArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: queueNotification, MaxAttempts: notificationMaxAttempts,
		UniqueOpts: river.UniqueOpts{ByArgs: true, ByState: []rivertype.JobState{
			rivertype.JobStateAvailable, rivertype.JobStatePending, rivertype.JobStateRetryable,
			rivertype.JobStateRunning, rivertype.JobStateScheduled,
		}}}
}
func (a NotificationArgs) Validate() error {
	if a.RequestID == uuid.Nil {
		return ErrInvalidNotificationArgs
	}
	return nil
}

type NotificationHandler interface {
	HandleNotification(context.Context, NotificationArgs) error
}
type notificationWorker struct {
	river.WorkerDefaults[NotificationArgs]
	handler NotificationHandler
}

func (w *notificationWorker) Work(ctx context.Context, job *river.Job[NotificationArgs]) error {
	if ctx == nil {
		return ErrNotificationExecution
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if w == nil || w.handler == nil {
		return ErrNotificationHandlerUnavailable
	}
	if job == nil {
		return river.JobCancel(ErrInvalidNotificationArgs)
	}
	if err := job.Args.Validate(); err != nil {
		return river.JobCancel(err)
	}
	if err := w.handler.HandleNotification(ctx, job.Args); err != nil {
		if contextErr := sanitizedNotificationContextError(ctx, err); contextErr != nil {
			return contextErr
		}
		return ErrNotificationExecution
	}
	return nil
}
func (*notificationWorker) Timeout(*river.Job[NotificationArgs]) time.Duration {
	return notificationJobTimeout
}
func registerNotificationWorker(workers *river.Workers, config ManagerConfig) (int, error) {
	if config.NotificationHandler == nil {
		if config.NotificationMaxWorkers != 0 {
			return 0, errors.New("通知队列并发配置缺少处理器")
		}
		return 0, nil
	}
	maxWorkers, err := normalizeNotificationMaxWorkers(config.NotificationMaxWorkers)
	if err != nil {
		return 0, err
	}
	if err := river.AddWorkerSafely(workers, &notificationWorker{handler: config.NotificationHandler}); err != nil {
		return 0, errors.New("注册通知 worker 失败")
	}
	return maxWorkers, nil
}
func normalizeNotificationMaxWorkers(requested int) (int, error) {
	if requested < 0 {
		return 0, errors.New("通知队列并发配置无效")
	}
	if requested == 0 {
		return notificationDefaultMaxWorkers, nil
	}
	return min(requested, notificationMaxWorkersLimit), nil
}
func (m *Manager) EnqueueNotification(ctx context.Context, requestID uuid.UUID) error {
	args := NotificationArgs{RequestID: requestID}
	if err := args.Validate(); err != nil {
		return err
	}
	if ctx == nil {
		return ErrNotificationEnqueue
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if m == nil || m.client == nil || !m.notificationEnabled {
		return ErrNotificationHandlerUnavailable
	}
	opts := args.InsertOpts()
	if _, err := m.client.Insert(ctx, args, &opts); err != nil {
		if contextErr := sanitizedNotificationContextError(ctx, err); contextErr != nil {
			return contextErr
		}
		return ErrNotificationEnqueue
	}
	return nil
}
func sanitizedNotificationContextError(ctx context.Context, err error) error {
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

var _ river.JobArgsWithInsertOpts = NotificationArgs{}
var _ river.Worker[NotificationArgs] = (*notificationWorker)(nil)

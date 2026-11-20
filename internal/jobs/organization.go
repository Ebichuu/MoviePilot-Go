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
	queueOrganization = "organization"
	organizationKind  = "organization_batch"

	organizationDefaultMaxWorkers = 3
	organizationMaxWorkersLimit   = 8
	// 批次重试节奏由 organization_batches.next_attempt_at 和恢复扫描驱动，
	// River 层不再叠加指数退避：否则一次失败会把该批次压制数小时。
	organizationMaxAttempts = 1
	organizationJobTimeout  = 30 * time.Minute
)

var (
	// ErrInvalidOrganizationArgs indicates a missing or malformed batch ID.
	ErrInvalidOrganizationArgs = errors.New("整理任务参数无效")
	// ErrOrganizationHandlerUnavailable indicates that the business handler is not configured.
	ErrOrganizationHandlerUnavailable = errors.New("整理任务处理器未配置")
	// ErrOrganizationEnqueue indicates that River could not persist the job safely.
	ErrOrganizationEnqueue = errors.New("插入整理任务失败")
	// ErrOrganizationExecution is the stable River-visible execution failure.
	ErrOrganizationExecution = errors.New("执行整理任务失败")
)

// OrganizationArgs contains only the stable identifier of a persisted organization batch.
type OrganizationArgs struct {
	BatchID uuid.UUID `json:"batch_id" river:"unique"`
}

// Kind returns the stable River job kind.
func (OrganizationArgs) Kind() string { return organizationKind }

// InsertOpts configures the organization queue and uniqueness scope.
// Historical jobs are deliberately excluded so a completed batch can be retried
// or a new batch with the same scheduling key can be enqueued.
func (OrganizationArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue:       queueOrganization,
		MaxAttempts: organizationMaxAttempts,
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

// Validate rejects an empty batch ID before it reaches River or the handler.
func (a OrganizationArgs) Validate() error {
	if a.BatchID == uuid.Nil {
		return ErrInvalidOrganizationArgs
	}
	return nil
}

// OrganizationHandler executes one persisted organization batch.
type OrganizationHandler interface {
	HandleOrganization(ctx context.Context, args OrganizationArgs) error
}

type organizationWorker struct {
	river.WorkerDefaults[OrganizationArgs]
	handler OrganizationHandler
}

func (w *organizationWorker) Work(ctx context.Context, job *river.Job[OrganizationArgs]) error {
	if ctx == nil {
		return ErrOrganizationExecution
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if w == nil || w.handler == nil {
		return ErrOrganizationHandlerUnavailable
	}
	if job == nil {
		return ErrInvalidOrganizationArgs
	}
	args := job.Args
	if err := args.Validate(); err != nil {
		return err
	}
	if err := w.handler.HandleOrganization(ctx, args); err != nil {
		if contextErr := sanitizedOrganizationContextError(ctx, err); contextErr != nil {
			return contextErr
		}
		return ErrOrganizationExecution
	}
	return nil
}

func (*organizationWorker) Timeout(*river.Job[OrganizationArgs]) time.Duration {
	return organizationJobTimeout
}

func registerOrganizationWorker(workers *river.Workers, config ManagerConfig) (int, error) {
	if config.OrganizationHandler == nil {
		if config.OrganizationMaxWorkers != 0 {
			return 0, errors.New("整理队列并发配置缺少处理器")
		}
		return 0, nil
	}
	maxWorkers, err := normalizeOrganizationMaxWorkers(config.OrganizationMaxWorkers)
	if err != nil {
		return 0, err
	}
	if err := river.AddWorkerSafely(workers, &organizationWorker{handler: config.OrganizationHandler}); err != nil {
		return 0, errors.New("注册整理任务 worker 失败")
	}
	return maxWorkers, nil
}

func normalizeOrganizationMaxWorkers(requested int) (int, error) {
	if requested < 0 {
		return 0, errors.New("整理队列并发配置无效")
	}
	if requested == 0 {
		return organizationDefaultMaxWorkers, nil
	}
	if requested > organizationMaxWorkersLimit {
		return organizationMaxWorkersLimit, nil
	}
	return requested, nil
}

func sanitizedOrganizationContextError(ctx context.Context, err error) error {
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

var _ river.JobArgsWithInsertOpts = OrganizationArgs{}
var _ river.Worker[OrganizationArgs] = (*organizationWorker)(nil)

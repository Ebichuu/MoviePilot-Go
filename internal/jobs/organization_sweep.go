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
	queueOrganizationSweep      = "organization_sweep"
	organizationSweepKind       = "organization_recovery_sweep"
	organizationSweepPeriodicID = "organization-recovery-v1"
	organizationSweepKey        = "due"
	organizationSweepInterval   = time.Minute
	organizationSweepTimeout    = 5 * time.Minute
)

var (
	ErrInvalidOrganizationSweepArgs        = errors.New("整理恢复任务参数无效")
	ErrOrganizationSweepHandlerUnavailable = errors.New("整理恢复任务处理器未配置")
	ErrOrganizationSweepExecution          = errors.New("执行整理恢复任务失败")
)

// OrganizationSweepArgs identifies the singleton recovery scan. Business time
// is intentionally read at execution so a delayed River job never uses a stale
// lease cutoff.
type OrganizationSweepArgs struct {
	Key string `json:"key" river:"unique"`
}

func (OrganizationSweepArgs) Kind() string { return organizationSweepKind }

func (OrganizationSweepArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue:       queueOrganizationSweep,
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

func (a OrganizationSweepArgs) Validate() error {
	if strings.TrimSpace(a.Key) != organizationSweepKey {
		return ErrInvalidOrganizationSweepArgs
	}
	return nil
}

// OrganizationSweepHandler recovers expired leases and re-enqueues a bounded
// page of due batch IDs.
type OrganizationSweepHandler interface {
	HandleOrganizationSweep(context.Context) error
}

type organizationSweepWorker struct {
	river.WorkerDefaults[OrganizationSweepArgs]
	handler OrganizationSweepHandler
}

func (w *organizationSweepWorker) Work(ctx context.Context, job *river.Job[OrganizationSweepArgs]) error {
	if ctx == nil {
		return ErrOrganizationSweepExecution
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if w == nil || w.handler == nil {
		return ErrOrganizationSweepHandlerUnavailable
	}
	if job == nil {
		return river.JobCancel(ErrInvalidOrganizationSweepArgs)
	}
	if err := job.Args.Validate(); err != nil {
		return river.JobCancel(err)
	}
	if err := w.handler.HandleOrganizationSweep(ctx); err != nil {
		if contextErr := sanitizedOrganizationContextError(ctx, err); contextErr != nil {
			return contextErr
		}
		return ErrOrganizationSweepExecution
	}
	return nil
}

func (*organizationSweepWorker) Timeout(*river.Job[OrganizationSweepArgs]) time.Duration {
	return organizationSweepTimeout
}

func registerOrganizationSweepWorker(workers *river.Workers, handler OrganizationSweepHandler) (bool, error) {
	if handler == nil {
		return false, nil
	}
	if err := river.AddWorkerSafely(workers, &organizationSweepWorker{handler: handler}); err != nil {
		return false, errors.New("注册整理恢复任务 worker 失败")
	}
	return true, nil
}

func organizationSweepPeriodicJob() *river.PeriodicJob {
	return river.NewPeriodicJob(
		river.PeriodicInterval(organizationSweepInterval),
		organizationSweepPeriodicArgs,
		&river.PeriodicJobOpts{ID: organizationSweepPeriodicID, RunOnStart: true},
	)
}

func organizationSweepPeriodicArgs() (river.JobArgs, *river.InsertOpts) {
	args := OrganizationSweepArgs{Key: organizationSweepKey}
	opts := args.InsertOpts()
	return args, &opts
}

var _ river.JobArgsWithInsertOpts = OrganizationSweepArgs{}
var _ river.Worker[OrganizationSweepArgs] = (*organizationSweepWorker)(nil)

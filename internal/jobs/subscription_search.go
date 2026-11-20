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
	queueSubscriptionSearch       = "subscription_search"
	subscriptionSearchKind        = "subscription_precise_search_sweep"
	subscriptionSearchPeriodicID  = "subscription-precise-search-v1"
	subscriptionSearchKey         = "pending"
	subscriptionSearchInterval    = time.Minute
	subscriptionSearchJobTimeout  = 15 * time.Minute
	subscriptionSearchMaxAttempts = 5
)

var (
	ErrInvalidSubscriptionSearchArgs = errors.New("订阅精准搜索任务参数无效")
	ErrSubscriptionSearchUnavailable = errors.New("订阅精准搜索处理器未配置")
	ErrSubscriptionSearchExecution   = errors.New("执行订阅精准搜索任务失败")
)

// SubscriptionSearchSweepArgs fixes one database snapshot boundary. The
// worker never expands the snapshot while it is running.
type SubscriptionSearchSweepArgs struct {
	Key    string    `json:"key" river:"unique"`
	Cutoff time.Time `json:"cutoff"`
}

func (SubscriptionSearchSweepArgs) Kind() string { return subscriptionSearchKind }

func (SubscriptionSearchSweepArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue:       queueSubscriptionSearch,
		MaxAttempts: subscriptionSearchMaxAttempts,
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

func (a SubscriptionSearchSweepArgs) Validate(now time.Time) error {
	if strings.TrimSpace(a.Key) != subscriptionSearchKey || a.Cutoff.IsZero() {
		return ErrInvalidSubscriptionSearchArgs
	}
	if a.Cutoff.After(now.Add(time.Minute)) {
		return ErrInvalidSubscriptionSearchArgs
	}
	return nil
}

// SubscriptionSearchHandler executes a bounded search over one fixed cutoff.
type SubscriptionSearchHandler interface {
	HandleSubscriptionSearch(ctx context.Context, cutoff time.Time) error
}

type subscriptionSearchWorker struct {
	river.WorkerDefaults[SubscriptionSearchSweepArgs]
	handler SubscriptionSearchHandler
	now     func() time.Time
}

func (w *subscriptionSearchWorker) Work(ctx context.Context, job *river.Job[SubscriptionSearchSweepArgs]) error {
	if ctx == nil {
		return ErrSubscriptionSearchExecution
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if w == nil || w.handler == nil {
		return ErrSubscriptionSearchUnavailable
	}
	if job == nil {
		return river.JobCancel(ErrInvalidSubscriptionSearchArgs)
	}
	now := time.Now().UTC()
	if w.now != nil {
		now = w.now().UTC()
	}
	if err := job.Args.Validate(now); err != nil {
		return river.JobCancel(err)
	}
	if err := w.handler.HandleSubscriptionSearch(ctx, job.Args.Cutoff.UTC()); err != nil {
		if contextErr := sanitizedDownloadContextError(ctx, err); contextErr != nil {
			return contextErr
		}
		return ErrSubscriptionSearchExecution
	}
	return nil
}

func (*subscriptionSearchWorker) Timeout(*river.Job[SubscriptionSearchSweepArgs]) time.Duration {
	return subscriptionSearchJobTimeout
}

func registerSubscriptionSearchWorker(workers *river.Workers, handler SubscriptionSearchHandler) (bool, error) {
	if handler == nil {
		return false, nil
	}
	if err := river.AddWorkerSafely(workers, &subscriptionSearchWorker{handler: handler}); err != nil {
		return false, errors.New("注册订阅精准搜索 worker 失败")
	}
	return true, nil
}

func subscriptionSearchPeriodicJob() *river.PeriodicJob {
	return river.NewPeriodicJob(
		river.PeriodicInterval(subscriptionSearchInterval),
		subscriptionSearchPeriodicArgs,
		&river.PeriodicJobOpts{ID: subscriptionSearchPeriodicID, RunOnStart: true},
	)
}

func subscriptionSearchPeriodicArgs() (river.JobArgs, *river.InsertOpts) {
	args := SubscriptionSearchSweepArgs{Key: subscriptionSearchKey, Cutoff: time.Now().UTC()}
	opts := args.InsertOpts()
	return args, &opts
}

// EnqueueSubscriptionSearchSweep 立即插入一轮订阅精准搜索（插件动作与运维手工触发）。
// cutoff 取当前时间：同一 cutoff 的重复插入由 InsertOpts 的唯一约束合并，不会重复扫。
func (m *Manager) EnqueueSubscriptionSearchSweep(ctx context.Context) error {
	if ctx == nil {
		return ErrSubscriptionSearchExecution
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if m == nil || m.client == nil || !m.subscriptionSearchEnabled {
		return ErrSubscriptionSearchUnavailable
	}
	now := time.Now().UTC()
	args := SubscriptionSearchSweepArgs{Key: subscriptionSearchKey, Cutoff: now}
	if err := args.Validate(now); err != nil {
		return err
	}
	opts := args.InsertOpts()
	if _, err := m.client.Insert(ctx, args, &opts); err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return contextErr
		}
		return ErrSubscriptionSearchExecution
	}
	return nil
}

var _ river.JobArgsWithInsertOpts = SubscriptionSearchSweepArgs{}
var _ river.Worker[SubscriptionSearchSweepArgs] = (*subscriptionSearchWorker)(nil)

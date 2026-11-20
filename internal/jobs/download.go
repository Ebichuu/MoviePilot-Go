package jobs

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
)

const (
	queueDownload             = "download"
	downloadKind              = "download_intent"
	downloadDefaultMaxWorkers = 2
	downloadMaxWorkersLimit   = 4
	downloadMaxAttempts       = 5
	downloadJobTimeout        = 15 * time.Minute
	downloadDefaultSnooze     = 30 * time.Second
	downloadMaxSnooze         = time.Hour
	// DownloadSubmissionRecoveryGrace covers the maximum worker execution plus
	// a visibility margin before a missing external task may be recovered.
	DownloadSubmissionRecoveryGrace = downloadJobTimeout + 5*time.Minute
	// DownloadPlannedRecoveryGrace is a crash-recovery fallback for a planned
	// intent whose River retries could not run their final cleanup. It covers
	// every configured worker timeout plus a generous retry/backoff margin, so
	// the periodic reconciler cannot release work still owned by a live job.
	DownloadPlannedRecoveryGrace = time.Duration(downloadMaxAttempts)*downloadJobTimeout + time.Hour
	// DownloadDownloadingRecoveryGrace 是「下载中」任务在下载器里消失后的观察窗口：
	// 覆盖下载器重启/重建导致的临时不可见，超过窗口仍不可见就判定失败并释放订阅重试。
	DownloadDownloadingRecoveryGrace = 30 * time.Minute
	downloadRescueAfter              = DownloadSubmissionRecoveryGrace
)

var (
	// ErrInvalidDownloadArgs 表示任务缺少安全的用户或下载意图引用。
	ErrInvalidDownloadArgs = errors.New("下载任务参数无效")
	// ErrDownloadHandlerUnavailable 表示下载任务尚未接入业务处理器。
	ErrDownloadHandlerUnavailable = errors.New("下载任务处理器未配置")
	// ErrDownloadEnqueue 表示 River 未能安全持久化下载任务。
	ErrDownloadEnqueue = errors.New("插入下载任务失败")
	// ErrDownloadExecution 表示处理器执行失败；具体底层错误不会写入 River。
	ErrDownloadExecution = errors.New("执行下载任务失败")
	// ErrDownloadCancelled 是写入 River 的稳定业务取消原因。
	ErrDownloadCancelled = errors.New("下载任务已取消")
)

// DownloadArgs 只保存安全引用；下载地址、凭据和种子内容必须由处理器按意图读取。
type DownloadArgs struct {
	Username string    `json:"username"`
	IntentID uuid.UUID `json:"intent_id" river:"unique"`
}

// Kind 返回稳定的 River 任务类型。
func (DownloadArgs) Kind() string { return downloadKind }

// InsertOpts 固定下载队列、重试次数及按意图 UUID 的唯一性。
func (DownloadArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue:       queueDownload,
		MaxAttempts: downloadMaxAttempts,
		UniqueOpts: river.UniqueOpts{
			ByArgs: true,
		},
	}
}

// Validate 校验任务只依赖有效的用户和下载意图引用。
func (a DownloadArgs) Validate() error {
	if strings.TrimSpace(a.Username) == "" || a.IntentID == uuid.Nil {
		return ErrInvalidDownloadArgs
	}
	return nil
}

func (a DownloadArgs) normalized() DownloadArgs {
	a.Username = strings.TrimSpace(a.Username)
	return a
}

// DownloadAction 描述业务处理器希望 River 如何结束本次执行。
type DownloadAction uint8

const (
	// DownloadActionComplete 表示任务已经完成或幂等重放无需继续执行。
	DownloadActionComplete DownloadAction = iota
	// DownloadActionRetry 表示发生可重试错误并消耗一次尝试。
	DownloadActionRetry
	// DownloadActionSnooze 表示稍后继续对账且不消耗最大尝试次数。
	DownloadActionSnooze
	// DownloadActionCancel 表示业务上明确取消任务。
	DownloadActionCancel
)

// DownloadResult 是下载业务层返回给 River worker 的结构化执行决策。
type DownloadResult struct {
	Action    DownloadAction
	SnoozeFor time.Duration
}

// CompleteDownload 返回成功完成结果。
func CompleteDownload() DownloadResult {
	return DownloadResult{Action: DownloadActionComplete}
}

// RetryDownload 返回消耗一次尝试的可重试结果。
func RetryDownload() DownloadResult {
	return DownloadResult{Action: DownloadActionRetry}
}

// SnoozeDownload 返回不消耗尝试次数的延后对账结果。
func SnoozeDownload(duration time.Duration) DownloadResult {
	return DownloadResult{Action: DownloadActionSnooze, SnoozeFor: duration}
}

// CancelDownload 返回明确取消结果。
func CancelDownload() DownloadResult {
	return DownloadResult{Action: DownloadActionCancel}
}

// DownloadHandler 将安全引用交给下载业务层执行。
type DownloadHandler interface {
	HandleDownload(ctx context.Context, args DownloadArgs) (DownloadResult, error)
}

// DownloadRetryExhaustedHandler releases durable work that never advanced
// beyond planned after the final River attempt. Implementations must leave
// submitting/downloading intents untouched because their external outcome may
// be unknown.
type DownloadRetryExhaustedHandler interface {
	HandleDownloadRetryExhausted(ctx context.Context, args DownloadArgs) error
}

// ManagerConfig 保存可选的任务队列依赖和受控并发。
type ManagerConfig struct {
	DownloadHandler            DownloadHandler
	OrganizationHandler        OrganizationHandler
	OrganizationSweepHandler   OrganizationSweepHandler
	MediaIndexHandler          MediaIndexHandler
	MediaIndexSweepHandler     MediaIndexSweepHandler
	DownloadReconcileHandler   DownloadReconcileHandler
	SubscriptionSearchHandler  SubscriptionSearchHandler
	MediaServerSyncHandler     MediaServerSyncHandler
	RSSSweepHandler            RSSSweepHandler
	RSSPollHandler             RSSPollHandler
	RSSMatchHandler            RSSMatchHandler
	RSSSweepInterval           time.Duration
	ScrapeHandler              ScrapeHandler
	ScrapeSweepHandler         ScrapeSweepHandler
	NotificationHandler        NotificationHandler
	NotificationSweepHandler   NotificationSweepHandler
	SiteUserDataRefreshHandler SiteUserDataRefreshHandler
	SiteSigninHandler          SiteSigninHandler
	DownloadMaxWorkers         int
	OrganizationMaxWorkers     int
	MediaIndexMaxWorkers       int
	RSSPollMaxWorkers          int
	ScrapeMaxWorkers           int
	NotificationMaxWorkers     int
}

type downloadWorker struct {
	river.WorkerDefaults[DownloadArgs]
	handler DownloadHandler
}

func (w *downloadWorker) Work(ctx context.Context, job *river.Job[DownloadArgs]) error {
	if ctx == nil {
		return ErrDownloadExecution
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if w == nil || w.handler == nil {
		return ErrDownloadHandlerUnavailable
	}
	if job == nil {
		return ErrInvalidDownloadArgs
	}
	args := job.Args.normalized()
	if err := args.Validate(); err != nil {
		return err
	}
	result, err := w.handler.HandleDownload(ctx, args)
	if contextErr := sanitizedDownloadContextError(ctx, err); contextErr != nil {
		return contextErr
	}
	switch result.Action {
	case DownloadActionComplete:
		if err != nil {
			return ErrDownloadExecution
		}
		return nil
	case DownloadActionRetry:
		if downloadRetryExhausted(job) {
			handler, ok := w.handler.(DownloadRetryExhaustedHandler)
			if ok {
				if err := handler.HandleDownloadRetryExhausted(ctx, args); err != nil {
					return wrapDownloadExecutionError(err)
				}
				return river.JobCancel(ErrDownloadCancelled)
			}
		}
		return ErrDownloadExecution
	case DownloadActionSnooze:
		duration := result.SnoozeFor
		if duration == 0 {
			duration = downloadDefaultSnooze
		}
		if duration < 0 || duration > downloadMaxSnooze {
			return ErrDownloadExecution
		}
		return river.JobSnooze(duration)
	case DownloadActionCancel:
		return river.JobCancel(ErrDownloadCancelled)
	default:
		return ErrDownloadExecution
	}
}

func downloadRetryExhausted(job *river.Job[DownloadArgs]) bool {
	return job != nil && job.JobRow != nil && job.MaxAttempts > 0 && job.Attempt >= job.MaxAttempts
}

type downloadExecutionError struct {
	cause error
}

func (e downloadExecutionError) Error() string {
	return ErrDownloadExecution.Error()
}

func (e downloadExecutionError) Unwrap() []error {
	return []error{ErrDownloadExecution, e.cause}
}

func wrapDownloadExecutionError(cause error) error {
	if cause == nil {
		return ErrDownloadExecution
	}
	return downloadExecutionError{cause: cause}
}

func (*downloadWorker) Timeout(*river.Job[DownloadArgs]) time.Duration {
	return downloadJobTimeout
}

func registerDownloadWorker(workers *river.Workers, config ManagerConfig) (int, error) {
	if config.DownloadHandler == nil {
		if config.DownloadMaxWorkers != 0 {
			return 0, errors.New("下载队列并发配置缺少处理器")
		}
		return 0, nil
	}
	maxWorkers, err := normalizeDownloadMaxWorkers(config.DownloadMaxWorkers)
	if err != nil {
		return 0, err
	}
	if err := river.AddWorkerSafely(workers, &downloadWorker{handler: config.DownloadHandler}); err != nil {
		return 0, errors.New("注册下载任务 worker 失败")
	}
	return maxWorkers, nil
}

func normalizeDownloadMaxWorkers(requested int) (int, error) {
	if requested < 0 {
		return 0, errors.New("下载队列并发配置无效")
	}
	if requested == 0 {
		return downloadDefaultMaxWorkers, nil
	}
	return min(requested, downloadMaxWorkersLimit), nil
}

// EnqueueDownload 将安全下载意图引用作为唯一任务写入固定队列。
func (m *Manager) EnqueueDownload(ctx context.Context, args DownloadArgs) error {
	args = args.normalized()
	if err := args.Validate(); err != nil {
		return err
	}
	if ctx == nil {
		return ErrDownloadEnqueue
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if m == nil || m.client == nil || !m.downloadEnabled {
		return ErrDownloadHandlerUnavailable
	}
	if _, err := m.client.Insert(ctx, args, nil); err != nil {
		return sanitizedDownloadError(ctx, err, ErrDownloadEnqueue)
	}
	return nil
}

// EnqueueDownloadTx 在业务事务内插入下载任务，使意图、执行计划和 River Job 原子提交。
func (m *Manager) EnqueueDownloadTx(ctx context.Context, tx pgx.Tx, args DownloadArgs) error {
	args = args.normalized()
	if err := args.Validate(); err != nil {
		return err
	}
	if ctx == nil {
		return ErrDownloadEnqueue
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if tx == nil {
		return ErrDownloadEnqueue
	}
	if m == nil || m.client == nil || !m.downloadEnabled {
		return ErrDownloadHandlerUnavailable
	}
	if _, err := m.client.InsertTx(ctx, tx, args, nil); err != nil {
		return sanitizedDownloadError(ctx, err, ErrDownloadEnqueue)
	}
	return nil
}

func sanitizedDownloadContextError(ctx context.Context, err error) error {
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

func sanitizedDownloadError(ctx context.Context, err, fallback error) error {
	if contextErr := sanitizedDownloadContextError(ctx, err); contextErr != nil {
		return contextErr
	}
	return fallback
}

var _ river.JobArgsWithInsertOpts = DownloadArgs{}
var _ river.Worker[DownloadArgs] = (*downloadWorker)(nil)

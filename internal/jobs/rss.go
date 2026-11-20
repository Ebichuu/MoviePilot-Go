package jobs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

// RSS jobs deliberately use separate queues. A slow site/feed must not block
// the subscription search or download queues, and the poll queue can be
// independently capped by ManagerConfig.
const (
	queueRSSSweep = "rss_sweep"
	queueRSSPoll  = "rss_poll"
	queueRSSMatch = "rss_match"

	rssSweepKind       = "rss_incremental_sweep"
	rssPollKind        = "rss_incremental_poll"
	rssMatchKind       = "rss_global_match"
	rssSweepPeriodicID = "rss-incremental-v1"
	rssSweepKey        = "due"
	rssMatchKey        = "global"

	rssSweepInterval    = 30 * time.Minute
	rssSweepMinInterval = 5 * time.Minute
	rssSweepJobTimeout  = 15 * time.Minute
	rssPollJobTimeout   = 15 * time.Minute
	rssMatchJobTimeout  = 15 * time.Minute
	rssSweepMaxAttempts = 5
	rssPollMaxAttempts  = 5
	rssMatchMaxAttempts = 5

	// RSSPollPageSize and RSSSweepPageSize are hard bounds used by the
	// business handlers when reading due cursors and pending items. Keeping
	// them here makes the worker contract explicit and prevents accidental
	// unbounded scans during a periodic run.
	RSSSweepPageSize = 128
	RSSPollPageSize  = 100

	rssPollDefaultMaxWorkers = 4
	rssPollMaxWorkersLimit   = 8
	rssPollLeaseSnooze       = 15 * time.Second
	rssPollMaxSnooze         = 5 * time.Minute
	rssSweepPendingSnooze    = 15 * time.Second
	rssMatchBarrierSnooze    = 15 * time.Second
)

var (
	ErrInvalidRSSSweepArgs        = errors.New("RSS 增量扫描任务参数无效")
	ErrRSSSweepHandlerUnavailable = errors.New("RSS 增量扫描处理器未配置")
	ErrRSSSweepExecution          = errors.New("执行 RSS 增量扫描任务失败")
	// ErrRSSSweepPending keeps the fixed-key controller active while its poll
	// barrier or matching phase is incomplete. Later periodic triggers are then
	// skipped by River's active-state uniqueness, matching V2's non-reentry.
	ErrRSSSweepPending = errors.New("RSS 刷新轮次尚未完成")

	ErrInvalidRSSPollArgs        = errors.New("RSS 站点轮询任务参数无效")
	ErrRSSPollHandlerUnavailable = errors.New("RSS 站点轮询处理器未配置")
	ErrRSSPollExecution          = errors.New("执行 RSS 站点轮询任务失败")
	// ErrRSSPollFailureRecorded means the handler durably recorded a site-level
	// fetch or parse failure and released the poll barrier. River must complete
	// this attempt instead of retrying the same cutoff.
	ErrRSSPollFailureRecorded = errors.New("RSS 站点轮询失败已记录")

	ErrInvalidRSSMatchArgs        = errors.New("RSS 全局匹配任务参数无效")
	ErrRSSMatchHandlerUnavailable = errors.New("RSS 全局匹配处理器未配置")
	ErrRSSMatchExecution          = errors.New("执行 RSS 全局匹配任务失败")
	// ErrRSSMatchBarrierPending means at least one expected site has not
	// completed its poll attempt for the fixed cutoff. It is normal control
	// flow and asks River to snooze without consuming a retry attempt.
	ErrRSSMatchBarrierPending = errors.New("RSS 全局匹配屏障尚未完成")
	// ErrRSSLeaseBusy tells the worker that another process owns the site
	// lease. It is intentionally exported so database/subscriptionflow can
	// preserve this condition without exposing driver errors to River.
	ErrRSSLeaseBusy = errors.New("RSS 站点租约繁忙")
	// ErrRSSPollLeaseBusy is kept as a descriptive alias for callers that name
	// the condition after the poll operation.
	ErrRSSPollLeaseBusy = ErrRSSLeaseBusy
	// Conflict aliases make it explicit that this is a normal contention path,
	// not a failed poll deserving a retry attempt.
	ErrRSSLeaseConflict     = ErrRSSLeaseBusy
	ErrRSSPollLeaseConflict = ErrRSSLeaseBusy
)

// RSSSweepArgs identifies one bounded snapshot of all due RSS cursors. Only
// Key is unique; Cutoff is excluded from River's unique argument hash via its
// struct tag so each interval can enqueue a fresh snapshot after the previous
// active job finishes.
type RSSSweepArgs struct {
	Key    string    `json:"key" river:"unique"`
	Cutoff time.Time `json:"cutoff"`
}

// RSSSweepState is the durable controller state for one sweep cutoff. An
// uninitialized zero value is passed only when the job has no recorded output;
// an initialized empty SiteIDs slice represents a real round with no sites.
type RSSSweepState struct {
	Initialized bool  `json:"initialized"`
	SiteIDs     []int `json:"site_ids"`
}

func (RSSSweepArgs) Kind() string { return rssSweepKind }

func (RSSSweepArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue:       queueRSSSweep,
		MaxAttempts: rssSweepMaxAttempts,
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

func (a RSSSweepArgs) Validate(now time.Time) error {
	if strings.TrimSpace(a.Key) != rssSweepKey || a.Cutoff.IsZero() {
		return ErrInvalidRSSSweepArgs
	}
	if a.Cutoff.After(now.Add(time.Minute)) {
		return ErrInvalidRSSSweepArgs
	}
	return nil
}

// RSSPollArgs identifies one site poll. SiteID is the only unique field, so
// a scheduler retry with a different cutoff cannot create duplicate polls for
// the same site while an earlier poll is active.
type RSSPollArgs struct {
	SiteID int       `json:"site_id" river:"unique"`
	Cutoff time.Time `json:"cutoff"`
}

func (RSSPollArgs) Kind() string { return rssPollKind }

func (RSSPollArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue:       queueRSSPoll,
		MaxAttempts: rssPollMaxAttempts,
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

func (a RSSPollArgs) Validate(now time.Time) error {
	if a.SiteID <= 0 || a.Cutoff.IsZero() {
		return ErrInvalidRSSPollArgs
	}
	if a.Cutoff.After(now.Add(time.Minute)) {
		return ErrInvalidRSSPollArgs
	}
	return nil
}

// RSSMatchArgs identifies one bounded, deterministic global match snapshot.
// Key and Cutoff form the round identity, so a waiting match cannot suppress a
// later cutoff. The single-worker queue still prevents concurrent matching.
// ExpectedSiteIDs is a strictly increasing barrier set produced by the sweep.
type RSSMatchArgs struct {
	Key             string    `json:"key" river:"unique"`
	Cutoff          time.Time `json:"cutoff" river:"unique"`
	ExpectedSiteIDs []int     `json:"expected_site_ids"`
}

func (RSSMatchArgs) Kind() string { return rssMatchKind }

func (RSSMatchArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue:       queueRSSMatch,
		MaxAttempts: rssMatchMaxAttempts,
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

func (a RSSMatchArgs) Validate(now time.Time) error {
	if a.Key != rssMatchKey || a.Cutoff.IsZero() {
		return ErrInvalidRSSMatchArgs
	}
	if a.Cutoff.After(now.Add(time.Minute)) || len(a.ExpectedSiteIDs) > RSSSweepPageSize {
		return ErrInvalidRSSMatchArgs
	}
	for index, siteID := range a.ExpectedSiteIDs {
		if siteID <= 0 || (index > 0 && a.ExpectedSiteIDs[index-1] >= siteID) {
			return ErrInvalidRSSMatchArgs
		}
	}
	return nil
}

// RSSSweepHandler processes one fixed cutoff and returns the state to persist
// before the worker completes or snoozes. On the first attempt state is the
// zero value. Every returned state for a successful or pending attempt must be
// initialized and contain at most RSSSweepPageSize strictly increasing site
// IDs.
type RSSSweepHandler interface {
	HandleRSSSweep(ctx context.Context, cutoff time.Time, state RSSSweepState) (RSSSweepState, error)
}

// RSSPollHandler owns the HTTP fetch, cursor lease, and bounded persistence
// for one site. ErrRSSLeaseBusy asks River to snooze without consuming a retry;
// ErrRSSPollFailureRecorded completes a site failure already committed by the
// handler. Other errors remain retryable.
type RSSPollHandler interface {
	HandleRSSPoll(ctx context.Context, args RSSPollArgs) error
}

// RSSMatchHandler waits for the expected poll barrier and performs one
// bounded global RSS cache match for the fixed cutoff.
type RSSMatchHandler interface {
	HandleRSSMatch(ctx context.Context, args RSSMatchArgs) error
}

type rssSweepWorker struct {
	river.WorkerDefaults[RSSSweepArgs]
	handler      RSSSweepHandler
	now          func() time.Time
	recordOutput func(context.Context, RSSSweepState) error
}

func (w *rssSweepWorker) Work(ctx context.Context, job *river.Job[RSSSweepArgs]) error {
	if ctx == nil {
		return ErrRSSSweepExecution
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if w == nil || w.handler == nil {
		return ErrRSSSweepHandlerUnavailable
	}
	if job == nil {
		return river.JobCancel(ErrInvalidRSSSweepArgs)
	}
	now := time.Now().UTC()
	if w.now != nil {
		now = w.now().UTC()
	}
	if err := job.Args.Validate(now); err != nil {
		return river.JobCancel(err)
	}
	state, err := restoreRSSSweepState(job)
	if err != nil {
		return river.JobCancel(ErrInvalidRSSSweepArgs)
	}

	nextState, handlerErr := w.handler.HandleRSSSweep(ctx, job.Args.Cutoff.UTC(), state)
	if nextState.Initialized {
		if err := validateInitializedRSSSweepState(nextState); err != nil {
			return river.JobCancel(ErrInvalidRSSSweepArgs)
		}
		recordOutput := w.recordOutput
		if recordOutput == nil {
			recordOutput = func(ctx context.Context, state RSSSweepState) error {
				return river.RecordOutput(ctx, state)
			}
		}
		if err := recordOutput(ctx, nextState); err != nil {
			if contextErr := sanitizedRSSContextError(ctx, err); contextErr != nil {
				return contextErr
			}
			return ErrRSSSweepExecution
		}
	} else if handlerErr == nil || errors.Is(handlerErr, ErrRSSSweepPending) {
		return river.JobCancel(ErrInvalidRSSSweepArgs)
	}
	if contextErr := sanitizedRSSContextError(ctx, handlerErr); contextErr != nil {
		return contextErr
	}
	if errors.Is(handlerErr, ErrRSSSweepPending) {
		return river.JobSnooze(rssSweepPendingSnooze)
	}
	if handlerErr != nil {
		return ErrRSSSweepExecution
	}
	return nil
}

func restoreRSSSweepState(job *river.Job[RSSSweepArgs]) (RSSSweepState, error) {
	if job.JobRow == nil {
		return RSSSweepState{}, nil
	}
	output := job.Output()
	if len(output) == 0 {
		return RSSSweepState{}, nil
	}

	var persisted struct {
		Initialized *bool  `json:"initialized"`
		SiteIDs     *[]int `json:"site_ids"`
	}
	decoder := json.NewDecoder(bytes.NewReader(output))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&persisted); err != nil {
		return RSSSweepState{}, ErrInvalidRSSSweepArgs
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return RSSSweepState{}, ErrInvalidRSSSweepArgs
	}
	if persisted.Initialized == nil || !*persisted.Initialized || persisted.SiteIDs == nil {
		return RSSSweepState{}, ErrInvalidRSSSweepArgs
	}
	state := RSSSweepState{Initialized: true, SiteIDs: *persisted.SiteIDs}
	if err := validateInitializedRSSSweepState(state); err != nil {
		return RSSSweepState{}, err
	}
	return state, nil
}

func validateInitializedRSSSweepState(state RSSSweepState) error {
	if !state.Initialized || state.SiteIDs == nil || len(state.SiteIDs) > RSSSweepPageSize {
		return ErrInvalidRSSSweepArgs
	}
	for index, siteID := range state.SiteIDs {
		if siteID <= 0 || (index > 0 && state.SiteIDs[index-1] >= siteID) {
			return ErrInvalidRSSSweepArgs
		}
	}
	return nil
}

func (*rssSweepWorker) Timeout(*river.Job[RSSSweepArgs]) time.Duration { return rssSweepJobTimeout }

func registerRSSSweepWorker(workers *river.Workers, handler RSSSweepHandler) (bool, error) {
	if handler == nil {
		return false, nil
	}
	if err := river.AddWorkerSafely(workers, &rssSweepWorker{handler: handler}); err != nil {
		return false, errors.New("注册 RSS 增量扫描 worker 失败")
	}
	return true, nil
}

func rssSweepPeriodicJob(configuredInterval time.Duration) *river.PeriodicJob {
	return river.NewPeriodicJob(
		river.PeriodicInterval(normalizeRSSSweepInterval(configuredInterval)),
		rssSweepPeriodicArgs,
		rssSweepPeriodicJobOptions(),
	)
}

func rssSweepRuntimePeriodicJob(configuredInterval time.Duration) *river.PeriodicJob {
	return river.NewPeriodicJob(
		river.PeriodicInterval(normalizeRSSSweepInterval(configuredInterval)),
		rssSweepPeriodicArgs,
		rssSweepRuntimePeriodicJobOptions(),
	)
}

func normalizeRSSSweepInterval(configured time.Duration) time.Duration {
	if configured < 0 {
		return rssSweepInterval
	}
	if configured == 0 {
		return rssSweepInterval
	}
	if configured < rssSweepMinInterval {
		return rssSweepMinInterval
	}
	return configured
}

func rssSweepPeriodicJobOptions() *river.PeriodicJobOpts {
	return &river.PeriodicJobOpts{ID: rssSweepPeriodicID}
}

func rssSweepRuntimePeriodicJobOptions() *river.PeriodicJobOpts {
	return &river.PeriodicJobOpts{}
}

func rssSweepPeriodicArgs() (river.JobArgs, *river.InsertOpts) {
	args := RSSSweepArgs{Key: rssSweepKey, Cutoff: time.Now().UTC()}
	opts := args.InsertOpts()
	return args, &opts
}

type rssPollWorker struct {
	river.WorkerDefaults[RSSPollArgs]
	handler RSSPollHandler
	now     func() time.Time
}

func (w *rssPollWorker) Work(ctx context.Context, job *river.Job[RSSPollArgs]) error {
	if ctx == nil {
		return ErrRSSPollExecution
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if w == nil || w.handler == nil {
		return ErrRSSPollHandlerUnavailable
	}
	if job == nil {
		return river.JobCancel(ErrInvalidRSSPollArgs)
	}
	now := time.Now().UTC()
	if w.now != nil {
		now = w.now().UTC()
	}
	if err := job.Args.Validate(now); err != nil {
		return river.JobCancel(err)
	}
	args := job.Args
	args.Cutoff = args.Cutoff.UTC()
	err := w.handler.HandleRSSPoll(ctx, args)
	if contextErr := sanitizedRSSContextError(ctx, err); contextErr != nil {
		return contextErr
	}
	if errors.Is(err, ErrRSSLeaseBusy) || errors.Is(err, ErrRSSPollLeaseBusy) {
		return river.JobSnooze(rssPollLeaseSnooze)
	}
	if errors.Is(err, ErrRSSPollFailureRecorded) {
		return nil
	}
	if err != nil {
		return ErrRSSPollExecution
	}
	return nil
}

func (*rssPollWorker) Timeout(*river.Job[RSSPollArgs]) time.Duration { return rssPollJobTimeout }

func registerRSSPollWorker(workers *river.Workers, handler RSSPollHandler) (bool, error) {
	if handler == nil {
		return false, nil
	}
	if err := river.AddWorkerSafely(workers, &rssPollWorker{handler: handler}); err != nil {
		return false, errors.New("注册 RSS 站点轮询 worker 失败")
	}
	return true, nil
}

type rssMatchWorker struct {
	river.WorkerDefaults[RSSMatchArgs]
	handler RSSMatchHandler
	now     func() time.Time
}

func (w *rssMatchWorker) Work(ctx context.Context, job *river.Job[RSSMatchArgs]) error {
	if ctx == nil {
		return ErrRSSMatchExecution
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if w == nil || w.handler == nil {
		return ErrRSSMatchHandlerUnavailable
	}
	if job == nil {
		return river.JobCancel(ErrInvalidRSSMatchArgs)
	}
	now := time.Now().UTC()
	if w.now != nil {
		now = w.now().UTC()
	}
	if err := job.Args.Validate(now); err != nil {
		return river.JobCancel(err)
	}
	args := job.Args
	args.Cutoff = args.Cutoff.UTC()
	err := w.handler.HandleRSSMatch(ctx, args)
	if contextErr := sanitizedRSSContextError(ctx, err); contextErr != nil {
		return contextErr
	}
	if errors.Is(err, ErrRSSMatchBarrierPending) {
		return river.JobSnooze(rssMatchBarrierSnooze)
	}
	if err != nil {
		return ErrRSSMatchExecution
	}
	return nil
}

func (*rssMatchWorker) Timeout(*river.Job[RSSMatchArgs]) time.Duration { return rssMatchJobTimeout }

func registerRSSMatchWorker(workers *river.Workers, handler RSSMatchHandler) (bool, error) {
	if handler == nil {
		return false, nil
	}
	if err := river.AddWorkerSafely(workers, &rssMatchWorker{handler: handler}); err != nil {
		return false, errors.New("注册 RSS 全局匹配 worker 失败")
	}
	return true, nil
}

func normalizeRSSPollMaxWorkers(requested int) (int, error) {
	if requested < 0 {
		return 0, errors.New("RSS 轮询队列并发配置无效")
	}
	if requested == 0 {
		return rssPollDefaultMaxWorkers, nil
	}
	return min(requested, rssPollMaxWorkersLimit), nil
}

func sanitizedRSSContextError(ctx context.Context, err error) error {
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

var _ river.JobArgsWithInsertOpts = RSSSweepArgs{}
var _ river.JobArgsWithInsertOpts = RSSPollArgs{}
var _ river.JobArgsWithInsertOpts = RSSMatchArgs{}
var _ river.Worker[RSSSweepArgs] = (*rssSweepWorker)(nil)
var _ river.Worker[RSSPollArgs] = (*rssPollWorker)(nil)
var _ river.Worker[RSSMatchArgs] = (*rssMatchWorker)(nil)

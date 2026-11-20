package downloadflow

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/moviepilot-go/moviepilot-go/internal/database"
	"github.com/moviepilot-go/moviepilot-go/internal/downloader"
	"github.com/moviepilot-go/moviepilot-go/internal/events"
	"github.com/moviepilot-go/moviepilot-go/internal/jobs"
	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

const (
	downloadReconciliationPageSize = 200
	downloadObservationBatchSize   = 50
	staleSubmissionGrace           = jobs.DownloadSubmissionRecoveryGrace
	stalePlannedGrace              = jobs.DownloadPlannedRecoveryGrace
	downloadMissingGrace           = jobs.DownloadDownloadingRecoveryGrace
)

var (
	// ErrDownloadReconciliationUnavailable is the stable sweep persistence error.
	ErrDownloadReconciliationUnavailable = errors.New("下载对账状态当前不可用")
	// ErrInvalidDownloadReconciliationPage indicates an invalid or non-progressing store page.
	ErrInvalidDownloadReconciliationPage = errors.New("下载对账分页结果无效")
)

// 对账阶段标识：只用于日志与诊断报告，不含下载器地址、凭据或 SQL 文本。
const (
	reconcileStageOrphanReclaim    = "orphan_reclaim"
	reconcileStageScan             = "scan"
	reconcileStageRecoverIntent    = "recover_intent"
	reconcileStageSyncCompleted    = "sync_completed"
	reconcileStageEpisodeSelection = "episode_selection"
	reconcileStageEpisodeSync      = "episode_sync"
	reconcileStageSubscriptionSync = "subscription_sync"
	reconcileStageAcceptance       = "acceptance"
	reconcileStageStoreWrite       = "store_write"
	reconcileStageObservation      = "observation"
	reconcileStageCompletionHook   = "completion_hook"
)

// ReconciliationTargetFailure 描述一轮对账里单个未能收敛的目标。
// Stage 是稳定阶段标识；Err 是可外露的稳定错误；Cause 是底层原因，
// 只写本地日志，避免把下载器地址或凭据带进 River 的任务错误。
type ReconciliationTargetFailure struct {
	IntentID     uuid.UUID
	DownloaderID string
	TorrentHash  string
	State        downloader.IntentState
	Stage        string
	Err          error
	Cause        error
}

// ReconciliationSweepReport 是一轮对账的结构化摘要，用于日志与自检端点。
type ReconciliationSweepReport struct {
	Cutoff     time.Time
	StartedAt  time.Time
	FinishedAt time.Time
	Scanned    int
	Succeeded  int
	Orphans    int64
	Failures   []ReconciliationTargetFailure
}

// OK reports whether every scanned target converged in this sweep.
func (r ReconciliationSweepReport) OK() bool { return len(r.Failures) == 0 }

// reconciliationStageError 给底层错误附加稳定的阶段标识，并保留原始原因。
type reconciliationStageError struct {
	stage string
	err   error
	cause error
}

func (e *reconciliationStageError) Error() string { return e.stage + ": " + e.err.Error() }

func (e *reconciliationStageError) Unwrap() error { return e.err }

func (e *reconciliationStageError) Cause() error {
	if e.cause != nil {
		return e.cause
	}
	return e.err
}

// stageSanitized 把底层原因折叠成「阶段 + 稳定错误」；底层原因只留作日志。
func stageSanitized(ctx context.Context, stage string, err error) error {
	if err == nil {
		return nil
	}
	return &reconciliationStageError{
		stage: stage,
		err:   reconciliationContextOrStable(ctx, err),
		cause: err,
	}
}

// stageStable 给已经是稳定错误的失败附加阶段标识。
func stageStable(stage string, err error) error {
	if err == nil {
		return nil
	}
	return &reconciliationStageError{stage: stage, err: err, cause: err}
}

func failureStage(err error) string {
	var staged *reconciliationStageError
	if errors.As(err, &staged) && staged.stage != "" {
		return staged.stage
	}
	return reconcileStageObservation
}

func failureCause(err error) error {
	var staged *reconciliationStageError
	if errors.As(err, &staged) {
		return staged.Cause()
	}
	return err
}

// recordFailure 把单个目标的失败附上目标身份记进本轮报告。
func (r *ReconciliationSweepReport) recordFailure(target downloader.ReconciliationTarget, err error) {
	if r == nil || err == nil {
		return
	}
	r.Failures = append(r.Failures, ReconciliationTargetFailure{
		IntentID:     target.ID,
		DownloaderID: target.DownloaderID,
		TorrentHash:  target.TorrentHash,
		State:        target.State,
		Stage:        failureStage(err),
		Err:          err,
		Cause:        failureCause(err),
	})
}

// publishSweepReport 发布本轮摘要并记录结构化日志：一条轮次摘要，
// 每个失败目标一条带底层原因的告警，便于直接定位。
func (h *ReconciliationHandler) publishSweepReport(report *ReconciliationSweepReport) {
	if h == nil || report == nil {
		return
	}
	stored := *report
	stored.Failures = append([]ReconciliationTargetFailure(nil), report.Failures...)
	h.lastSweepReport.Store(&stored)

	logger := h.loggerOrDefault()
	attrs := []any{
		slog.String("cutoff", report.Cutoff.Format(time.RFC3339)),
		slog.Int("scanned", report.Scanned),
		slog.Int("succeeded", report.Succeeded),
		slog.Int("failed", len(report.Failures)),
		slog.Int64("orphans", report.Orphans),
		slog.Duration("elapsed", report.FinishedAt.Sub(report.StartedAt)),
	}
	if report.OK() {
		logger.Info("下载对账完成", attrs...)
		return
	}
	logger.Warn("下载对账完成，存在未收敛目标", attrs...)
	for _, failure := range report.Failures {
		causeText := ""
		if failure.Cause != nil {
			causeText = failure.Cause.Error()
		}
		logger.Warn("下载对账目标失败",
			slog.String("intent_id", failure.IntentID.String()),
			slog.String("downloader_id", failure.DownloaderID),
			slog.String("torrent_hash", failure.TorrentHash),
			slog.String("state", string(failure.State)),
			slog.String("stage", failure.Stage),
			slog.String("reason", failure.Err.Error()),
			slog.String("cause", causeText))
	}
}

// ReconciliationStore is the secret-free storage boundary used by a sweep.
type ReconciliationStore interface {
	ListDownloadReconciliationTargets(
		ctx context.Context,
		cutoff time.Time,
		afterUpdatedAt time.Time,
		afterID uuid.UUID,
		limit int,
	) ([]downloader.ReconciliationTarget, error)
	CompareAndSwapDownloadIntent(
		ctx context.Context,
		username string,
		id uuid.UUID,
		fromStates []downloader.IntentState,
		next downloader.IntentState,
		torrentHash string,
	) (downloader.Intent, error)
	TransitionSubscriptionEpisodesByIntent(
		ctx context.Context,
		username string,
		intentID uuid.UUID,
		fromStates []subscription.SubscriptionEpisodeState,
		next subscription.SubscriptionEpisodeState,
	) (int64, error)
	CompareAndSwapSubscriptionStateByIntent(
		ctx context.Context,
		username string,
		intentID uuid.UUID,
		current subscription.State,
		next subscription.State,
	) error
	RecoverSubscriptionDownloadIntent(
		ctx context.Context,
		username string,
		intentID uuid.UUID,
		fromStates []downloader.IntentState,
		expectedUpdatedAt *time.Time,
	) error
}

// DownloadCompletionHook turns a confirmed completed intent into the next
// durable workflow stage. It is optional so deployments can enable download
// reconciliation before organization support is configured.
type DownloadCompletionHook interface {
	HandleDownloadCompleted(context.Context, downloader.ReconciliationTarget) error
}

// orphanIntentReclaimer 由存储层可选实现，用于收尾缺少提交计划的遗留意图。
type orphanIntentReclaimer interface {
	FailOrphanDownloadIntents(context.Context, time.Time) (int64, error)
}

// ReconciliationHandler performs a fixed-cutoff sweep with bounded pages and
// bounded qBittorrent hash batches.
type ReconciliationHandler struct {
	store          ReconciliationStore
	submitters     SubmitterProvider
	completionHook DownloadCompletionHook
	pageSize       int
	observeBatch   int
	// eventBus 是旁路事件总线；为空表示不广播完成事件。
	eventBus *events.Bus
	// logger 记录逐目标失败原因，默认 slog.Default()。
	logger *slog.Logger
	// lastSweepReport 保存最近一轮对账摘要，供自检端点读取。
	lastSweepReport atomic.Pointer[ReconciliationSweepReport]
}

// SetLogger 注入结构化日志器，用于记录对账失败目标的底层原因。
func (h *ReconciliationHandler) SetLogger(logger *slog.Logger) {
	if h == nil || logger == nil {
		return
	}
	h.logger = logger
}

// LastSweepReport 返回最近一轮对账摘要；尚未跑过对账时返回零值报告。
func (h *ReconciliationHandler) LastSweepReport() ReconciliationSweepReport {
	if h == nil {
		return ReconciliationSweepReport{}
	}
	report := h.lastSweepReport.Load()
	if report == nil {
		return ReconciliationSweepReport{}
	}
	clone := *report
	clone.Failures = append([]ReconciliationTargetFailure(nil), report.Failures...)
	return clone
}

func (h *ReconciliationHandler) loggerOrDefault() *slog.Logger {
	if h != nil && h.logger != nil {
		return h.logger
	}
	return slog.Default()
}

// SetEventBus 注入域事件总线，用于广播对账路径发现的 download.completed。
func (h *ReconciliationHandler) SetEventBus(bus *events.Bus) {
	if h == nil {
		return
	}
	h.eventBus = bus
}

func (h *ReconciliationHandler) publishDownloadCompleted(target downloader.ReconciliationTarget) {
	if h == nil || h.eventBus == nil {
		return
	}
	if !markDownloadCompletionPublished(target.ID) {
		return
	}
	h.eventBus.Publish(events.Event{
		Type:     events.TypeDownloadCompleted,
		Username: target.Username,
		Payload: map[string]any{
			"intent_id":     target.ID.String(),
			"torrent_hash":  target.TorrentHash,
			"downloader_id": target.DownloaderID,
			"source":        "reconcile",
		},
	})
}

// NewReconciliationHandler creates a periodic download status reconciler.
func NewReconciliationHandler(
	store ReconciliationStore,
	submitters SubmitterProvider,
	completionHooks ...DownloadCompletionHook,
) (*ReconciliationHandler, error) {
	if store == nil || submitters == nil || len(completionHooks) > 1 ||
		(len(completionHooks) == 1 && completionHooks[0] == nil) {
		return nil, errors.New("下载对账处理器依赖不完整")
	}
	var completionHook DownloadCompletionHook
	if len(completionHooks) == 1 {
		completionHook = completionHooks[0]
	}
	return &ReconciliationHandler{
		store: store, submitters: submitters, completionHook: completionHook,
		pageSize: downloadReconciliationPageSize, observeBatch: downloadObservationBatchSize,
	}, nil
}

// HandleDownloadReconciliation scans bounded active and recoverable intents.
//
// 失败策略——保证单个坏目标不会拖垮整轮：
//   - 扫描、分页或分页校验失败：返回错误，让 River 重试同一快照；
//   - 单个目标失败：记进本轮报告并打日志（带底层原因），继续处理其它目标，
//     下一轮对账自然会再试它；
//   - 上下文取消：立即返回。
func (h *ReconciliationHandler) HandleDownloadReconciliation(ctx context.Context, cutoff time.Time) error {
	if ctx == nil {
		return ErrDownloadReconciliationUnavailable
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if h == nil || h.store == nil || h.submitters == nil || cutoff.IsZero() ||
		h.pageSize < 1 || h.pageSize > downloadReconciliationPageSize ||
		h.observeBatch < 1 || h.observeBatch > downloadObservationBatchSize {
		return ErrDownloadReconciliationUnavailable
	}

	report := ReconciliationSweepReport{Cutoff: cutoff.UTC(), StartedAt: time.Now().UTC()}
	err := h.sweep(ctx, cutoff.UTC(), &report)
	report.FinishedAt = time.Now().UTC()
	h.publishSweepReport(&report)
	return err
}

// sweep 执行一次固定截止时间的分页扫描。只有扫描本身不可用才返回错误。
func (h *ReconciliationHandler) sweep(
	ctx context.Context,
	cutoff time.Time,
	report *ReconciliationSweepReport,
) error {
	// 没有提交计划的遗留意图无法确定下载器，对账会永远跳过；
	// 超过宽限期直接判失败，避免永久停留在 submitting/downloading。
	if reclaimer, ok := h.store.(orphanIntentReclaimer); ok {
		orphans, err := reclaimer.FailOrphanDownloadIntents(ctx, cutoff.Add(-downloadMissingGrace))
		if err != nil {
			if contextErr := contextError(ctx, err); contextErr != nil {
				return contextErr
			}
			// 遗留意图收尾失败不阻断整轮：记录原因，下一轮继续尝试。
			h.loggerOrDefault().Warn("下载对账遗留意图收尾失败",
				slog.String("stage", reconcileStageOrphanReclaim),
				slog.String("error", err.Error()))
		}
		report.Orphans = orphans
	}

	var (
		afterUpdatedAt time.Time
		afterID        uuid.UUID
	)
	for {
		targets, err := h.store.ListDownloadReconciliationTargets(
			ctx, cutoff, afterUpdatedAt, afterID, h.pageSize,
		)
		if err != nil {
			if contextErr := contextError(ctx, err); contextErr != nil {
				return contextErr
			}
			// 扫描本身不可用是整轮级故障：River 需要重试同一快照。
			// 底层原因只写本地日志，返回给 River 的仍是稳定错误。
			h.loggerOrDefault().Error("下载对账扫描失败",
				slog.String("stage", reconcileStageScan),
				slog.String("cutoff", cutoff.Format(time.RFC3339)),
				slog.Int("scanned", report.Scanned),
				slog.String("error", err.Error()))
			return ErrDownloadReconciliationUnavailable
		}
		if len(targets) == 0 {
			return nil
		}
		if len(targets) > h.pageSize {
			h.loggerOrDefault().Error("下载对账分页结果超出上限",
				slog.String("stage", reconcileStageScan),
				slog.Int("page_size", h.pageSize),
				slog.Int("targets", len(targets)))
			return ErrInvalidDownloadReconciliationPage
		}
		if err := validateReconciliationPage(targets, cutoff, afterUpdatedAt, afterID); err != nil {
			h.loggerOrDefault().Error("下载对账分页结果无效",
				slog.String("stage", reconcileStageScan),
				slog.Int("scanned", report.Scanned),
				slog.Int("targets", len(targets)),
				slog.String("error", err.Error()))
			return err
		}
		report.Scanned += len(targets)
		if err := h.reconcilePage(ctx, cutoff, targets, report); err != nil {
			return err
		}
		last := targets[len(targets)-1]
		afterUpdatedAt, afterID = last.UpdatedAt.UTC(), last.ID
		if len(targets) < h.pageSize {
			return nil
		}
	}
}

func (h *ReconciliationHandler) reconcilePage(
	ctx context.Context,
	cutoff time.Time,
	targets []downloader.ReconciliationTarget,
	report *ReconciliationSweepReport,
) error {
	grouped := make(map[string]map[string][]downloader.ReconciliationTarget)
	for _, target := range targets {
		if err := ctx.Err(); err != nil {
			return err
		}
		if target.State == downloader.IntentPlanned {
			if target.UpdatedAt.Add(stalePlannedGrace).After(cutoff) {
				continue
			}
			if err := h.recoverReconciledIntent(ctx, target); err != nil {
				if contextErr := contextError(ctx, err); contextErr != nil {
					return contextErr
				}
				report.recordFailure(target, err)
				continue
			}
			report.Succeeded++
			continue
		}
		if target.State == downloader.IntentFailed {
			if err := h.recoverReconciledIntent(ctx, target); err != nil {
				if contextErr := contextError(ctx, err); contextErr != nil {
					return contextErr
				}
				report.recordFailure(target, err)
				continue
			}
			report.Succeeded++
			continue
		}
		if target.State == downloader.IntentCompleted {
			if err := h.syncReconciledCompleted(ctx, target); err != nil {
				if contextErr := contextError(ctx, err); contextErr != nil {
					return contextErr
				}
				report.recordFailure(target, err)
				continue
			}
			report.Succeeded++
			continue
		}
		byHash := grouped[target.DownloaderID]
		if byHash == nil {
			byHash = make(map[string][]downloader.ReconciliationTarget)
			grouped[target.DownloaderID] = byHash
		}
		byHash[target.TorrentHash] = append(byHash[target.TorrentHash], target)
	}

	downloaderIDs := make([]string, 0, len(grouped))
	for downloaderID := range grouped {
		downloaderIDs = append(downloaderIDs, downloaderID)
	}
	sort.Strings(downloaderIDs)
	for _, downloaderID := range downloaderIDs {
		if err := ctx.Err(); err != nil {
			return err
		}
		submitter, err := h.submitters.ResolveSubmitter(downloaderID)
		if err != nil {
			// 下载器缺失或暂不可用：跳过本组，下一轮再试，不拖垮整轮。
			h.loggerOrDefault().Warn("下载对账跳过不可用下载器",
				slog.String("stage", reconcileStageObservation),
				slog.String("downloader_id", downloaderID),
				slog.String("error", err.Error()))
			continue
		}
		byHash := grouped[downloaderID]
		hashes := make([]string, 0, len(byHash))
		for hash := range byHash {
			hashes = append(hashes, hash)
		}
		sort.Strings(hashes)
		for start := 0; start < len(hashes); start += h.observeBatch {
			end := min(start+h.observeBatch, len(hashes))
			batch := hashes[start:end]
			observations, err := submitter.ObserveTasks(ctx, batch)
			if err != nil {
				if contextErr := contextError(ctx, err); contextErr != nil {
					return contextErr
				}
				h.loggerOrDefault().Warn("下载对账查询下载器失败",
					slog.String("stage", reconcileStageObservation),
					slog.String("downloader_id", downloaderID),
					slog.Int("tasks", len(batch)),
					slog.String("error", err.Error()))
				continue
			}
			for _, hash := range batch {
				observation, ok := observations[hash]
				if !ok || observation.Hash != hash {
					continue
				}
				for _, target := range byHash[hash] {
					if err := ctx.Err(); err != nil {
						return err
					}
					if err := h.applyObservation(ctx, cutoff, submitter, target, observation); err != nil {
						if contextErr := contextError(ctx, err); contextErr != nil {
							return contextErr
						}
						report.recordFailure(target, err)
						continue
					}
					report.Succeeded++
				}
			}
		}
	}
	return ctx.Err()
}

func (h *ReconciliationHandler) applyObservation(
	ctx context.Context,
	cutoff time.Time,
	submitter TorrentSubmitter,
	target downloader.ReconciliationTarget,
	observation downloader.TaskObservation,
) error {
	switch target.State {
	case downloader.IntentSubmitting:
		if observation.State == downloader.TaskObservationMissing {
			if target.UpdatedAt.Add(staleSubmissionGrace).After(cutoff) {
				return nil
			}
			return h.recoverReconciledIntent(ctx, target)
		}
		if observation.State != downloader.TaskObservationActive &&
			observation.State != downloader.TaskObservationPaused &&
			observation.State != downloader.TaskObservationCompleted {
			return nil
		}
		if err := h.ensureReconciledEpisodeSelection(ctx, submitter, target); err != nil {
			return err
		}
		supported, accepted, err := h.acceptReconciledDownload(ctx, target)
		if err != nil {
			return stageSanitized(ctx, reconcileStageAcceptance, err)
		}
		if supported {
			if !accepted {
				return nil
			}
		} else {
			_, err = h.store.CompareAndSwapDownloadIntent(
				ctx,
				target.Username,
				target.ID,
				[]downloader.IntentState{downloader.IntentSubmitting},
				downloader.IntentDownloading,
				target.TorrentHash,
			)
			if reconciliationConflict(err) {
				return nil
			}
			if err != nil {
				return stageSanitized(ctx, reconcileStageStoreWrite, err)
			}
			if err := h.syncReconciledDownloading(ctx, target); err != nil {
				return err
			}
		}
		if observation.State != downloader.TaskObservationCompleted {
			return nil
		}
		target.State = downloader.IntentDownloading
		return h.completeReconciledDownload(ctx, target)
	case downloader.IntentDownloading:
		switch observation.State {
		case downloader.TaskObservationActive,
			downloader.TaskObservationPaused,
			downloader.TaskObservationCompleted:
			if err := h.ensureReconciledEpisodeSelection(ctx, submitter, target); err != nil {
				return err
			}
		case downloader.TaskObservationMissing:
			// Preserve legacy acceptance compensation after the task disappeared.
			// 超过宽限期仍然看不到任务，说明它已被删除或替换：
			// 判为失败并释放订阅重试，否则意图会永久停在 downloading。
			if !target.UpdatedAt.Add(downloadMissingGrace).After(cutoff) {
				return h.recoverReconciledIntent(ctx, target)
			}
		default:
			return nil
		}
		supported, accepted, err := h.acceptReconciledDownload(ctx, target)
		if err != nil {
			return stageSanitized(ctx, reconcileStageAcceptance, err)
		}
		if supported && !accepted {
			return nil
		}
		if observation.State != downloader.TaskObservationCompleted {
			return nil
		}
		return h.completeReconciledDownload(ctx, target)
	}
	return nil
}

func (h *ReconciliationHandler) ensureReconciledEpisodeSelection(
	ctx context.Context,
	submitter TorrentSubmitter,
	target downloader.ReconciliationTarget,
) error {
	episodes, err := h.loadReconciledTargetEpisodes(ctx, target.Username, target.ID)
	if err != nil {
		return stageSanitized(ctx, reconcileStageEpisodeSelection, err)
	}
	if len(episodes) == 0 {
		return nil
	}
	if err := ensureEpisodeSelection(ctx, submitter, downloader.Intent{
		ID:          target.ID,
		State:       downloader.IntentDownloading,
		TorrentHash: target.TorrentHash,
	}, episodes); err != nil {
		return stageSanitized(ctx, reconcileStageEpisodeSelection, err)
	}
	return nil
}

func (h *ReconciliationHandler) loadReconciledTargetEpisodes(
	ctx context.Context,
	username string,
	intentID uuid.UUID,
) ([]downloader.Episode, error) {
	return loadDownloadFileSelection(ctx, h.store, username, intentID, ErrDownloadReconciliationUnavailable)
}

func (h *ReconciliationHandler) completeReconciledDownload(
	ctx context.Context,
	target downloader.ReconciliationTarget,
) error {
	_, err := h.store.CompareAndSwapDownloadIntent(
		ctx,
		target.Username,
		target.ID,
		[]downloader.IntentState{downloader.IntentDownloading},
		downloader.IntentCompleted,
		target.TorrentHash,
	)
	if reconciliationConflict(err) {
		return nil
	}
	if err != nil {
		return stageSanitized(ctx, reconcileStageStoreWrite, err)
	}
	target.State = downloader.IntentCompleted
	return h.syncReconciledCompleted(ctx, target)
}

func (h *ReconciliationHandler) acceptReconciledDownload(
	ctx context.Context,
	target downloader.ReconciliationTarget,
) (supported bool, accepted bool, err error) {
	store, supported := h.store.(DownloadAcceptanceStore)
	if !supported {
		return false, false, nil
	}
	expectedHash, err := downloader.NormalizeTorrentHash(target.TorrentHash)
	if err != nil {
		return true, false, ErrDownloadReconciliationUnavailable
	}
	intent, subscriptionReleased, err := acceptDownloadIntent(
		ctx,
		store,
		downloadAcceptanceFinishSearch,
		target.Username,
		target.ID,
		target.TorrentHash,
	)
	if reconciliationConflict(err) {
		return true, false, nil
	}
	if err != nil {
		return true, false, err
	}
	acceptedHash, err := downloader.NormalizeTorrentHash(intent.TorrentHash)
	if err != nil || intent.ID != target.ID || intent.State != downloader.IntentDownloading ||
		acceptedHash != expectedHash {
		return true, false, ErrDownloadReconciliationUnavailable
	}
	return true, subscriptionReleased, nil
}

func (h *ReconciliationHandler) recoverReconciledIntent(
	ctx context.Context,
	target downloader.ReconciliationTarget,
) error {
	expectedUpdatedAt := target.UpdatedAt.UTC()
	err := h.store.RecoverSubscriptionDownloadIntent(
		ctx,
		target.Username,
		target.ID,
		[]downloader.IntentState{target.State},
		&expectedUpdatedAt,
	)
	if reconciliationConflict(err) {
		return nil
	}
	if err != nil {
		return stageSanitized(ctx, reconcileStageRecoverIntent, err)
	}
	return nil
}

func (h *ReconciliationHandler) syncReconciledEpisodes(
	ctx context.Context,
	target downloader.ReconciliationTarget,
	fromStates []subscription.SubscriptionEpisodeState,
	next subscription.SubscriptionEpisodeState,
) error {
	_, err := h.store.TransitionSubscriptionEpisodesByIntent(
		ctx,
		target.Username,
		target.ID,
		fromStates,
		next,
	)
	if err == nil || errors.Is(err, database.ErrSubscriptionEpisodeNotFound) {
		return nil
	}
	return stageSanitized(ctx, reconcileStageEpisodeSync, err)
}

func (h *ReconciliationHandler) syncReconciledDownloading(
	ctx context.Context,
	target downloader.ReconciliationTarget,
) error {
	if err := h.syncReconciledEpisodes(
		ctx,
		target,
		[]subscription.SubscriptionEpisodeState{subscription.SubscriptionEpisodeAcquired},
		subscription.SubscriptionEpisodeDownloading,
	); err != nil {
		return err
	}
	return h.syncReconciledParent(
		ctx,
		target,
		subscription.Acquired,
		subscription.Downloading,
	)
}

func (h *ReconciliationHandler) syncReconciledCompleted(
	ctx context.Context,
	target downloader.ReconciliationTarget,
) error {
	if err := h.syncReconciledDownloading(ctx, target); err != nil {
		return err
	}
	if err := h.syncReconciledEpisodes(
		ctx,
		target,
		[]subscription.SubscriptionEpisodeState{subscription.SubscriptionEpisodeDownloading},
		subscription.SubscriptionEpisodePendingOrganization,
	); err != nil {
		return err
	}
	if err := h.syncReconciledParent(
		ctx,
		target,
		subscription.Downloading,
		subscription.PendingOrganization,
	); err != nil {
		return err
	}
	h.publishDownloadCompleted(target)
	if h.completionHook == nil {
		return nil
	}
	target.State = downloader.IntentCompleted
	if err := h.completionHook.HandleDownloadCompleted(ctx, target); err != nil {
		return stageSanitized(ctx, reconcileStageCompletionHook, err)
	}
	return nil
}

func (h *ReconciliationHandler) syncReconciledParent(
	ctx context.Context,
	target downloader.ReconciliationTarget,
	current subscription.State,
	next subscription.State,
) error {
	err := h.store.CompareAndSwapSubscriptionStateByIntent(
		ctx,
		target.Username,
		target.ID,
		current,
		next,
	)
	if err == nil || errors.Is(err, database.ErrSubscriptionDownloadStateNotFound) ||
		errors.Is(err, database.ErrSubscriptionDownloadStateConflict) {
		return nil
	}
	return stageSanitized(ctx, reconcileStageSubscriptionSync, err)
}

func validateReconciliationPage(
	targets []downloader.ReconciliationTarget,
	cutoff time.Time,
	afterUpdatedAt time.Time,
	afterID uuid.UUID,
) error {
	previousTime, previousID := afterUpdatedAt, afterID
	for _, target := range targets {
		if target.ID == uuid.Nil || target.UpdatedAt.IsZero() || target.UpdatedAt.After(cutoff) ||
			!reconciliationCursorAfter(target.UpdatedAt, target.ID, previousTime, previousID) {
			return ErrInvalidDownloadReconciliationPage
		}
		previousTime, previousID = target.UpdatedAt, target.ID
	}
	return nil
}

func reconciliationCursorAfter(currentTime time.Time, currentID uuid.UUID, previousTime time.Time, previousID uuid.UUID) bool {
	if previousTime.IsZero() && previousID == uuid.Nil {
		return true
	}
	if currentTime.After(previousTime) {
		return true
	}
	return currentTime.Equal(previousTime) && currentID.String() > previousID.String()
}

func reconciliationConflict(err error) bool {
	return errors.Is(err, database.ErrDownloadIntentStateConflict) ||
		errors.Is(err, database.ErrDownloadIntentNotFound)
}

func reconciliationContextOrStable(ctx context.Context, err error) error {
	if contextErr := contextError(ctx, err); contextErr != nil {
		return contextErr
	}
	return ErrDownloadReconciliationUnavailable
}

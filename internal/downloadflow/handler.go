package downloadflow

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/moviepilot-go/moviepilot-go/internal/database"
	"github.com/moviepilot-go/moviepilot-go/internal/downloader"
	"github.com/moviepilot-go/moviepilot-go/internal/events"
	"github.com/moviepilot-go/moviepilot-go/internal/indexer"
	"github.com/moviepilot-go/moviepilot-go/internal/jobs"
	"github.com/moviepilot-go/moviepilot-go/internal/organization"
	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
	"github.com/moviepilot-go/moviepilot-go/internal/torrentmeta"
)

const (
	reconciliationSnooze = 30 * time.Second
	dependencySnooze     = 5 * time.Minute
	contentionSnooze     = 5 * time.Second
	maxHandlerCASLoops   = 3
)

var (
	// ErrDownloadStateUnavailable 表示当前无法读取或持久化下载状态。
	ErrDownloadStateUnavailable = errors.New("下载状态当前不可用")
	// ErrDownloadSourceUnavailable 表示站点资源暂时无法重新解析。
	ErrDownloadSourceUnavailable = errors.New("下载来源当前不可用")
	// ErrDownloadMetainfoInvalid 表示站点返回了不可接受的种子内容。
	ErrDownloadMetainfoInvalid = errors.New("下载来源返回无效种子")
)

// IntentStore 是下载 Worker 使用的 owner-scoped 最小持久化接口。
type IntentStore interface {
	GetDownloadIntent(ctx context.Context, username string, id uuid.UUID) (downloader.Intent, error)
	GetDownloadIntentPlan(ctx context.Context, username string, id uuid.UUID) (database.DownloadIntentPlan, error)
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

// DownloadAcceptanceStore atomically persists a confirmed downloader
// acceptance and consumes the subscription work bound to the intent. It is an
// optional capability so lightweight stores can keep using the V2 transition
// path during rolling upgrades.
type DownloadAcceptanceStore interface {
	AcceptDownloadIntent(
		ctx context.Context,
		username string,
		id uuid.UUID,
		torrentHash string,
	) (downloader.Intent, bool, error)
}

// DownloadSuccessInputStore exposes the immutable V2 submission snapshot.
// It is optional so legacy intents created before snapshots remain usable.
type DownloadSuccessInputStore interface {
	GetDownloadIntentSuccessInput(
		ctx context.Context,
		username string,
		intentID uuid.UUID,
	) (organization.SuccessInput, error)
}

// ResolverProvider 按持久化的站点解析器身份返回当前实现。
type ResolverProvider interface {
	ResolveDownloadResolver(siteID int, kind string, version int16) (indexer.VersionedDownloadResolver, error)
}

// Handler 实现 River 下载任务的恢复、对账、CAS 许可与单次外部提交。
type Handler struct {
	store          IntentStore
	resolvers      ResolverProvider
	submitters     SubmitterProvider
	completionHook DownloadCompletionHook
	// eventBus 是旁路事件总线；为空表示不广播完成事件（测试与最小部署）。
	eventBus *events.Bus
}

// SetEventBus 注入域事件总线，用于广播 download.completed。
func (h *Handler) SetEventBus(bus *events.Bus) {
	if h == nil {
		return
	}
	h.eventBus = bus
}

// publishedDownloadCompletions 去重：同一个下载意图只广播一次
// download.completed。对账任务会反复观察到已完成的种子，若不去重，
// 统计会被每轮对账重复累加。
var publishedDownloadCompletions sync.Map

// markDownloadCompletionPublished 返回 true 表示这是该意图第一次广播。
func markDownloadCompletionPublished(intentID uuid.UUID) bool {
	_, loaded := publishedDownloadCompletions.LoadOrStore(intentID.String(), struct{}{})
	return !loaded
}

// publishDownloadCompleted 广播下载完成事实。事件是旁路，不影响主流程结果。
func (h *Handler) publishDownloadCompleted(username string, intentID uuid.UUID, intent downloader.Intent, downloaderID string) {
	if h == nil || h.eventBus == nil {
		return
	}
	if !markDownloadCompletionPublished(intentID) {
		return
	}
	h.eventBus.Publish(events.Event{
		Type:     events.TypeDownloadCompleted,
		Username: username,
		Payload: map[string]any{
			"intent_id":     intentID.String(),
			"torrent_hash":  intent.TorrentHash,
			"label":         intent.Label,
			"downloader_id": downloaderID,
		},
	})
}

// NewHandler 创建下载闭环处理器。
func NewHandler(
	store IntentStore,
	resolvers ResolverProvider,
	submitters SubmitterProvider,
	completionHooks ...DownloadCompletionHook,
) (*Handler, error) {
	if store == nil || resolvers == nil || submitters == nil || len(completionHooks) > 1 ||
		(len(completionHooks) == 1 && completionHooks[0] == nil) {
		return nil, errors.New("下载处理器依赖不完整")
	}
	var completionHook DownloadCompletionHook
	if len(completionHooks) == 1 {
		completionHook = completionHooks[0]
	}
	return &Handler{
		store: store, resolvers: resolvers, submitters: submitters,
		completionHook: completionHook,
	}, nil
}

// ExecuteIntent synchronously executes one already-persisted intent while
// preserving HandleDownload's River behavior and error sanitization.
func (h *Handler) ExecuteIntent(
	ctx context.Context,
	username string,
	intentID uuid.UUID,
) (IntentExecutionResult, error) {
	return h.executeIntent(ctx, username, intentID, nil)
}

// ExecuteIntentWithMetainfo synchronously executes a staged intent with the
// immutable metainfo already fetched for V2 full-season or unpack inspection.
func (h *Handler) ExecuteIntentWithMetainfo(
	ctx context.Context,
	username string,
	intentID uuid.UUID,
	metainfo []byte,
) (IntentExecutionResult, error) {
	if len(metainfo) == 0 {
		return IntentExecutionResult{Outcome: IntentExecutionRetryableFailure}, ErrDownloadMetainfoInvalid
	}
	return h.executeIntent(ctx, username, intentID, metainfo)
}

func (h *Handler) executeIntent(
	ctx context.Context,
	username string,
	intentID uuid.UUID,
	metainfo []byte,
) (IntentExecutionResult, error) {
	retryable := IntentExecutionResult{Outcome: IntentExecutionRetryableFailure}
	if ctx == nil || h == nil || h.store == nil || h.resolvers == nil || h.submitters == nil {
		return retryable, ErrDownloadStateUnavailable
	}
	if err := ctx.Err(); err != nil {
		return retryable, err
	}
	args := jobs.DownloadArgs{Username: normalizeUsername(username), IntentID: intentID}
	if err := args.Validate(); err != nil {
		return retryable, ErrDownloadStateUnavailable
	}

	result, err := h.handleDownload(ctx, args, downloadAcceptanceBatch, true, metainfo)
	if contextErr := contextError(ctx, err); contextErr != nil {
		return retryable, contextErr
	}
	switch result.Action {
	case jobs.DownloadActionComplete:
		if err != nil {
			return retryable, ErrDownloadStateUnavailable
		}
		return IntentExecutionResult{Outcome: IntentExecutionAccepted}, nil
	case jobs.DownloadActionSnooze:
		if err != nil {
			return retryable, ErrDownloadStateUnavailable
		}
		retryAfter := result.SnoozeFor
		if retryAfter <= 0 {
			retryAfter = reconciliationSnooze
		}
		return IntentExecutionResult{
			Outcome:    IntentExecutionPendingReconciliation,
			RetryAfter: retryAfter,
		}, nil
	case jobs.DownloadActionRetry:
		if err == nil {
			err = ErrDownloadStateUnavailable
		}
		return retryable, err
	case jobs.DownloadActionCancel:
		return h.classifyCancelledIntent(ctx, args, err)
	default:
		return retryable, ErrDownloadStateUnavailable
	}
}

func (h *Handler) classifyCancelledIntent(
	ctx context.Context,
	args jobs.DownloadArgs,
	handleErr error,
) (IntentExecutionResult, error) {
	retryable := IntentExecutionResult{Outcome: IntentExecutionRetryableFailure}
	if handleErr != nil {
		return retryable, ErrDownloadStateUnavailable
	}
	intent, err := h.store.GetDownloadIntent(ctx, args.Username, args.IntentID)
	if err == nil {
		if intent.State == downloader.IntentFailed {
			return IntentExecutionResult{Outcome: IntentExecutionDefinitiveFailureRecovered}, nil
		}
		if intent.State == downloader.IntentPlanned {
			recoveryErr := h.store.RecoverSubscriptionDownloadIntent(
				ctx,
				args.Username,
				args.IntentID,
				[]downloader.IntentState{downloader.IntentPlanned},
				nil,
			)
			if recoveryErr != nil {
				return retryable, contextOrStableError(ctx, recoveryErr, ErrDownloadStateUnavailable)
			}
		}
		return IntentExecutionResult{Outcome: IntentExecutionPermanentCandidateFailure}, nil
	}
	if errors.Is(err, database.ErrDownloadIntentNotFound) || errors.Is(err, database.ErrInvalidDownloadIntent) {
		return IntentExecutionResult{Outcome: IntentExecutionPermanentCandidateFailure}, nil
	}
	return retryable, contextOrStableError(ctx, err, ErrDownloadStateUnavailable)
}

// HandleDownload 只接受 River 中的安全引用，并从数据库重建不可变执行计划。
func (h *Handler) HandleDownload(ctx context.Context, args jobs.DownloadArgs) (jobs.DownloadResult, error) {
	return h.handleDownload(ctx, args, downloadAcceptanceFinishSearch, false, nil)
}

func (h *Handler) handleDownload(
	ctx context.Context,
	args jobs.DownloadArgs,
	acceptanceMode downloadAcceptanceMode,
	acceptHashDespiteEpisodeSelectionFailure bool,
	resolvedMetainfo []byte,
) (jobs.DownloadResult, error) {
	if ctx == nil {
		return jobs.RetryDownload(), ErrDownloadStateUnavailable
	}
	if err := ctx.Err(); err != nil {
		return jobs.RetryDownload(), err
	}
	args.Username = normalizeUsername(args.Username)
	if err := args.Validate(); err != nil || h == nil || h.store == nil || h.resolvers == nil || h.submitters == nil {
		return jobs.CancelDownload(), nil
	}

	for range maxHandlerCASLoops {
		result, retry, err := h.handleOnce(
			ctx,
			args,
			acceptanceMode,
			acceptHashDespiteEpisodeSelectionFailure,
			resolvedMetainfo,
		)
		if !retry {
			return result, err
		}
	}
	return jobs.SnoozeDownload(contentionSnooze), nil
}

func (h *Handler) handleOnce(
	ctx context.Context,
	args jobs.DownloadArgs,
	acceptanceMode downloadAcceptanceMode,
	acceptHashDespiteEpisodeSelectionFailure bool,
	resolvedMetainfo []byte,
) (jobs.DownloadResult, bool, error) {
	intent, err := h.store.GetDownloadIntent(ctx, args.Username, args.IntentID)
	if err != nil {
		if errors.Is(err, database.ErrDownloadIntentNotFound) || errors.Is(err, database.ErrInvalidDownloadIntent) {
			return jobs.CancelDownload(), false, nil
		}
		return jobs.RetryDownload(), false, contextOrStableError(ctx, err, ErrDownloadStateUnavailable)
	}
	var targetEpisodes []downloader.Episode
	if intent.State != downloader.IntentCompleted {
		var targetErr error
		targetEpisodes, targetErr = h.loadTargetEpisodes(ctx, args.Username, args.IntentID)
		if targetErr != nil {
			return jobs.RetryDownload(), false, targetErr
		}
	}
	if intent.State == downloader.IntentDownloading {
		if len(targetEpisodes) > 0 {
			plan, planErr := h.store.GetDownloadIntentPlan(ctx, args.Username, args.IntentID)
			if planErr != nil {
				return jobs.RetryDownload(), false, contextOrStableError(ctx, planErr, ErrDownloadStateUnavailable)
			}
			submitter, resolveErr := h.submitters.ResolveSubmitter(plan.DownloaderID)
			if resolveErr != nil {
				return jobs.SnoozeDownload(dependencySnooze), false, nil
			}
			if selectionErr := h.ensureEpisodeSelection(ctx, submitter, intent, targetEpisodes); selectionErr != nil &&
				!canAcceptHashAfterEpisodeSelectionFailure(
					ctx,
					selectionErr,
					acceptHashDespiteEpisodeSelectionFailure,
				) {
				return episodeSelectionResult(ctx, selectionErr)
			}
		}
		if supported, acceptErr := h.acceptDownload(
			ctx,
			args,
			intent.TorrentHash,
			acceptHashDespiteEpisodeSelectionFailure,
			acceptanceMode,
		); supported {
			if errors.Is(acceptErr, database.ErrDownloadIntentStateConflict) {
				return jobs.DownloadResult{}, true, nil
			}
			if acceptErr != nil {
				return jobs.RetryDownload(), false, contextOrStableError(ctx, acceptErr, ErrDownloadStateUnavailable)
			}
			return jobs.CompleteDownload(), false, nil
		}
		if err := h.syncSubscriptionDownloading(ctx, args); err != nil {
			return jobs.RetryDownload(), false, err
		}
		return jobs.CompleteDownload(), false, nil
	}
	if intent.State == downloader.IntentCompleted {
		if err := h.syncSubscriptionCompleted(ctx, args); err != nil {
			return jobs.RetryDownload(), false, err
		}
		if err := h.notifyDownloadCompleted(ctx, args, intent); err != nil {
			return jobs.RetryDownload(), false, err
		}
		return jobs.CompleteDownload(), false, nil
	}

	plan, err := h.store.GetDownloadIntentPlan(ctx, args.Username, args.IntentID)
	if err != nil {
		if errors.Is(err, database.ErrDownloadIntentPlanNotFound) || errors.Is(err, database.ErrInvalidDownloadIntentPlan) {
			return jobs.CancelDownload(), false, nil
		}
		return jobs.RetryDownload(), false, contextOrStableError(ctx, err, ErrDownloadStateUnavailable)
	}
	candidate, err := SourceReference{
		SiteID:          plan.SiteID,
		ResolverKind:    plan.ResolverKind,
		ResolverVersion: plan.ResolverVersion,
		ResourceID:      plan.ResourceID,
		DownloaderID:    plan.DownloaderID,
	}.Candidate(plan.SavePath, plan.Paused)
	if err != nil || !candidateMatchesIntent(candidate, intent) {
		return jobs.CancelDownload(), false, nil
	}
	candidate.TargetEpisodes = targetEpisodes

	submitter, err := h.submitters.ResolveSubmitter(plan.DownloaderID)
	if err != nil {
		return jobs.SnoozeDownload(dependencySnooze), false, nil
	}
	reconciled, found, err := submitter.Reconcile(ctx, intent)
	if err != nil {
		if contextErr := contextError(ctx, err); contextErr != nil {
			return jobs.RetryDownload(), false, contextErr
		}
		if intent.State == downloader.IntentSubmitting {
			return jobs.SnoozeDownload(reconciliationSnooze), false, nil
		}
		return jobs.RetryDownload(), false, ErrDownloadStateUnavailable
	}
	if found {
		if selectionErr := h.ensureEpisodeSelection(ctx, submitter, reconciled, targetEpisodes); selectionErr != nil &&
			!canAcceptHashAfterEpisodeSelectionFailure(
				ctx,
				selectionErr,
				acceptHashDespiteEpisodeSelectionFailure,
			) {
			return episodeSelectionResult(ctx, selectionErr)
		}
		return h.persistReconciled(
			ctx,
			args,
			intent,
			reconciled,
			acceptHashDespiteEpisodeSelectionFailure,
			acceptanceMode,
		)
	}
	if intent.State == downloader.IntentSubmitting {
		return jobs.SnoozeDownload(reconciliationSnooze), false, nil
	}

	metainfo := resolvedMetainfo
	if len(metainfo) == 0 {
		locator, restoreErr := RestoreResourceLocator(plan.ResourceID, plan.ResourceSecret)
		if restoreErr != nil {
			return jobs.CancelDownload(), false, nil
		}
		resolver, resolveProviderErr := h.resolvers.ResolveDownloadResolver(
			int(plan.SiteID),
			plan.ResolverKind,
			plan.ResolverVersion,
		)
		if resolveProviderErr != nil {
			return jobs.SnoozeDownload(dependencySnooze), false, nil
		}
		download, resolveErr := resolver.ResolveDownload(ctx, locator)
		if resolveErr != nil {
			return jobs.RetryDownload(), false, contextOrStableError(ctx, resolveErr, ErrDownloadSourceUnavailable)
		}
		metainfo = download.Body
	}
	inspection, err := torrentmeta.Inspect(metainfo)
	if err != nil {
		return jobs.RetryDownload(), false, ErrDownloadMetainfoInvalid
	}

	permitted, err := h.store.CompareAndSwapDownloadIntent(
		ctx,
		args.Username,
		args.IntentID,
		[]downloader.IntentState{intent.State},
		downloader.IntentSubmitting,
		inspection.PrimaryHash,
	)
	if errors.Is(err, database.ErrDownloadIntentStateConflict) {
		return jobs.DownloadResult{}, true, nil
	}
	if err != nil {
		return jobs.RetryDownload(), false, contextOrStableError(ctx, err, ErrDownloadStateUnavailable)
	}

	updated, err := submitter.AddOnce(ctx, permitted, candidate, metainfo)
	if errors.Is(err, downloader.ErrReconciliationPending) {
		return jobs.SnoozeDownload(reconciliationSnooze), false, nil
	}
	if errors.Is(err, downloader.ErrDefinitiveAddFailure) {
		recoveryErr := h.store.RecoverSubscriptionDownloadIntent(
			ctx,
			args.Username,
			args.IntentID,
			[]downloader.IntentState{downloader.IntentSubmitting},
			nil,
		)
		if errors.Is(recoveryErr, database.ErrDownloadIntentStateConflict) {
			return jobs.DownloadResult{}, true, nil
		}
		if recoveryErr != nil {
			return jobs.RetryDownload(), false, contextOrStableError(ctx, recoveryErr, ErrDownloadStateUnavailable)
		}
		return jobs.CancelDownload(), false, nil
	}
	if err != nil {
		return jobs.RetryDownload(), false, contextOrStableError(ctx, err, ErrDownloadStateUnavailable)
	}
	if updated.State != downloader.IntentDownloading || updated.TorrentHash == "" {
		return jobs.SnoozeDownload(reconciliationSnooze), false, nil
	}
	// V2 `app/modules/qbittorrent/__init__.py:198–225` 在接管已有种子后直接返回
	// 「下载任务已存在」，不进入 238 行之后的选集分支；接管结果不能再次暂停、
	// 改文件优先级或恢复该种子。
	if !updated.AdoptedExisting {
		if selectionErr := h.ensureEpisodeSelection(ctx, submitter, updated, targetEpisodes); selectionErr != nil &&
			!canAcceptHashAfterEpisodeSelectionFailure(
				ctx,
				selectionErr,
				acceptHashDespiteEpisodeSelectionFailure,
			) {
			return episodeSelectionResult(ctx, selectionErr)
		}
	}
	if supported, acceptErr := h.acceptDownload(
		ctx,
		args,
		updated.TorrentHash,
		acceptHashDespiteEpisodeSelectionFailure,
		acceptanceMode,
	); supported {
		if errors.Is(acceptErr, database.ErrDownloadIntentStateConflict) {
			return jobs.DownloadResult{}, true, nil
		}
		if acceptErr != nil {
			return jobs.RetryDownload(), false, contextOrStableError(ctx, acceptErr, ErrDownloadStateUnavailable)
		}
		return jobs.CompleteDownload(), false, nil
	}
	_, err = h.store.CompareAndSwapDownloadIntent(
		ctx,
		args.Username,
		args.IntentID,
		[]downloader.IntentState{downloader.IntentSubmitting},
		downloader.IntentDownloading,
		updated.TorrentHash,
	)
	if errors.Is(err, database.ErrDownloadIntentStateConflict) {
		return jobs.DownloadResult{}, true, nil
	}
	if err != nil {
		return jobs.RetryDownload(), false, contextOrStableError(ctx, err, ErrDownloadStateUnavailable)
	}
	if err := h.syncSubscriptionDownloading(ctx, args); err != nil {
		return jobs.RetryDownload(), false, err
	}
	return jobs.CompleteDownload(), false, nil
}

// HandleDownloadRetryExhausted releases a subscription only when the final
// River attempt never crossed the external-side-effect boundary. Submitting
// and downloading intents are deliberately left to Hash reconciliation.
func (h *Handler) HandleDownloadRetryExhausted(ctx context.Context, args jobs.DownloadArgs) error {
	if ctx == nil || h == nil || h.store == nil {
		return ErrDownloadStateUnavailable
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	args.Username = strings.TrimSpace(args.Username)
	if err := args.Validate(); err != nil {
		return ErrDownloadStateUnavailable
	}
	intent, err := h.store.GetDownloadIntent(ctx, args.Username, args.IntentID)
	if errors.Is(err, database.ErrDownloadIntentNotFound) {
		return nil
	}
	if err != nil {
		return contextOrStableError(ctx, err, ErrDownloadStateUnavailable)
	}
	if intent.State != downloader.IntentPlanned && intent.State != downloader.IntentFailed {
		return nil
	}
	err = h.store.RecoverSubscriptionDownloadIntent(
		ctx,
		args.Username,
		args.IntentID,
		[]downloader.IntentState{downloader.IntentPlanned, downloader.IntentFailed},
		nil,
	)
	if err == nil || errors.Is(err, database.ErrDownloadIntentNotFound) ||
		errors.Is(err, database.ErrDownloadIntentStateConflict) {
		return nil
	}
	return contextOrStableError(ctx, err, ErrDownloadStateUnavailable)
}

func (h *Handler) notifyDownloadCompleted(
	ctx context.Context,
	args jobs.DownloadArgs,
	intent downloader.Intent,
) error {
	if h.completionHook == nil {
		h.publishDownloadCompleted(args.Username, args.IntentID, intent, "")
		return nil
	}
	plan, err := h.store.GetDownloadIntentPlan(ctx, args.Username, args.IntentID)
	if err != nil {
		return contextOrStableError(ctx, err, ErrDownloadStateUnavailable)
	}
	h.publishDownloadCompleted(args.Username, args.IntentID, intent, plan.DownloaderID)
	target := downloader.ReconciliationTarget{
		ID:           args.IntentID,
		Username:     args.Username,
		DownloaderID: plan.DownloaderID,
		State:        downloader.IntentCompleted,
		TorrentHash:  intent.TorrentHash,
	}
	if err := h.completionHook.HandleDownloadCompleted(ctx, target); err != nil {
		return contextOrStableError(ctx, err, ErrDownloadStateUnavailable)
	}
	return nil
}

func (h *Handler) persistReconciled(
	ctx context.Context,
	args jobs.DownloadArgs,
	intent downloader.Intent,
	reconciled downloader.Intent,
	requireSubscriptionRelease bool,
	acceptanceMode downloadAcceptanceMode,
) (jobs.DownloadResult, bool, error) {
	if reconciled.State != downloader.IntentDownloading || reconciled.TorrentHash == "" {
		return jobs.SnoozeDownload(reconciliationSnooze), false, nil
	}
	if intent.State == downloader.IntentPlanned || intent.State == downloader.IntentFailed {
		_, err := h.store.CompareAndSwapDownloadIntent(
			ctx,
			args.Username,
			args.IntentID,
			[]downloader.IntentState{intent.State},
			downloader.IntentSubmitting,
			reconciled.TorrentHash,
		)
		if errors.Is(err, database.ErrDownloadIntentStateConflict) {
			return jobs.DownloadResult{}, true, nil
		}
		if err != nil {
			return jobs.RetryDownload(), false, contextOrStableError(ctx, err, ErrDownloadStateUnavailable)
		}
	}
	if supported, acceptErr := h.acceptDownload(
		ctx,
		args,
		reconciled.TorrentHash,
		requireSubscriptionRelease,
		acceptanceMode,
	); supported {
		if errors.Is(acceptErr, database.ErrDownloadIntentStateConflict) {
			return jobs.DownloadResult{}, true, nil
		}
		if acceptErr != nil {
			return jobs.RetryDownload(), false, contextOrStableError(ctx, acceptErr, ErrDownloadStateUnavailable)
		}
		return jobs.CompleteDownload(), false, nil
	}
	_, err := h.store.CompareAndSwapDownloadIntent(
		ctx,
		args.Username,
		args.IntentID,
		[]downloader.IntentState{downloader.IntentSubmitting},
		downloader.IntentDownloading,
		reconciled.TorrentHash,
	)
	if errors.Is(err, database.ErrDownloadIntentStateConflict) {
		return jobs.DownloadResult{}, true, nil
	}
	if err != nil {
		return jobs.RetryDownload(), false, contextOrStableError(ctx, err, ErrDownloadStateUnavailable)
	}
	if err := h.syncSubscriptionDownloading(ctx, args); err != nil {
		return jobs.RetryDownload(), false, err
	}
	return jobs.CompleteDownload(), false, nil
}

func (h *Handler) acceptDownload(
	ctx context.Context,
	args jobs.DownloadArgs,
	torrentHash string,
	requireSubscriptionRelease bool,
	acceptanceMode downloadAcceptanceMode,
) (bool, error) {
	store, supported := h.store.(DownloadAcceptanceStore)
	if !supported {
		return false, nil
	}
	expectedHash, err := downloader.NormalizeTorrentHash(torrentHash)
	if err != nil {
		return true, ErrDownloadStateUnavailable
	}
	accepted, subscriptionReleased, err := acceptDownloadIntent(
		ctx,
		store,
		acceptanceMode,
		args.Username,
		args.IntentID,
		torrentHash,
	)
	if err != nil {
		return true, err
	}
	acceptedHash, err := downloader.NormalizeTorrentHash(accepted.TorrentHash)
	if err != nil || accepted.ID != args.IntentID || accepted.State != downloader.IntentDownloading ||
		acceptedHash != expectedHash {
		return true, ErrDownloadStateUnavailable
	}
	if requireSubscriptionRelease && !subscriptionReleased {
		return true, ErrDownloadStateUnavailable
	}
	return true, nil
}

func (h *Handler) loadTargetEpisodes(
	ctx context.Context,
	username string,
	intentID uuid.UUID,
) ([]downloader.Episode, error) {
	return loadDownloadFileSelection(ctx, h.store, username, intentID, ErrDownloadStateUnavailable)
}

func isDownloadSuccessInputNotFound(err error) bool {
	return errors.Is(err, database.ErrDownloadIntentSuccessInputNotFound)
}

func (h *Handler) ensureEpisodeSelection(
	ctx context.Context,
	submitter TorrentSubmitter,
	intent downloader.Intent,
	episodes []downloader.Episode,
) error {
	return ensureEpisodeSelection(ctx, submitter, intent, episodes)
}

func ensureEpisodeSelection(
	ctx context.Context,
	submitter TorrentSubmitter,
	intent downloader.Intent,
	episodes []downloader.Episode,
) error {
	if len(episodes) == 0 {
		return nil
	}
	selector, supported := submitter.(EpisodeFileSelectionSubmitter)
	if !supported {
		return downloader.ErrEpisodeSelectionUnavailable
	}
	return selector.SelectEpisodeFiles(ctx, intent, episodes)
}

func episodeSelectionResult(
	ctx context.Context,
	err error,
) (jobs.DownloadResult, bool, error) {
	if contextErr := contextError(ctx, err); contextErr != nil {
		return jobs.RetryDownload(), false, contextErr
	}
	if errors.Is(err, downloader.ErrEpisodeSelectionPending) {
		return jobs.SnoozeDownload(reconciliationSnooze), false, nil
	}
	for _, stable := range []error{
		downloader.ErrEpisodeSelectionUnavailable,
		downloader.ErrEpisodeSelectionNoMatch,
		downloader.ErrEpisodeSelectionFailed,
	} {
		if errors.Is(err, stable) {
			return jobs.RetryDownload(), false, errors.Join(ErrDownloadStateUnavailable, stable)
		}
	}
	return jobs.RetryDownload(), false, ErrDownloadStateUnavailable
}

func canAcceptHashAfterEpisodeSelectionFailure(
	ctx context.Context,
	err error,
	acceptHashDespiteEpisodeSelectionFailure bool,
) bool {
	return acceptHashDespiteEpisodeSelectionFailure && err != nil && contextError(ctx, err) == nil
}

func (h *Handler) syncSubscriptionEpisodes(
	ctx context.Context,
	args jobs.DownloadArgs,
	fromStates []subscription.SubscriptionEpisodeState,
	next subscription.SubscriptionEpisodeState,
) error {
	_, err := h.store.TransitionSubscriptionEpisodesByIntent(
		ctx,
		args.Username,
		args.IntentID,
		fromStates,
		next,
	)
	if err == nil || errors.Is(err, database.ErrSubscriptionEpisodeNotFound) {
		return nil
	}
	return contextOrStableError(ctx, err, ErrDownloadStateUnavailable)
}

func (h *Handler) syncSubscriptionDownloading(ctx context.Context, args jobs.DownloadArgs) error {
	if err := h.syncSubscriptionEpisodes(
		ctx,
		args,
		[]subscription.SubscriptionEpisodeState{subscription.SubscriptionEpisodeAcquired},
		subscription.SubscriptionEpisodeDownloading,
	); err != nil {
		return err
	}
	return h.syncParentSubscriptionState(
		ctx,
		args,
		subscription.Acquired,
		subscription.Downloading,
	)
}

func (h *Handler) syncSubscriptionCompleted(ctx context.Context, args jobs.DownloadArgs) error {
	if err := h.syncSubscriptionDownloading(ctx, args); err != nil {
		return err
	}
	if err := h.syncSubscriptionEpisodes(
		ctx,
		args,
		[]subscription.SubscriptionEpisodeState{subscription.SubscriptionEpisodeDownloading},
		subscription.SubscriptionEpisodePendingOrganization,
	); err != nil {
		return err
	}
	return h.syncParentSubscriptionState(
		ctx,
		args,
		subscription.Downloading,
		subscription.PendingOrganization,
	)
}

func (h *Handler) syncParentSubscriptionState(
	ctx context.Context,
	args jobs.DownloadArgs,
	current subscription.State,
	next subscription.State,
) error {
	err := h.store.CompareAndSwapSubscriptionStateByIntent(
		ctx,
		args.Username,
		args.IntentID,
		current,
		next,
	)
	if err == nil || errors.Is(err, database.ErrSubscriptionDownloadStateNotFound) ||
		errors.Is(err, database.ErrSubscriptionDownloadStateConflict) {
		return nil
	}
	return contextOrStableError(ctx, err, ErrDownloadStateUnavailable)
}

func candidateMatchesIntent(candidate downloader.Candidate, intent downloader.Intent) bool {
	associationKey, err := downloader.CandidateAssociationKey(candidate)
	return err == nil && associationKey == intent.CandidateKey
}

func normalizeUsername(value string) string {
	return strings.TrimSpace(value)
}

func contextOrStableError(ctx context.Context, source, stable error) error {
	if err := contextError(ctx, source); err != nil {
		return err
	}
	return stable
}

func contextError(ctx context.Context, err error) error {
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

var (
	_ jobs.DownloadHandler       = (*Handler)(nil)
	_ DownloadAcceptanceStore    = (*database.Store)(nil)
	_ DownloadFileSelectionStore = (*database.Store)(nil)
)

// Package subscriptionflow connects durable subscription searches to indexers
// and atomic download submission.
package subscriptionflow

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/moviepilot-go/moviepilot-go/internal/database"
	"github.com/moviepilot-go/moviepilot-go/internal/downloadconfig"
	"github.com/moviepilot-go/moviepilot-go/internal/downloader"
	"github.com/moviepilot-go/moviepilot-go/internal/downloadflow"
	"github.com/moviepilot-go/moviepilot-go/internal/indexer"
	"github.com/moviepilot-go/moviepilot-go/internal/media"
	"github.com/moviepilot-go/moviepilot-go/internal/organization"
	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

const (
	searchPageSize     = 100
	maxSearchAdapters  = 100
	maxTrackedEpisodes = 10_000
	maxTrackedEpisode  = 5_000
	maxTrackedSeason   = 999
	directoriesSetting = "Directories"
)

var (
	ErrInvalidHandler               = errors.New("订阅精准搜索处理器依赖不完整")
	ErrSubscriptionSearchStorage    = errors.New("订阅精准搜索存储暂时不可用")
	ErrSubscriptionIndexer          = errors.New("订阅精准搜索站点暂时不可用")
	ErrSubscriptionDownloadSettings = errors.New("订阅下载配置暂时不可用")
	ErrSubscriptionConfiguration    = errors.New("订阅配置无效")
	ErrSubscriptionSubmission       = errors.New("订阅下载提交失败")
	ErrSubscriptionTargetChanged    = errors.New("订阅目标快照已变化")
)

// These aliases contain the package's existing persistence-type migration
// debt in the legacy boundary file. New flow files must not import database.
type persistedSubscription = database.Subscription
type persistedDownloadIntentPlan = database.DownloadIntentPlan

var errSubscriptionSearchStateConflict = database.ErrSubscriptionPreciseSearchStateConflict

type TargetStore interface {
	GetActiveSubscription(context.Context, string, int64) (database.Subscription, error)
	ListSubscriptionsForPreciseSearch(
		ctx context.Context,
		cutoff time.Time,
		afterUpdatedAt time.Time,
		afterID int64,
		limit int,
	) ([]database.Subscription, error)
	CompareAndSwapSubscriptionPreciseSearchState(
		ctx context.Context,
		username string,
		id int64,
		next subscription.State,
	) error
	CompareAndSwapSubscriptionPreciseSearchStateAt(
		context.Context, string, int64, subscription.State, time.Time,
	) error
	UpsertSubscriptionEpisodes(
		ctx context.Context,
		username string,
		subscriptionID int64,
		episodes []subscription.Episode,
	) error
	ListSubscriptionEpisodes(
		ctx context.Context,
		username string,
		subscriptionID int64,
		states []subscription.SubscriptionEpisodeState,
		limit int,
	) ([]subscription.SubscriptionEpisode, error)
	ReconcileSubscriptionTarget(context.Context, database.SubscriptionTargetReconcile) (time.Time, error)
	ListSubscriptionEpisodesForTarget(
		context.Context,
		string,
		int64,
		time.Time,
		[]subscription.Episode,
		[]subscription.SubscriptionEpisodeState,
		int,
	) ([]subscription.SubscriptionEpisode, error)
	LoadDownloaders(context.Context) ([]downloadconfig.Downloader, bool, error)
	GetSetting(context.Context, string, any) (bool, error)
}

type v2CandidateSortInputStore interface {
	LoadV2CandidateSortInputs(context.Context) ([]string, map[string]float64, map[int]int, error)
}

type satisfiedBestVersionTVSubscriptionStore interface {
	CompleteSatisfiedBestVersionTVSubscription(context.Context, string, int64, subscription.State, time.Time) error
}

type Searcher interface {
	Search(
		ctx context.Context,
		adapters []indexer.Adapter,
		query indexer.Query,
		onEvent func(indexer.Event),
	) ([]indexer.Resource, error)
}

type AdapterProvider interface {
	Snapshot() []indexer.Adapter
}

type DownloadSubmission interface {
	SubmitSubscriptionEpisodes(
		ctx context.Context,
		username string,
		subscriptionID int64,
		intent downloader.Intent,
		plan database.DownloadIntentPlan,
		successInput organization.SuccessInput,
		episodes []subscription.Episode,
	) (uuid.UUID, error)
	SubmitSubscriptionTargetEpisodes(
		context.Context,
		string,
		int64,
		time.Time,
		downloader.Intent,
		database.DownloadIntentPlan,
		organization.SuccessInput,
		[]subscription.Episode,
	) (uuid.UUID, error)
}

type Handler struct {
	store             TargetStore
	searcher          Searcher
	adapters          AdapterProvider
	submission        DownloadSubmission
	execution         DownloadIntentExecution
	mediaDetails      MediaDetailResolver
	mediaSupplementer MediaSupplementer
	categoryResolver  CategoryResolver
	existingEpisodes  ExistingEpisodesSource
	existingMovies    ExistingMovieSource
	logger            *slog.Logger
}

// SetLogger 注入可选的跳过日志记录器；未注入时静默跳过失败的订阅目标。
func (h *Handler) SetLogger(logger *slog.Logger) {
	if h == nil {
		return
	}
	h.logger = logger
}

func (h *Handler) reconcileTVTarget(
	ctx context.Context,
	target persistedSubscription,
	resolution subscription.TVTargetResolution,
) (time.Time, error) {
	next, err := h.store.ReconcileSubscriptionTarget(ctx, database.SubscriptionTargetReconcile{
		Username: target.Username, SubscriptionID: target.ID,
		ExpectedUpdatedAt: target.UpdatedAt, Target: resolution,
	})
	if errors.Is(err, database.ErrSubscriptionTargetConflict) || errors.Is(err, database.ErrSubscriptionTargetNotFound) {
		return time.Time{}, ErrSubscriptionTargetChanged
	}
	return next, err
}

func subscriptionTargetNotFound(err error) bool {
	return errors.Is(err, database.ErrSubscriptionNotFound)
}

func NewHandler(
	store TargetStore,
	searcher Searcher,
	adapters AdapterProvider,
	submission DownloadSubmission,
	executions ...DownloadIntentExecution,
) (*Handler, error) {
	if store == nil || searcher == nil || adapters == nil || submission == nil || len(executions) > 1 ||
		(len(executions) == 1 && executions[0] == nil) {
		return nil, ErrInvalidHandler
	}
	if len(executions) == 1 {
		if _, ok := submission.(stagedTVDownloadSubmission); !ok {
			return nil, ErrInvalidHandler
		}
	}
	var execution DownloadIntentExecution
	if len(executions) == 1 {
		execution = executions[0]
	}
	return &Handler{
		store: store, searcher: searcher, adapters: adapters,
		submission: submission, execution: execution,
	}, nil
}

// HandleSubscriptionSearch processes a fixed database snapshot with bounded
// keyset pages. A target that was paused, deleted, or handled concurrently is
// skipped through its owner-scoped CAS.
func (h *Handler) HandleSubscriptionSearch(ctx context.Context, cutoff time.Time) error {
	if ctx == nil || h == nil || h.store == nil || h.searcher == nil || h.adapters == nil || h.submission == nil {
		return ErrInvalidHandler
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if cutoff.IsZero() {
		return ErrSubscriptionSearchStorage
	}

	var afterUpdatedAt time.Time
	var afterID int64
	succeeded := 0
	for {
		targets, err := h.store.ListSubscriptionsForPreciseSearch(
			ctx,
			cutoff.UTC(),
			afterUpdatedAt,
			afterID,
			searchPageSize,
		)
		if err != nil {
			return stableContextError(ctx, err, ErrSubscriptionSearchStorage)
		}
		if len(targets) == 0 {
			return nil
		}
		var firstTargetErr error
		for _, target := range targets {
			if err := h.handleTarget(ctx, target); err != nil {
				if ctxErr := ctx.Err(); ctxErr != nil {
					return stableContextError(ctx, err, ErrSubscriptionSearchStorage)
				}
				// 单个订阅失败（外部依赖超时、站点不可用等）不阻断本轮其他订阅；
				// 只有整轮全败才把错误上抛触发退避重试，避免一个坏目标的失败
				// 拖累其他订阅让整轮 job 被丢弃。失败目标保持待搜索状态。
				if firstTargetErr == nil {
					firstTargetErr = err
				}
				if h.logger != nil {
					h.logger.Warn("订阅精准搜索跳过失败目标", "subscription_id", target.ID, "error", err)
				}
			} else {
				succeeded++
			}
			afterUpdatedAt = target.UpdatedAt.UTC()
			afterID = target.ID
		}
		if len(targets) < searchPageSize {
			if succeeded == 0 {
				return firstTargetErr
			}
			if firstTargetErr != nil && h.logger != nil {
				h.logger.Warn("订阅精准搜索部分目标失败，本轮其余目标已完成", "failed_targets", len(targets)-succeeded)
			}
			return nil
		}
	}
}

func (h *Handler) handleTarget(ctx context.Context, target database.Subscription) error {
	var tvTarget subscription.TVTargetResolution
	var tvRead tvSearchReadState
	if media.MediaType(strings.TrimSpace(target.MediaType)) == media.TypeTV {
		var refreshErr error
		var refreshed persistedSubscription
		refreshed, tvTarget, tvRead, refreshErr = h.prepareTVSearchTarget(ctx, target)
		if errors.Is(refreshErr, ErrSubscriptionTargetChanged) {
			return nil
		}
		if errors.Is(refreshErr, ErrSubscriptionConfiguration) || errors.Is(refreshErr, database.ErrInvalidSubscriptionTarget) {
			return h.finishTarget(ctx, target, subscription.Paused)
		}
		if refreshErr != nil {
			return stableContextError(ctx, refreshErr, ErrSubscriptionSearchStorage)
		}
		target = refreshed
		if tvRead.InitialRecognitionFailed || tvRead.UnresolvedSeasonFacts {
			if target.InternalState != subscription.PendingSearch {
				return nil
			}
			return h.finishTarget(ctx, target, subscription.Active)
		}
		if tvRead.PreflightSatisfied {
			err := h.finishEmptyTVMissingContainer(ctx, target)
			if errors.Is(err, ErrSubscriptionTargetChanged) {
				return nil
			}
			return err
		}
	}
	query, searchKeyword, err := candidateQuery(target)
	if err != nil {
		return h.finishTarget(ctx, target, subscription.Paused)
	}
	if query.MediaType == media.TypeTV {
		var matchable, completable bool
		if tvRead.MissingContainerEmpty {
			query.MissingEpisodes = nil
			query.FullSeasonMissing = nil
			query.TVMissingContainerEmpty = true
			matchable = true
		} else if tvRead.RecoveredTotal > 0 {
			query, err = h.withRecoveredTVMissingEpisodes(ctx, target, tvTarget, tvRead.RecoveredTotal, query, tvRead.RecoveredLibraryQuery)
			completable, matchable = true, true
		} else {
			query, completable, matchable, err = h.withCurrentTVMissingEpisodes(ctx, target, tvTarget, tvRead.LegacyWithoutTotal, query)
		}
		if err != nil {
			if errors.Is(err, database.ErrInvalidSubscriptionTarget) {
				return h.finishTarget(ctx, target, subscription.Paused)
			}
			return stableContextError(ctx, err, ErrSubscriptionSearchStorage)
		}
		if !matchable {
			return h.finishTarget(ctx, target, subscription.Active)
		}
		if len(query.MissingEpisodes) == 0 && !query.TVMissingContainerEmpty {
			if !completable {
				return h.finishTarget(ctx, target, subscription.Active)
			}
			var err error
			if query.TVRecoveredTarget != nil {
				err = h.finishRecoveredTVTarget(ctx, target, 0)
			} else if subscriptionBestVersionEnabled(target.Config) {
				err = h.finishSatisfiedTarget(ctx, target, subscription.PendingSearch)
			} else {
				err = h.finishEmptyTVMissingContainer(ctx, target)
			}
			if errors.Is(err, ErrSubscriptionTargetChanged) {
				return nil
			}
			return err
		}
	}
	if query.MediaType == media.TypeMovie {
		satisfied, lookupErr := h.existingMovieSatisfied(
			ctx, target.Name, target.Year, target.MediaSource, target.MediaID, target.Config,
		)
		if lookupErr != nil {
			return stableContextError(ctx, lookupErr, ErrSubscriptionSearchStorage)
		}
		if satisfied {
			err := h.finishSatisfiedMovieTarget(ctx, target, subscription.PendingSearch)
			if errors.Is(err, ErrSubscriptionTargetChanged) {
				return nil
			}
			return err
		}
	}
	adapters := selectAdapters(h.adapters.Snapshot(), query.SiteIDs)
	if len(adapters) == 0 {
		if len(query.SiteIDs) > 0 {
			return h.finishTarget(ctx, target, subscription.Paused)
		}
		return ErrSubscriptionIndexer
	}
	query, err = h.withV2DefaultCandidateFilters(ctx, target.Config, query)
	if err != nil {
		return stableContextError(ctx, err, ErrSubscriptionSearchStorage)
	}

	events, failures := 0, 0
	resources := make([]indexer.Resource, 0)
	_, err = h.searcher.Search(
		ctx,
		adapters,
		indexer.Query{Keyword: searchKeyword, Page: 0, Limit: 100},
		func(event indexer.Event) {
			events++
			if event.Error != "" {
				failures++
			}
			resources = append(resources, event.Resources...)
		},
	)
	if err != nil {
		return stableContextError(ctx, err, ErrSubscriptionIndexer)
	}
	if events == 0 || failures == events {
		return ErrSubscriptionIndexer
	}

	candidates, err := subscription.MatchV2DownloadCandidates(query, resources)
	if err != nil {
		if errors.Is(err, subscription.ErrInvalidCandidateQuery) {
			return h.finishTarget(ctx, target, subscription.Paused)
		}
		return ErrSubscriptionIndexer
	}
	candidates, err = h.filterV2SubscriptionCandidates(ctx, target, candidates)
	if err != nil {
		if errors.Is(err, subscription.ErrInvalidV2FilterRules) {
			return ErrSubscriptionIndexer
		}
		return stableContextError(ctx, err, ErrSubscriptionSearchStorage)
	}
	candidates, err = sortV2CandidatesFromStore(ctx, h.store, candidates)
	if err != nil {
		return stableContextError(ctx, err, ErrSubscriptionSearchStorage)
	}
	if query.TVMissingContainerEmpty {
		err := h.finishEmptyTVMissingContainer(ctx, target)
		if errors.Is(err, ErrSubscriptionTargetChanged) {
			return nil
		}
		return err
	}
	if query.MediaType == media.TypeTV && canExecuteV2TVCandidates(h, false) {
		_, executeErr := h.executeV2TVCandidates(ctx, target, query, candidates, adapters, false)
		if errors.Is(executeErr, ErrSubscriptionConfiguration) {
			return h.finishTarget(ctx, target, subscription.Paused)
		}
		if executeErr != nil {
			return stableContextError(ctx, executeErr, ErrSubscriptionSubmission)
		}
		return nil
	}
	if query.MediaType == media.TypeMovie && canExecuteV2TVCandidates(h, false) {
		result, executeErr := h.executeV2MovieCandidates(ctx, target, candidates, adapters, false)
		if errors.Is(executeErr, ErrSubscriptionConfiguration) {
			return h.finishTarget(ctx, target, subscription.Paused)
		}
		if executeErr != nil {
			return stableContextError(ctx, executeErr, ErrSubscriptionSubmission)
		}
		if result.Accepted || result.Stopped {
			return nil
		}
		return h.finishTarget(ctx, target, subscription.Active)
	}
	selected, resolver, ok := firstDownloadableCandidate(candidates, adapters)
	if !ok {
		if query.TVRecoveredTarget != nil {
			return h.finishRecoveredTVTarget(ctx, target, len(query.MissingEpisodes))
		}
		return h.finishTarget(ctx, target, subscription.Active)
	}
	if query.MediaType == media.TypeTV && len(selected.CoveredEpisodes) == 0 {
		if query.TVRecoveredTarget != nil {
			return h.finishRecoveredTVTarget(ctx, target, len(query.MissingEpisodes))
		}
		return h.finishTarget(ctx, target, subscription.Active)
	}

	configuredDownloader, savePath, err := h.downloadSettings(ctx, target.Config)
	if err != nil {
		if errors.Is(err, ErrSubscriptionConfiguration) {
			return h.finishTarget(ctx, target, subscription.Paused)
		}
		return err
	}
	resourceID, resourceSecret, err := downloadflow.ProtectResourceLocator(selected.Resource.DownloadID)
	if err != nil {
		return h.finishTarget(ctx, target, subscription.Active)
	}
	reference := downloadflow.SourceReference{
		SiteID:          int64(selected.Resource.SiteID),
		ResolverKind:    resolver.ResolverKind(),
		ResolverVersion: resolver.ResolverVersion(),
		ResourceID:      resourceID,
		DownloaderID:    configuredDownloader.ID,
	}
	downloadCandidate, err := reference.Candidate(savePath, false)
	if err != nil {
		return ErrSubscriptionSubmission
	}
	intent, err := downloader.NewIntent(target.Username, downloadCandidate)
	if err != nil {
		return ErrSubscriptionSubmission
	}
	plan := database.DownloadIntentPlan{
		IntentID:        intent.ID,
		SiteID:          int64(selected.Resource.SiteID),
		DownloaderID:    configuredDownloader.ID,
		ResolverKind:    resolver.ResolverKind(),
		ResolverVersion: resolver.ResolverVersion(),
		ResourceID:      resourceID,
		ResourceSecret:  resourceSecret,
		SavePath:        savePath,
		MoviePriority:   movieCandidatePriority(target, selected),
	}
	if query.TVRecoveredTarget != nil {
		plan.TVRecoveredTarget = recoveredTVPlanTarget(query, selected.CoveredEpisodes)
		plan.TVPriority = intPointerCopy(selected.Resource.PriOrder)
		plan.TVCoverage = append([]subscription.Episode{}, selected.CoveredEpisodes...)
	}
	if _, err := h.submission.SubmitSubscriptionTargetEpisodes(
		ctx,
		target.Username,
		target.ID,
		target.UpdatedAt,
		intent,
		plan,
		h.buildSubscriptionSuccessInput(ctx, subscriptionSuccessTarget{
			Username: target.Username, Name: target.Name, Year: target.Year,
			MediaType: target.MediaType, MediaSource: target.MediaSource,
			MediaID: target.MediaID, VoteAverage: subscriptionConfigVote(target.Config),
			PosterURL: configText(target.Config, "poster"), BackdropURL: configText(target.Config, "backdrop"),
			Config: target.Config,
		}, selected, intent.ID),
		selected.CoveredEpisodes,
	); err != nil {
		return stableContextError(ctx, err, ErrSubscriptionSubmission)
	}
	return nil
}

func sortV2CandidatesFromStore(
	ctx context.Context,
	store any,
	candidates []subscription.Candidate,
) ([]subscription.Candidate, error) {
	provider, ok := store.(v2CandidateSortInputStore)
	if !ok {
		return subscription.SortV2Candidates(candidates, nil, nil), nil
	}
	priorityRules, siteUploads, siteOrders, err := provider.LoadV2CandidateSortInputs(ctx)
	if err != nil {
		return nil, err
	}
	prepared := append([]subscription.Candidate(nil), candidates...)
	for index := range prepared {
		if siteOrder, exists := siteOrders[prepared[index].Resource.SiteID]; exists {
			prepared[index].Resource.SiteOrder = siteOrder
		}
	}
	return subscription.SortV2Candidates(prepared, priorityRules, siteUploads), nil
}

func (h *Handler) withCurrentTVMissingEpisodes(
	ctx context.Context,
	target database.Subscription,
	resolution subscription.TVTargetResolution,
	legacyWithoutTotal bool,
	query subscription.Query,
) (subscription.Query, bool, bool, error) {
	if query.MediaType != media.TypeTV {
		return query, false, false, nil
	}
	query.FullSeasonMissing = make(map[int]bool)
	if resolution.Snapshot.BestVersion {
		facts, err := subscription.DescribeTVBestVersion(resolution.Snapshot)
		if err != nil {
			return subscription.Query{}, false, false, database.ErrInvalidSubscriptionTarget
		}
		query.MissingEpisodes = append([]subscription.Episode{}, facts.PendingEpisodes...)
		if resolution.Snapshot.BestVersionFull {
			query.MissingEpisodes = []subscription.Episode{}
			if !facts.Complete {
				query.MissingEpisodes = append(query.MissingEpisodes, resolution.TargetEpisodes...)
			}
		}
		return query, facts.Complete, facts.Complete || len(resolution.TargetEpisodes) > 0, nil
	}
	if resolution.Knowledge == subscription.TVTargetKnown && len(resolution.TargetEpisodes) == 0 {
		if _, err := h.emptyTVMetadataLibrarySatisfied(ctx, target); err != nil {
			return subscription.Query{}, false, false, err
		}
		query.MissingEpisodes = nil
		return query, true, true, nil
	}
	var stored []subscription.SubscriptionEpisode
	var err error
	known := resolution.Knowledge == subscription.TVTargetKnown
	if legacyWithoutTotal {
		stored, err = h.store.ListSubscriptionEpisodes(
			ctx, target.Username, target.ID,
			[]subscription.SubscriptionEpisodeState{subscription.SubscriptionEpisodeMissing}, maxTrackedEpisodes,
		)
	} else {
		stored, err = h.store.ListSubscriptionEpisodesForTarget(
			ctx, target.Username, target.ID, target.UpdatedAt, resolution.TargetEpisodes,
			[]subscription.SubscriptionEpisodeState{
				subscription.SubscriptionEpisodeMissing,
				subscription.SubscriptionEpisodeAcquired,
				subscription.SubscriptionEpisodeDownloading,
				subscription.SubscriptionEpisodePendingOrganization,
				subscription.SubscriptionEpisodeOrganizing,
				subscription.SubscriptionEpisodePendingIndex,
				subscription.SubscriptionEpisodeIndexed,
			}, maxTrackedEpisodes,
		)
	}
	if err != nil {
		return subscription.Query{}, false, false, err
	}
	query.MissingEpisodes = make([]subscription.Episode, 0, len(stored))
	allIndexed := len(stored) == len(resolution.TargetEpisodes) && len(stored) > 0
	hasInFlight := false
	for _, item := range stored {
		if item.State != subscription.SubscriptionEpisodeMissing {
			if item.State != subscription.SubscriptionEpisodeIndexed {
				allIndexed = false
				hasInFlight = true
			}
			continue
		}
		allIndexed = false
		query.MissingEpisodes = append(query.MissingEpisodes, subscription.Episode{
			Season:  item.Season,
			Episode: item.Episode,
		})
	}
	missingBeforeLibrary := len(query.MissingEpisodes)
	existing, err := h.excludeExistingEpisodesWithShape(ctx, ExistingEpisodesQuery{
		Title:       target.Name,
		Year:        target.Year,
		MediaSource: target.MediaSource,
		MediaID:     target.MediaID,
	}, target.Season, target.Config, query.MissingEpisodes)
	if err != nil {
		return subscription.Query{}, false, false, err
	}
	query.MissingEpisodes = existing.Episodes
	query.FullSeasonMissing = ordinaryTVFullSeasonMissing(resolution, stored, existing)
	if legacyWithoutTotal {
		return query, false, len(stored) > 0, nil
	}
	if !known || len(resolution.TargetEpisodes) == 0 {
		return query, false, false, nil
	}
	if allIndexed {
		return query, true, true, nil
	}
	if len(query.MissingEpisodes) == 0 {
		if missingBeforeLibrary > 0 && !hasInFlight {
			return query, true, true, nil
		}
		return query, false, false, nil
	}
	return query, true, true, nil
}

func metadataEpisodeFallbackAllowed(config map[string]any) bool {
	value, exists := config["total_episode"]
	if !exists {
		return true
	}
	total, ok := integerValue(value)
	return ok && total == 0
}

func (h *Handler) finishTarget(
	ctx context.Context,
	target database.Subscription,
	next subscription.State,
) error {
	err := h.store.CompareAndSwapSubscriptionPreciseSearchStateAt(
		ctx,
		target.Username,
		target.ID,
		next,
		target.UpdatedAt,
	)
	if err == nil || errors.Is(err, database.ErrSubscriptionNotFound) ||
		errors.Is(err, database.ErrSubscriptionPreciseSearchStateConflict) {
		return nil
	}
	return stableContextError(ctx, err, ErrSubscriptionSearchStorage)
}

func (h *Handler) finishSatisfiedTarget(
	ctx context.Context,
	target database.Subscription,
	expected subscription.State,
) error {
	if subscriptionBestVersionEnabled(target.Config) {
		store, supported := h.store.(satisfiedBestVersionTVSubscriptionStore)
		if !supported {
			return ErrSubscriptionSearchStorage
		}
		err := store.CompleteSatisfiedBestVersionTVSubscription(
			ctx, target.Username, target.ID, expected, target.UpdatedAt,
		)
		if err == nil {
			return nil
		}
		if errors.Is(err, database.ErrSubscriptionNotFound) || errors.Is(err, database.ErrSubscriptionPreciseSearchStateConflict) {
			return ErrSubscriptionTargetChanged
		}
		return stableContextError(ctx, err, ErrSubscriptionSearchStorage)
	}
	store, supported := h.store.(SatisfiedSubscriptionStore)
	if !supported {
		return ErrSubscriptionSearchStorage
	}
	err := store.CompleteSatisfiedTVSubscription(
		ctx,
		target.Username,
		target.ID,
		expected,
		target.UpdatedAt,
	)
	if err == nil || errors.Is(err, database.ErrSubscriptionNotFound) {
		return nil
	}
	if errors.Is(err, database.ErrSubscriptionPreciseSearchStateConflict) {
		return ErrSubscriptionTargetChanged
	}
	return stableContextError(ctx, err, ErrSubscriptionSearchStorage)
}

func (h *Handler) finishSatisfiedMovieTarget(
	ctx context.Context,
	target database.Subscription,
	expected subscription.State,
) error {
	store, supported := h.store.(SatisfiedMovieSubscriptionStore)
	if !supported {
		return ErrSubscriptionSearchStorage
	}
	err := store.CompleteSatisfiedMovieSubscription(
		ctx,
		target.Username,
		target.ID,
		expected,
		target.UpdatedAt,
	)
	if err == nil || errors.Is(err, database.ErrSubscriptionNotFound) {
		return nil
	}
	if errors.Is(err, database.ErrSubscriptionPreciseSearchStateConflict) {
		return ErrSubscriptionTargetChanged
	}
	return stableContextError(ctx, err, ErrSubscriptionSearchStorage)
}

func (h *Handler) downloadSettings(
	ctx context.Context,
	config map[string]any,
) (downloadconfig.Downloader, string, error) {
	downloaders, found, err := h.store.LoadDownloaders(ctx)
	if err != nil || !found {
		return downloadconfig.Downloader{}, "", stableContextError(ctx, err, ErrSubscriptionDownloadSettings)
	}
	requestedDownloader := configText(config, "downloader")
	selected, ok := downloadconfig.SelectEnabledDownloader(downloaders, requestedDownloader)
	if !ok {
		if requestedDownloader != "" {
			return downloadconfig.Downloader{}, "", ErrSubscriptionConfiguration
		}
		return downloadconfig.Downloader{}, "", ErrSubscriptionDownloadSettings
	}
	var directories []downloadconfig.Directory
	found, err = h.store.GetSetting(ctx, directoriesSetting, &directories)
	if err != nil {
		return downloadconfig.Downloader{}, "", stableContextError(ctx, err, ErrSubscriptionDownloadSettings)
	}
	if !found {
		directories = []downloadconfig.Directory{}
	}
	requestedSavePath := configText(config, "save_path")
	savePath, ok := downloadconfig.AllowedLocalSavePath(directories, requestedSavePath)
	if !ok {
		return downloadconfig.Downloader{}, "", ErrSubscriptionConfiguration
	}
	return selected, savePath, nil
}

func selectAdapters(all []indexer.Adapter, siteIDs []int) []indexer.Adapter {
	byID := make(map[int]indexer.Adapter, len(all))
	for _, adapter := range all {
		identified, ok := adapter.(indexer.SiteIDAdapter)
		if !ok || identified.SiteID() <= 0 {
			continue
		}
		if _, exists := byID[identified.SiteID()]; !exists {
			byID[identified.SiteID()] = adapter
		}
	}
	selected := make([]indexer.Adapter, 0, min(len(byID), maxSearchAdapters))
	if len(siteIDs) > 0 {
		for _, siteID := range siteIDs {
			if adapter := byID[siteID]; adapter != nil {
				selected = append(selected, adapter)
				delete(byID, siteID)
			}
			if len(selected) == maxSearchAdapters {
				break
			}
		}
		return selected
	}
	orderedIDs := make([]int, 0, len(byID))
	for siteID := range byID {
		orderedIDs = append(orderedIDs, siteID)
	}
	sort.Ints(orderedIDs)
	for _, siteID := range orderedIDs {
		selected = append(selected, byID[siteID])
		if len(selected) == maxSearchAdapters {
			break
		}
	}
	return selected
}

func firstDownloadableCandidate(
	candidates []subscription.Candidate,
	adapters []indexer.Adapter,
) (subscription.Candidate, indexer.VersionedDownloadResolver, bool) {
	resolvers := make(map[int]indexer.VersionedDownloadResolver, len(adapters))
	for _, adapter := range adapters {
		identified, identifiedOK := adapter.(indexer.SiteIDAdapter)
		resolver, resolverOK := adapter.(indexer.VersionedDownloadResolver)
		if !identifiedOK || !resolverOK || strings.TrimSpace(resolver.ResolverKind()) == "" || resolver.ResolverVersion() <= 0 {
			continue
		}
		resolvers[identified.SiteID()] = resolver
	}
	for _, candidate := range candidates {
		if strings.TrimSpace(candidate.Resource.DownloadID) == "" {
			continue
		}
		if resolver := resolvers[candidate.Resource.SiteID]; resolver != nil {
			return candidate, resolver, true
		}
	}
	return subscription.Candidate{}, nil, false
}

func stableContextError(ctx context.Context, source, fallback error) error {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	if errors.Is(source, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(source, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return fallback
}

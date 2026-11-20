package subscriptionflow

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/moviepilot-go/moviepilot-go/internal/database"
	"github.com/moviepilot-go/moviepilot-go/internal/downloader"
	"github.com/moviepilot-go/moviepilot-go/internal/downloadflow"
	"github.com/moviepilot-go/moviepilot-go/internal/indexer"
	"github.com/moviepilot-go/moviepilot-go/internal/jobs"
	"github.com/moviepilot-go/moviepilot-go/internal/media"
	"github.com/moviepilot-go/moviepilot-go/internal/organization"
	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

const (
	rssPollInterval              = 30 * time.Minute
	rssMinimumPollInterval       = 5 * time.Minute
	rssCursorLease               = 20 * time.Minute
	rssTargetPageSize            = 100
	rssMaxParsedFeedItems        = 200
	rssMaxRefreshItems           = 50
	rssCacheItemsPerSite         = 100
	rssCacheTrimBatch            = 200
	rssMaxStoredTitle            = 1024
	rssMaxStoredLocator          = 2048
	rssMaxMediaRecognizeFailures = 3
	rssSitesSetting              = "RssSites"
)

var (
	ErrInvalidRSSHandler = errors.New("RSS 订阅处理器依赖不完整")
	ErrRSSStorage        = errors.New("RSS 订阅存储暂时不可用")
	ErrRSSFetch          = errors.New("RSS 站点请求失败")
	ErrRSSMatch          = errors.New("RSS 订阅匹配失败")
	ErrRSSSubmission     = errors.New("RSS 订阅下载提交失败")
)

// RSSStore is the bounded persistence contract consumed by RSSHandler.
type RSSStore interface {
	GetSubscriptionRSSSites(context.Context) (database.SubscriptionRSSSitesSnapshot, error)
	GetSetting(context.Context, string, any) (bool, error)
	EnsureRSSCursor(context.Context, int64, string) (database.RSSCursor, error)
	ListDueRSSCursors(context.Context, time.Time, int) ([]database.RSSCursor, error)
	TryAcquireRSSLease(context.Context, int64, string, time.Time, time.Duration) (database.RSSCursor, bool, error)
	CommitRSSPoll(context.Context, int64, string, uuid.UUID, database.RSSPollResult, time.Time) error
	ListRSSCacheSignatures(context.Context, int64) (map[string]struct{}, error)
	FailRSSPoll(context.Context, int64, string, uuid.UUID, time.Time, string) error
	RecoverExpiredRSSLeases(context.Context, time.Time, int) (int, error)
	RSSPollRoundReady(context.Context, time.Time, []int64) (bool, error)
	ListRSSCacheSnapshot(context.Context, int64, time.Time) ([]database.RSSItem, error)
	UpdateRSSItemMediaRecognition(context.Context, int64, string) error
	TrimRSSCacheItems(context.Context, int64, int) (int, error)
	ListSubscriptionsForRSS(context.Context, time.Time, time.Time, int64, int) ([]database.Subscription, error)
}

// RSSPollEnqueuer persists site polls and their global matching barrier in River.
type RSSPollEnqueuer interface {
	EnqueueRSSPoll(context.Context, jobs.RSSPollArgs) error
	EnqueueRSSMatch(context.Context, time.Time, []int) error
}

// RSSDownloadSubmission is an optional extension of the normal download
// submission contract. It allows active subscriptions to be acquired by RSS.
type RSSDownloadSubmission interface {
	SubmitRSSSubscriptionEpisodes(
		context.Context,
		string,
		int64,
		subscription.State,
		downloader.Intent,
		database.DownloadIntentPlan,
		organization.SuccessInput,
		[]subscription.Episode,
	) (uuid.UUID, error)
	SubmitRSSTargetEpisodes(
		context.Context,
		string,
		int64,
		subscription.State,
		time.Time,
		downloader.Intent,
		database.DownloadIntentPlan,
		organization.SuccessInput,
		[]subscription.Episode,
	) (uuid.UUID, error)
}

// RSSHandler connects durable cursor/item leases to the existing subscription
// candidate selector and atomic download submission service.
type RSSHandler struct {
	store           RSSStore
	subscription    *Handler
	adapters        AdapterProvider
	enqueuer        RSSPollEnqueuer
	mediaRecognizer RSSMediaRecognizer
	pollInterval    atomic.Int64
	now             func() time.Time
}

func NewRSSHandler(
	store RSSStore,
	subscriptionHandler *Handler,
	adapters AdapterProvider,
	enqueuer RSSPollEnqueuer,
	pollIntervals ...time.Duration,
) (*RSSHandler, error) {
	if store == nil || subscriptionHandler == nil || adapters == nil || enqueuer == nil {
		return nil, ErrInvalidRSSHandler
	}
	pollInterval := rssPollInterval
	if len(pollIntervals) > 1 || (len(pollIntervals) == 1 && pollIntervals[0] < rssMinimumPollInterval) {
		return nil, ErrInvalidRSSHandler
	}
	if len(pollIntervals) == 1 {
		pollInterval = pollIntervals[0]
	}
	if _, ok := subscriptionHandler.submission.(RSSDownloadSubmission); !ok {
		return nil, ErrInvalidRSSHandler
	}
	if subscriptionHandler.execution != nil {
		if _, ok := subscriptionHandler.submission.(stagedRSSTVDownloadSubmission); !ok {
			return nil, ErrInvalidRSSHandler
		}
	}
	handler := &RSSHandler{
		store:        store,
		subscription: subscriptionHandler,
		adapters:     adapters,
		enqueuer:     enqueuer,
		now:          time.Now,
	}
	handler.pollInterval.Store(int64(pollInterval))
	return handler, nil
}

// SetPollInterval updates the V2 RSS cursor interval for subsequent polls.
func (h *RSSHandler) SetPollInterval(interval time.Duration) error {
	if h == nil || interval < rssMinimumPollInterval {
		return ErrInvalidRSSHandler
	}
	h.pollInterval.Store(int64(interval))
	return nil
}

// HandleRSSSweep owns the complete non-reentrant V2 refresh round. It keeps
// the fixed-key River job active until every due poll has completed and the
// full subscription snapshot has been matched.
func (h *RSSHandler) HandleRSSSweep(
	ctx context.Context,
	cutoff time.Time,
	state jobs.RSSSweepState,
) (jobs.RSSSweepState, error) {
	if err := h.validate(ctx); err != nil {
		return state, err
	}
	if cutoff.IsZero() {
		return state, ErrRSSStorage
	}
	now := h.now().UTC()
	allSources := rssSources(h.adapters.Snapshot())
	if !state.Initialized {
		snapshot, err := h.store.GetSubscriptionRSSSites(ctx)
		if err != nil {
			return state, stableContextError(ctx, err, ErrRSSStorage)
		}
		if !snapshot.HasSubscriptions {
			return jobs.RSSSweepState{Initialized: true, SiteIDs: []int{}}, nil
		}
		defaultSiteIDs := []int{}
		if _, err := h.store.GetSetting(ctx, rssSitesSetting, &defaultSiteIDs); err != nil {
			return state, stableContextError(ctx, err, ErrRSSStorage)
		}
		selectedSiteIDs, run := v2RSSRefreshSiteIDs(snapshot, defaultSiteIDs)
		if !run {
			return jobs.RSSSweepState{Initialized: true, SiteIDs: []int{}}, nil
		}
		sources := selectedRSSSources(allSources, selectedSiteIDs, len(selectedSiteIDs) == 0)
		state = jobs.RSSSweepState{Initialized: true, SiteIDs: rssSourceIDs(sources)}
	} else if !validRSSSweepState(state) {
		return state, ErrRSSStorage
	}

	sources := selectedRSSSources(allSources, state.SiteIDs, false)
	if len(sources) == 0 {
		return state, nil
	}
	for _, source := range orderedRSSSources(sources) {
		if _, err := h.store.EnsureRSSCursor(ctx, int64(source.siteID), source.cursorKey); err != nil {
			return state, stableContextError(ctx, err, ErrRSSStorage)
		}
	}
	if _, err := h.store.RecoverExpiredRSSLeases(ctx, now, jobs.RSSPollPageSize); err != nil {
		return state, stableContextError(ctx, err, ErrRSSStorage)
	}
	cursors, err := h.store.ListDueRSSCursors(ctx, cutoff.UTC(), jobs.RSSSweepPageSize)
	if err != nil {
		return state, stableContextError(ctx, err, ErrRSSStorage)
	}
	dueSources := make([]rssSource, 0, len(cursors))
	for _, cursor := range cursors {
		source, ok := sources[int(cursor.SiteID)]
		if !ok || cursor.Endpoint != source.cursorKey {
			continue
		}
		dueSources = append(dueSources, source)
	}
	sort.Slice(dueSources, func(i, j int) bool { return dueSources[i].siteID < dueSources[j].siteID })
	for _, source := range dueSources {
		if err := h.enqueuer.EnqueueRSSPoll(ctx, jobs.RSSPollArgs{
			SiteID: source.siteID,
			Cutoff: cutoff.UTC(),
		}); err != nil {
			return state, stableContextError(ctx, err, ErrRSSStorage)
		}
	}
	expectedSiteIDs := make([]int, 0, len(sources))
	for _, source := range orderedRSSSources(sources) {
		expectedSiteIDs = append(expectedSiteIDs, source.siteID)
	}
	ready, err := h.store.RSSPollRoundReady(ctx, cutoff.UTC(), rssSiteIDs64(expectedSiteIDs))
	if err != nil {
		return state, stableContextError(ctx, err, ErrRSSStorage)
	}
	if !ready {
		return state, jobs.ErrRSSSweepPending
	}
	if err := h.matchRSSSubscriptions(ctx, cutoff.UTC(), sources); err != nil {
		return state, err
	}
	return state, h.trimRSSCaches(ctx, sources)
}

// HandleRSSPoll leases one cursor, performs the conditional HTTP request
// outside a database transaction, and commits a bounded cache update. Global
// subscription matching runs only after the sweep's poll barrier is complete.
func (h *RSSHandler) HandleRSSPoll(ctx context.Context, args jobs.RSSPollArgs) error {
	if err := h.validate(ctx); err != nil {
		return err
	}
	if args.SiteID <= 0 || args.Cutoff.IsZero() {
		return ErrRSSStorage
	}
	source, ok := rssSources(h.adapters.Snapshot())[args.SiteID]
	if !ok {
		// A site may be disabled while its durable job is still queued.
		return nil
	}
	if _, err := h.store.EnsureRSSCursor(ctx, int64(source.siteID), source.cursorKey); err != nil {
		return stableContextError(ctx, err, ErrRSSStorage)
	}
	now := h.now().UTC()
	cursor, acquired, err := h.store.TryAcquireRSSLease(
		ctx,
		int64(source.siteID),
		source.cursorKey,
		now,
		rssCursorLease,
	)
	if errors.Is(err, database.ErrRSSLeaseConflict) {
		return jobs.ErrRSSLeaseBusy
	}
	if err != nil {
		return stableContextError(ctx, err, ErrRSSStorage)
	}
	if !acquired {
		return h.trimRSSCaches(ctx, map[int]rssSource{source.siteID: source})
	}

	response, fetchErr := source.fetcher.FetchRSS(ctx, indexer.RSSRequest{
		ETag:         cursor.ETag,
		LastModified: cursor.LastModified,
		Limit:        rssMaxParsedFeedItems,
	})
	if fetchErr != nil {
		return h.failPoll(ctx, cursor, now, ErrRSSFetch)
	}
	if response.NotModified && cursor.ETag == "" && cursor.LastModified == "" {
		return h.failPoll(ctx, cursor, now, ErrRSSFetch)
	}
	response.Items = rssRefreshWindow(response.Items)
	if !response.NotModified && !source.noCache && len(response.Items) > 0 {
		cachedSignatures, signatureErr := h.store.ListRSSCacheSignatures(ctx, int64(source.siteID))
		if signatureErr != nil {
			return h.failPoll(ctx, cursor, now, ErrRSSStorage)
		}
		response.Items = filterRSSItemsByHistoricalSignatures(cachedSignatures, response.Items)
	}
	recognitionAttempted := h.mediaRecognizer != nil
	if err := h.recognizeRSSResources(ctx, response.Items); err != nil {
		return err
	}
	pollResult, err := buildRSSPollResult(cursor, response, recognitionAttempted)
	if err != nil {
		return h.failPoll(ctx, cursor, now, ErrRSSFetch)
	}
	pollResult.ReplaceCache = source.noCache && !pollResult.NotModified && len(pollResult.Items) > 0
	pollResult.ObservedAt = args.Cutoff.UTC()
	if err := h.store.CommitRSSPoll(
		ctx,
		cursor.SiteID,
		cursor.Endpoint,
		cursor.LeaseToken,
		pollResult,
		now.Add(time.Duration(h.pollInterval.Load())),
	); err != nil {
		if errors.Is(err, database.ErrRSSLeaseConflict) {
			return jobs.ErrRSSLeaseBusy
		}
		return stableContextError(ctx, err, ErrRSSStorage)
	}
	return h.trimRSSCaches(ctx, map[int]rssSource{source.siteID: source})
}

// HandleRSSMatch waits until every still-configured expected site has finished
// its poll attempt, then scans the complete R/P target snapshot. A retry starts
// from the first target; already-bound targets are excluded by the store query.
func (h *RSSHandler) HandleRSSMatch(ctx context.Context, args jobs.RSSMatchArgs) error {
	if err := h.validate(ctx); err != nil {
		return err
	}
	if args.Cutoff.IsZero() || len(args.ExpectedSiteIDs) > jobs.RSSSweepPageSize ||
		!validExpectedRSSSiteIDs(args.ExpectedSiteIDs) {
		return ErrRSSStorage
	}
	sources := rssSources(h.adapters.Snapshot())
	expectedSiteIDs := activeExpectedRSSSiteIDs(args.ExpectedSiteIDs, sources)
	ready, err := h.store.RSSPollRoundReady(ctx, args.Cutoff.UTC(), rssSiteIDs64(expectedSiteIDs))
	if err != nil {
		return stableContextError(ctx, err, ErrRSSStorage)
	}
	if !ready {
		return jobs.ErrRSSMatchBarrierPending
	}
	if err := h.matchRSSSubscriptions(ctx, args.Cutoff.UTC(), sources); err != nil {
		return err
	}
	return h.trimRSSCaches(ctx, sources)
}

func (h *RSSHandler) matchRSSSubscriptions(
	ctx context.Context,
	cutoff time.Time,
	sources map[int]rssSource,
) error {
	recognitionAttempted := make(map[int64]struct{})
	var afterUpdatedAt time.Time
	var afterID int64
	for {
		targets, err := h.store.ListSubscriptionsForRSS(
			ctx,
			cutoff,
			afterUpdatedAt,
			afterID,
			rssTargetPageSize,
		)
		if err != nil {
			return stableContextError(ctx, err, ErrRSSStorage)
		}
		if len(targets) == 0 {
			// V2 pre-processes the retained cache before scanning subscriptions;
			// keep bounded recognition retries progressing even when this round
			// has no active R/P target.
			_, err := h.loadRSSPageResources(ctx, cutoff, sources, recognitionAttempted)
			return err
		}
		pageResources, err := h.loadRSSPageResources(ctx, cutoff, sources, recognitionAttempted)
		if err != nil {
			return err
		}
		for _, target := range targets {
			if _, err := h.matchRSSTarget(ctx, sources, pageResources, target); err != nil {
				return err
			}
			afterUpdatedAt = target.UpdatedAt.UTC()
			afterID = target.ID
		}
		if len(targets) < rssTargetPageSize {
			return nil
		}
	}
}

func (h *RSSHandler) loadRSSPageResources(
	ctx context.Context,
	cutoff time.Time,
	sources map[int]rssSource,
	recognitionAttemptedInput ...map[int64]struct{},
) (map[int][]indexer.Resource, error) {
	recognitionAttempted := make(map[int64]struct{})
	if len(recognitionAttemptedInput) > 0 && recognitionAttemptedInput[0] != nil {
		recognitionAttempted = recognitionAttemptedInput[0]
	}
	resourcesBySite := make(map[int][]indexer.Resource, len(sources))
	for _, source := range orderedRSSSources(sources) {
		items, err := h.store.ListRSSCacheSnapshot(ctx, int64(source.siteID), cutoff)
		if err != nil {
			return nil, stableContextError(ctx, err, ErrRSSStorage)
		}
		resources := make([]indexer.Resource, 0, len(items))
		for _, item := range items {
			if resource, ok := restoreRSSResource(item); ok {
				if err := h.retryRSSMediaRecognition(ctx, item, &resource, recognitionAttempted); err != nil {
					return nil, err
				}
				resources = append(resources, resource)
			}
		}
		resourcesBySite[source.siteID] = resources
	}
	return resourcesBySite, nil
}

// retryRSSMediaRecognition mirrors V2's bounded cache retry: an item starts
// with one failed attempt during poll, receives at most two match-time retry
// attempts, and is skipped once the durable counter reaches three. The map is
// scoped to one match round so multiple subscription pages cannot retry the
// same resource repeatedly in one round.
func (h *RSSHandler) retryRSSMediaRecognition(
	ctx context.Context,
	item database.RSSItem,
	resource *indexer.Resource,
	attempted map[int64]struct{},
) error {
	if h == nil || h.mediaRecognizer == nil || resource == nil || item.ID < 1 ||
		strings.TrimSpace(resource.RecognizedMediaTitle) != "" ||
		item.MediaRecognizeFailCount >= rssMaxMediaRecognizeFailures {
		return nil
	}
	if _, seen := attempted[item.ID]; seen {
		return nil
	}
	attempted[item.ID] = struct{}{}
	match, found, recognizeErr := h.mediaRecognizer.Recognize(ctx, rssMetadataQuery(*resource, nil))
	if ctx != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	canonicalTitle := ""
	if recognizeErr == nil && found {
		canonicalTitle = strings.TrimSpace(match.Media.Title)
	}
	if err := h.store.UpdateRSSItemMediaRecognition(ctx, item.ID, canonicalTitle); err != nil {
		return stableContextError(ctx, err, ErrRSSStorage)
	}
	if canonicalTitle != "" {
		resource.RecognizedMediaTitle = canonicalTitle
	}
	return nil
}

func (h *RSSHandler) matchRSSTarget(
	ctx context.Context,
	sources map[int]rssSource,
	resourcesBySite map[int][]indexer.Resource,
	target database.Subscription,
) (bool, error) {
	query, matchable, err := h.prepareRSSTarget(ctx, &target)
	if err != nil || !matchable {
		return false, err
	}
	query, err = h.subscription.withV2DefaultCandidateFilters(ctx, target.Config, query)
	if err != nil {
		return false, stableContextError(ctx, err, ErrRSSStorage)
	}
	query.Limit = rssCacheItemsPerSite
	candidates := make([]subscription.Candidate, 0, rssCacheItemsPerSite)
	for _, source := range orderedRSSSources(sources) {
		if !rssQueryAllowsSite(query, source.siteID) {
			continue
		}
		cachedResources := resourcesBySite[source.siteID]
		resources := make([]indexer.Resource, 0, len(candidates)+len(cachedResources))
		for _, candidate := range candidates {
			resources = append(resources, candidate.Resource)
		}
		resources = append(resources, cachedResources...)
		candidates, err = subscription.MatchV2DownloadCandidates(query, resources)
		if err != nil {
			if errors.Is(err, subscription.ErrInvalidCandidateQuery) {
				return false, nil
			}
			return false, ErrRSSMatch
		}
		if query.MediaType == media.TypeTV {
			// V2 RSS matching rejects multi-season TV resources before selection.
			matched := candidates[:0]
			for _, candidate := range candidates {
				if len(candidate.Parsed.SeasonList()) > 1 {
					continue
				}
				matched = append(matched, candidate)
			}
			candidates = matched
		}
	}
	candidates, err = h.subscription.filterV2SubscriptionCandidates(ctx, target, candidates)
	if err != nil {
		if errors.Is(err, subscription.ErrInvalidV2FilterRules) {
			return false, ErrRSSMatch
		}
		return false, stableContextError(ctx, err, ErrRSSStorage)
	}
	if err := h.refreshRSSCandidateMediaTitles(ctx, candidates); err != nil {
		return false, stableContextError(ctx, err, ErrRSSStorage)
	}
	if fallbackTitle := h.rssTargetMediaTitle(ctx, target, query); fallbackTitle != "" {
		for index := range candidates {
			if strings.TrimSpace(candidates[index].MediaTitle) == "" {
				candidates[index].MediaTitle = fallbackTitle
				candidates[index].Resource.RecognizedMediaTitle = fallbackTitle
			}
		}
	}
	candidates, err = sortV2CandidatesFromStore(ctx, h.store, candidates)
	if err != nil {
		return false, stableContextError(ctx, err, ErrRSSStorage)
	}
	if query.TVMissingContainerEmpty {
		err := h.subscription.finishEmptyTVMissingContainer(ctx, target)
		if errors.Is(err, ErrSubscriptionTargetChanged) {
			return false, ErrRSSMatch
		}
		if err != nil {
			return false, stableContextError(ctx, err, ErrRSSStorage)
		}
		return false, nil
	}
	adapters := make([]indexer.Adapter, 0, len(sources))
	for _, source := range orderedRSSSources(sources) {
		adapters = append(adapters, source.adapter)
	}
	if query.MediaType == media.TypeTV && canExecuteV2TVCandidates(h.subscription, true) {
		result, executeErr := h.subscription.executeV2TVCandidates(
			ctx, target, query, candidates, adapters, true,
		)
		if errors.Is(executeErr, ErrSubscriptionConfiguration) {
			return false, nil
		}
		if executeErr != nil {
			return false, stableContextError(ctx, executeErr, ErrRSSSubmission)
		}
		return len(result.AcceptedCandidateKeys) > 0, nil
	}
	if query.MediaType == media.TypeMovie && canExecuteV2TVCandidates(h.subscription, true) {
		result, executeErr := h.subscription.executeV2MovieCandidates(
			ctx, target, candidates, adapters, true,
		)
		if errors.Is(executeErr, ErrSubscriptionConfiguration) {
			return false, nil
		}
		if executeErr != nil {
			return false, stableContextError(ctx, executeErr, ErrRSSSubmission)
		}
		return result.Accepted, nil
	}
	selected, resolver, ok := firstDownloadableCandidate(candidates, adapters)
	if !ok || (query.MediaType == media.TypeTV && len(selected.CoveredEpisodes) == 0) {
		if query.TVRecoveredTarget != nil {
			if err := h.subscription.finishRecoveredTVTarget(ctx, target, len(query.MissingEpisodes)); err != nil && !errors.Is(err, ErrSubscriptionTargetChanged) {
				return false, stableContextError(ctx, err, ErrRSSStorage)
			}
		}
		return false, nil
	}
	return h.submitRSSCandidate(ctx, target, selected, resolver, query)
}

func (h *RSSHandler) prepareRSSTarget(
	ctx context.Context,
	prepared *database.Subscription,
) (subscription.Query, bool, error) {
	target := *prepared
	var tvTarget subscription.TVTargetResolution
	var tvRead tvSearchReadState
	if media.MediaType(strings.TrimSpace(target.MediaType)) == media.TypeTV {
		var refreshErr error
		var refreshed persistedSubscription
		refreshed, tvTarget, tvRead, refreshErr = h.subscription.prepareTVSearchTarget(ctx, target)
		if errors.Is(refreshErr, ErrSubscriptionTargetChanged) {
			return subscription.Query{}, false, nil
		}
		if errors.Is(refreshErr, ErrSubscriptionConfiguration) || errors.Is(refreshErr, database.ErrInvalidSubscriptionTarget) {
			return subscription.Query{}, false, nil
		}
		if refreshErr != nil {
			return subscription.Query{}, false, stableContextError(ctx, refreshErr, ErrRSSStorage)
		}
		target = refreshed
		*prepared = refreshed
		if tvRead.InitialRecognitionFailed || tvRead.UnresolvedSeasonFacts {
			return subscription.Query{}, false, nil
		}
		if tvRead.PreflightSatisfied {
			err := h.subscription.finishEmptyTVMissingContainer(ctx, target)
			if errors.Is(err, ErrSubscriptionTargetChanged) {
				return subscription.Query{}, false, ErrRSSMatch
			}
			if err != nil {
				return subscription.Query{}, false, stableContextError(ctx, err, ErrRSSStorage)
			}
			return subscription.Query{}, false, nil
		}
	}
	query, _, err := candidateQuery(target)
	if err != nil {
		return subscription.Query{}, false, nil
	}
	if query.MediaType == media.TypeTV {
		var completable, matchable bool
		query.TVMediaTitle = tvRead.InitialMediaTitle
		query.TVMediaRecognized = tvRead.InitialRecognized
		if tvRead.MissingContainerEmpty {
			query.MissingEpisodes = nil
			query.FullSeasonMissing = nil
			query.TVMissingContainerEmpty = true
			matchable = true
		} else if tvRead.RecoveredTotal > 0 {
			query, err = h.subscription.withRecoveredTVMissingEpisodes(ctx, target, tvTarget, tvRead.RecoveredTotal, query, tvRead.RecoveredLibraryQuery)
			completable, matchable = true, true
		} else {
			query, completable, matchable, err = h.subscription.withCurrentTVMissingEpisodes(
				ctx, target, tvTarget, tvRead.LegacyWithoutTotal, query,
			)
		}
		if err != nil {
			if errors.Is(err, database.ErrInvalidSubscriptionTarget) {
				return subscription.Query{}, false, nil
			}
			return subscription.Query{}, false, stableContextError(ctx, err, ErrRSSStorage)
		}
		if !matchable {
			return subscription.Query{}, false, nil
		}
		if len(query.MissingEpisodes) == 0 && !query.TVMissingContainerEmpty {
			if !completable {
				return subscription.Query{}, false, nil
			}
			if query.TVRecoveredTarget != nil {
				if err := h.subscription.finishRecoveredTVTarget(ctx, target, 0); err != nil {
					if errors.Is(err, ErrSubscriptionTargetChanged) {
						return subscription.Query{}, false, ErrRSSMatch
					}
					return subscription.Query{}, false, stableContextError(ctx, err, ErrRSSStorage)
				}
				return subscription.Query{}, false, nil
			}
			if target.InternalState == subscription.PendingIdentification {
				if !subscriptionBestVersionEnabled(target.Config) {
					if err := h.subscription.finishEmptyTVMissingContainer(ctx, target); err != nil {
						if errors.Is(err, ErrSubscriptionTargetChanged) {
							return subscription.Query{}, false, ErrRSSMatch
						}
						return subscription.Query{}, false, stableContextError(ctx, err, ErrRSSStorage)
					}
				}
				return subscription.Query{}, false, nil
			}
			if err := h.subscription.finishSatisfiedTarget(ctx, target, target.InternalState); err != nil {
				if errors.Is(err, ErrSubscriptionTargetChanged) {
					return subscription.Query{}, false, ErrRSSMatch
				}
				return subscription.Query{}, false, stableContextError(ctx, err, ErrRSSStorage)
			}
			return subscription.Query{}, false, nil
		}
	}
	if query.MediaType == media.TypeMovie {
		satisfied, lookupErr := h.subscription.existingMovieSatisfied(
			ctx, target.Name, target.Year, target.MediaSource, target.MediaID, target.Config,
		)
		if lookupErr != nil {
			return subscription.Query{}, false, stableContextError(ctx, lookupErr, ErrRSSStorage)
		}
		if satisfied {
			// V2 checks the library for P subscriptions but __finish_subscribe
			// refuses to complete them until identification has settled.
			if target.InternalState == subscription.PendingIdentification {
				return subscription.Query{}, false, nil
			}
			if err := h.subscription.finishSatisfiedMovieTarget(ctx, target, target.InternalState); err != nil {
				if errors.Is(err, ErrSubscriptionTargetChanged) {
					return subscription.Query{}, false, ErrRSSMatch
				}
				return subscription.Query{}, false, stableContextError(ctx, err, ErrRSSStorage)
			}
			return subscription.Query{}, false, nil
		}
	}
	// Refined R states with in-flight or post-download work are still visible
	// to the V2 completion check, but only Active/R and PendingIdentification/P
	// may claim a new intent in the single-intent Go model.
	if target.InternalState != subscription.Active &&
		target.InternalState != subscription.PendingIdentification {
		return subscription.Query{}, false, nil
	}
	return query, true, nil
}

func (h *RSSHandler) submitRSSCandidate(
	ctx context.Context,
	target database.Subscription,
	selected subscription.Candidate,
	resolver indexer.VersionedDownloadResolver,
	query subscription.Query,
) (bool, error) {
	configuredDownloader, savePath, err := h.subscription.downloadSettings(ctx, target.Config)
	if err != nil {
		if errors.Is(err, ErrSubscriptionConfiguration) {
			return false, nil
		}
		return false, err
	}
	resourceID, resourceSecret, err := downloadflow.ProtectResourceLocator(selected.Resource.DownloadID)
	if err != nil {
		return false, nil
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
		return false, ErrRSSSubmission
	}
	intent, err := downloader.NewIntent(target.Username, downloadCandidate)
	if err != nil {
		return false, ErrRSSSubmission
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
	submission := h.subscription.submission.(RSSDownloadSubmission)
	if _, err := submission.SubmitRSSTargetEpisodes(
		ctx,
		target.Username,
		target.ID,
		target.InternalState,
		target.UpdatedAt,
		intent,
		plan,
		h.subscription.buildSubscriptionSuccessInput(ctx, subscriptionSuccessTarget{
			Username: target.Username, Name: target.Name, Year: target.Year,
			MediaType: target.MediaType, MediaSource: target.MediaSource,
			MediaID: target.MediaID, VoteAverage: subscriptionConfigVote(target.Config),
			PosterURL: configText(target.Config, "poster"), BackdropURL: configText(target.Config, "backdrop"),
			Config: target.Config,
		}, selected, intent.ID),
		selected.CoveredEpisodes,
	); err != nil {
		return false, stableContextError(ctx, err, ErrRSSSubmission)
	}
	return true, nil
}

func (h *RSSHandler) trimRSSCaches(ctx context.Context, sources map[int]rssSource) error {
	for _, source := range orderedRSSSources(sources) {
		for {
			deleted, err := h.store.TrimRSSCacheItems(
				ctx,
				int64(source.siteID),
				rssCacheTrimBatch,
			)
			if err != nil {
				return stableContextError(ctx, err, ErrRSSStorage)
			}
			if deleted < rssCacheTrimBatch {
				break
			}
		}
	}
	return nil
}

func (h *RSSHandler) failPoll(ctx context.Context, cursor database.RSSCursor, now time.Time, reason error) error {
	if contextErr := stableContextError(ctx, reason, reason); errors.Is(contextErr, context.Canceled) || errors.Is(contextErr, context.DeadlineExceeded) {
		return contextErr
	}
	retryAt := now.Add(rssFailureBackoff(cursor.FailureCount))
	if err := h.store.FailRSSPoll(
		ctx,
		cursor.SiteID,
		cursor.Endpoint,
		cursor.LeaseToken,
		retryAt,
		reason.Error(),
	); err != nil {
		if errors.Is(err, database.ErrRSSLeaseConflict) {
			return jobs.ErrRSSLeaseBusy
		}
		return stableContextError(ctx, err, ErrRSSStorage)
	}
	return errors.Join(reason, jobs.ErrRSSPollFailureRecorded)
}

func (h *RSSHandler) validate(ctx context.Context) error {
	if ctx == nil || h == nil || h.store == nil || h.subscription == nil || h.adapters == nil || h.enqueuer == nil || h.now == nil {
		return ErrInvalidRSSHandler
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}

func validRSSSweepState(state jobs.RSSSweepState) bool {
	if !state.Initialized || state.SiteIDs == nil || len(state.SiteIDs) > jobs.RSSSweepPageSize {
		return false
	}
	for index, siteID := range state.SiteIDs {
		if siteID <= 0 || (index > 0 && state.SiteIDs[index-1] >= siteID) {
			return false
		}
	}
	return true
}

func v2RSSRefreshSiteIDs(
	snapshot database.SubscriptionRSSSitesSnapshot,
	defaultSiteIDs []int,
) ([]int, bool) {
	if !snapshot.HasSubscriptions {
		return nil, false
	}

	defaults := normalizedRSSSiteIDs(defaultSiteIDs)
	selected := make(map[int]struct{})
	for _, configured := range snapshot.SiteSelections {
		userSiteIDs := normalizedRSSSiteIDs(configured)
		effective := userSiteIDs
		switch {
		case len(userSiteIDs) == 0:
			effective = defaults
		case len(defaults) == 0:
		case rssSiteIDIntersection(userSiteIDs, defaults, selected):
			continue
		default:
			effective = defaults
		}
		for _, siteID := range effective {
			selected[siteID] = struct{}{}
		}
	}
	if len(selected) == 0 {
		for _, siteID := range defaults {
			selected[siteID] = struct{}{}
		}
	}

	result := make([]int, 0, len(selected))
	for siteID := range selected {
		result = append(result, siteID)
	}
	sort.Ints(result)
	if result == nil {
		return []int{}, true
	}
	return result, true
}

func buildRSSPollResult(cursor database.RSSCursor, response indexer.RSSResponse, recognitionAttempted ...bool) (database.RSSPollResult, error) {
	result := database.RSSPollResult{
		NotModified:  response.NotModified,
		ETag:         response.ETag,
		LastModified: response.LastModified,
		Items:        make([]database.RSSItemInput, 0, len(response.Items)),
	}
	if result.ETag == "" {
		result.ETag = cursor.ETag
	}
	if result.LastModified == "" {
		result.LastModified = cursor.LastModified
	}
	if response.NotModified {
		return result, nil
	}
	items := rssRefreshWindow(response.Items)
	attempted := len(recognitionAttempted) > 0 && recognitionAttempted[0]
	for _, item := range items {
		resource := item.Resource
		resource.ItemKey = ""
		downloadID := strings.TrimSpace(resource.DownloadID)
		if downloadID == "" {
			downloadID = strings.TrimSpace(resource.DownloadURL)
		}
		resourceID, resourceSecret, protectErr := downloadflow.ProtectResourceLocator(downloadID)
		if protectErr != nil {
			continue
		}
		resource.DetailURL = sanitizeRSSPublicURL(resource.DetailURL)
		resource.DownloadURL = sanitizeRSSPublicURL(resource.DownloadURL)
		if item.ItemKey == "" || resource.Title == "" || len(resource.Title) > rssMaxStoredTitle {
			continue
		}
		if len(downloadID) > rssMaxStoredLocator || len(resource.DetailURL) > rssMaxStoredLocator || len(resource.DownloadURL) > rssMaxStoredLocator {
			continue
		}
		encoded, err := marshalRSSResource(resource)
		if err != nil || len(encoded) > 1<<20 {
			continue
		}
		var publishedAt *time.Time
		if !resource.PublishedAt.IsZero() {
			published := resource.PublishedAt.UTC()
			publishedAt = &published
		}
		failCount := 0
		if attempted && strings.TrimSpace(resource.RecognizedMediaTitle) == "" {
			failCount = 1
		}
		result.Items = append(result.Items, database.RSSItemInput{
			ItemKey:                 item.ItemKey,
			DownloadID:              resourceID,
			DetailURL:               resource.DetailURL,
			DownloadURL:             resource.DownloadURL,
			ResourceSecret:          resourceSecret,
			Title:                   resource.Title,
			RecognizedMediaTitle:    strings.TrimSpace(resource.RecognizedMediaTitle),
			MediaRecognizeFailCount: failCount,
			PublishedAt:             publishedAt,
			Resource:                encoded,
		})
	}
	return result, nil
}

func rssRefreshWindow(input []indexer.RSSItem) []indexer.RSSItem {
	items := append([]indexer.RSSItem(nil), input...)
	sort.SliceStable(items, func(i, j int) bool {
		return items[i].Resource.PublishedAt.After(items[j].Resource.PublishedAt)
	})
	if len(items) > rssMaxRefreshItems {
		items = items[:rssMaxRefreshItems]
	}
	return items
}

func filterRSSItemsByHistoricalSignatures(cached map[string]struct{}, items []indexer.RSSItem) []indexer.RSSItem {
	if len(cached) == 0 || len(items) == 0 {
		return items
	}
	filtered := make([]indexer.RSSItem, 0, len(items))
	for _, item := range items {
		signature := item.Resource.Title + item.Resource.Description
		if _, exists := cached[signature]; !exists {
			filtered = append(filtered, item)
		}
	}
	return filtered
}

// rssResourceSnapshot keeps the V2 context title inside the existing bounded
// resource JSON without exposing it through the public indexer model/API.
type rssResourceSnapshot struct {
	indexer.Resource
	V2MediaTitle string `json:"_v2_media_title,omitempty"`
}

func marshalRSSResource(resource indexer.Resource) ([]byte, error) {
	return json.Marshal(rssResourceSnapshot{
		Resource:     resource,
		V2MediaTitle: resource.RecognizedMediaTitle,
	})
}

func restoreRSSResource(item database.RSSItem) (indexer.Resource, bool) {
	var snapshot rssResourceSnapshot
	if len(item.Resource) == 0 || json.Unmarshal(item.Resource, &snapshot) != nil {
		return indexer.Resource{}, false
	}
	resource := snapshot.Resource
	resource.RecognizedMediaTitle = snapshot.V2MediaTitle
	if strings.TrimSpace(item.RecognizedMediaTitle) != "" {
		resource.RecognizedMediaTitle = strings.TrimSpace(item.RecognizedMediaTitle)
	}
	if strings.TrimSpace(item.RecognizedMediaTitle) != "" {
		resource.RecognizedMediaTitle = strings.TrimSpace(item.RecognizedMediaTitle)
	}
	resource.SiteID = int(item.SiteID)
	resource.ItemKey = item.ItemKey
	resource.Title = strings.TrimSpace(item.Title)
	resource.DetailURL = item.DetailURL
	resource.DownloadURL = item.DownloadURL
	resource.DownloadID = strings.TrimSpace(item.ResourceSecret)
	if resource.DownloadID == "" {
		resource.DownloadID = strings.TrimSpace(item.DownloadID)
	}
	if resource.DownloadID == "" {
		resource.DownloadID = strings.TrimSpace(resource.DownloadURL)
	}
	if resource.SiteID <= 0 || resource.ItemKey == "" || resource.Title == "" || resource.DownloadID == "" {
		return indexer.Resource{}, false
	}
	return resource, true
}

func sanitizeRSSPublicURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	parsed.User = nil
	parsed.RawQuery = ""
	parsed.ForceQuery = false
	parsed.Fragment = ""
	return parsed.String()
}

func rssFailureBackoff(failures int) time.Duration {
	if failures < 0 {
		failures = 0
	}
	if failures > 6 {
		failures = 6
	}
	return min(30*time.Second*time.Duration(1<<failures), 30*time.Minute)
}

var (
	_ jobs.RSSSweepHandler = (*RSSHandler)(nil)
	_ jobs.RSSPollHandler  = (*RSSHandler)(nil)
	_ jobs.RSSMatchHandler = (*RSSHandler)(nil)
)

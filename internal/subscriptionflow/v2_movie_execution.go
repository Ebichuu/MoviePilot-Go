package subscriptionflow

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/moviepilot-go/moviepilot-go/internal/downloadconfig"
	"github.com/moviepilot-go/moviepilot-go/internal/downloader"
	"github.com/moviepilot-go/moviepilot-go/internal/downloadflow"
	"github.com/moviepilot-go/moviepilot-go/internal/indexer"
	"github.com/moviepilot-go/moviepilot-go/internal/organization"
	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
	"github.com/moviepilot-go/moviepilot-go/internal/torrentmeta"
)

type v2MovieExecutionResult struct {
	Accepted bool
	Stopped  bool
}

type v2MovieCandidateRecord struct {
	candidate   subscription.Candidate
	resolver    indexer.VersionedDownloadResolver
	fingerprint string
}

type v2MovieExecutionSession struct {
	handler  *Handler
	target   persistedSubscription
	records  []v2MovieCandidateRecord
	active   map[string]struct{}
	failures v2DownloadFailureStore
	source   string
	rss      bool

	contextKey     []byte
	settingsLoaded bool
	downloader     downloadconfig.Downloader
	savePath       string
}

func (h *Handler) executeV2MovieCandidates(
	ctx context.Context,
	target persistedSubscription,
	candidates []subscription.Candidate,
	adapters []indexer.Adapter,
	rss bool,
) (v2MovieExecutionResult, error) {
	candidates, err := filterMovieUpgradeCandidates(target, candidates)
	if err != nil {
		return v2MovieExecutionResult{}, err
	}
	session := newV2MovieExecutionSession(ctx, h, target, candidates, adapters, rss)
	if len(session.records) == 0 {
		return v2MovieExecutionResult{}, nil
	}

	for _, record := range session.records {
		if _, blocked := session.active[record.fingerprint]; blocked && record.fingerprint != "" {
			continue
		}
		if err := session.loadDownloadSettings(ctx); err != nil {
			return v2MovieExecutionResult{}, err
		}
		resolved, resolveErr := record.resolver.ResolveDownload(ctx, record.candidate.Resource.DownloadID)
		if resolveErr != nil {
			if contextErr := v2MovieContextError(ctx, resolveErr); contextErr != nil {
				return v2MovieExecutionResult{}, contextErr
			}
			session.recordFailure(ctx, record, "下载种子内容为空")
			continue
		}
		if len(resolved.Body) == 0 {
			session.recordFailure(ctx, record, "下载种子内容为空")
			continue
		}
		if _, inspectErr := torrentmeta.Inspect(resolved.Body); inspectErr != nil {
			session.recordFailure(ctx, record, "无法读取种子文件")
			continue
		}
		if stop, currentErr := session.verifyBeforeStage(ctx); currentErr != nil || stop {
			return v2MovieExecutionResult{Stopped: stop}, currentErr
		}

		intentID, stageable, stageErr := session.stage(ctx, record)
		if stageErr != nil {
			if errors.Is(stageErr, downloadflow.ErrDownloadPlanConflict) {
				continue
			}
			return v2MovieExecutionResult{}, stageErr
		}
		if !stageable {
			continue
		}
		execution, executeErr := h.execution.ExecuteIntentWithMetainfo(
			ctx, target.Username, intentID, resolved.Body,
		)
		if executeErr != nil {
			if contextErr := v2MovieContextError(ctx, executeErr); contextErr != nil {
				return v2MovieExecutionResult{}, contextErr
			}
			return v2MovieExecutionResult{}, executeErr
		}
		switch execution.Outcome {
		case downloadflow.IntentExecutionAccepted:
			return v2MovieExecutionResult{Accepted: true}, nil
		case downloadflow.IntentExecutionDefinitiveFailureRecovered:
			session.recordFailure(ctx, record, "添加下载任务失败")
			if stop, refreshErr := session.refreshAfterRecoveredAttempt(ctx); refreshErr != nil || stop {
				return v2MovieExecutionResult{Stopped: stop}, refreshErr
			}
		case downloadflow.IntentExecutionPermanentCandidateFailure:
			if stop, refreshErr := session.refreshAfterRecoveredAttempt(ctx); refreshErr != nil || stop {
				return v2MovieExecutionResult{Stopped: stop}, refreshErr
			}
			continue
		case downloadflow.IntentExecutionPendingReconciliation:
			return v2MovieExecutionResult{Stopped: true}, nil
		case downloadflow.IntentExecutionRetryableFailure:
			return v2MovieExecutionResult{}, ErrSubscriptionSubmission
		default:
			return v2MovieExecutionResult{}, ErrSubscriptionSubmission
		}
	}
	return v2MovieExecutionResult{}, nil
}

func newV2MovieExecutionSession(
	ctx context.Context,
	handler *Handler,
	target persistedSubscription,
	candidates []subscription.Candidate,
	adapters []indexer.Adapter,
	rss bool,
) *v2MovieExecutionSession {
	session := &v2MovieExecutionSession{
		handler: handler, target: target, active: make(map[string]struct{}),
		source: v2SubscriptionSource(target), rss: rss,
	}
	session.contextKey, _ = v2MovieExecutionContextKey(target)
	if store, ok := handler.store.(v2DownloadFailureStore); ok {
		session.failures = store
	}
	resolvers := make(map[int]indexer.VersionedDownloadResolver, len(adapters))
	for _, adapter := range adapters {
		identified, identifiedOK := adapter.(indexer.SiteIDAdapter)
		resolver, resolverOK := adapter.(indexer.VersionedDownloadResolver)
		if !identifiedOK || !resolverOK || strings.TrimSpace(resolver.ResolverKind()) == "" ||
			resolver.ResolverVersion() <= 0 {
			continue
		}
		resolvers[identified.SiteID()] = resolver
	}
	fingerprints := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		resolver := resolvers[candidate.Resource.SiteID]
		if resolver == nil || strings.TrimSpace(candidate.Resource.DownloadID) == "" {
			continue
		}
		failure, _ := subscription.NewV2DownloadFailure(
			v2DownloadFailureTarget(target), candidate, nil, "", session.source, "",
		)
		session.records = append(session.records, v2MovieCandidateRecord{
			candidate: candidate, resolver: resolver, fingerprint: failure.Fingerprint,
		})
		if failure.Fingerprint != "" {
			fingerprints = append(fingerprints, failure.Fingerprint)
		}
	}
	if session.failures == nil || len(fingerprints) == 0 {
		return session
	}
	active, err := session.failures.ListActiveV2DownloadFailureFingerprints(ctx, fingerprints, time.Now())
	if err != nil {
		slog.WarnContext(ctx, "查询 V2 电影下载失败冷却失败，按原版放行")
		return session
	}
	if active != nil {
		session.active = active
	}
	return session
}

func (s *v2MovieExecutionSession) stage(
	ctx context.Context,
	record v2MovieCandidateRecord,
) (uuid.UUID, bool, error) {
	resourceID, resourceSecret, err := downloadflow.ProtectResourceLocator(record.candidate.Resource.DownloadID)
	if err != nil {
		return uuid.Nil, false, nil
	}
	reference := downloadflow.SourceReference{
		SiteID: int64(record.candidate.Resource.SiteID), ResolverKind: record.resolver.ResolverKind(),
		ResolverVersion: record.resolver.ResolverVersion(), ResourceID: resourceID,
		DownloaderID: s.downloader.ID,
	}
	downloadCandidate, err := reference.Candidate(s.savePath, false)
	if err != nil {
		return uuid.Nil, false, nil
	}
	intent, err := downloader.NewIntent(s.target.Username, downloadCandidate)
	if err != nil {
		return uuid.Nil, false, nil
	}
	plan := persistedDownloadIntentPlan{
		IntentID: intent.ID, SiteID: int64(record.candidate.Resource.SiteID),
		DownloaderID: s.downloader.ID, ResolverKind: record.resolver.ResolverKind(),
		ResolverVersion: record.resolver.ResolverVersion(), ResourceID: resourceID,
		ResourceSecret: resourceSecret, SavePath: s.savePath,
		MoviePriority: movieCandidatePriority(s.target, record.candidate),
	}
	input := s.handler.buildSubscriptionSuccessInput(
		ctx,
		s.successTarget(),
		record.candidate,
		intent.ID,
	)
	storedID, err := s.persistStage(ctx, intent, plan, input)
	if errors.Is(err, downloadflow.ErrDownloadPlanConflict) {
		return uuid.Nil, false, err
	}
	if err != nil || storedID != intent.ID {
		return uuid.Nil, false, stableContextError(ctx, err, ErrSubscriptionSubmission)
	}
	return storedID, true, nil
}

func (s *v2MovieExecutionSession) persistStage(
	ctx context.Context,
	intent downloader.Intent,
	plan persistedDownloadIntentPlan,
	input organization.SuccessInput,
) (uuid.UUID, error) {
	if s.rss && (s.target.InternalState == subscription.PendingIdentification || s.target.InternalState == subscription.Active) {
		stager := s.handler.submission.(stagedRSSTVDownloadSubmission)
		return stager.StageRSSTargetEpisodes(
			ctx, s.target.Username, s.target.ID, s.target.InternalState, s.target.UpdatedAt,
			intent, plan, input, nil,
		)
	}
	stager := s.handler.submission.(stagedTVDownloadSubmission)
	return stager.StageSubscriptionTargetEpisodes(
		ctx, s.target.Username, s.target.ID, s.target.UpdatedAt, intent, plan, input, nil,
	)
}

// A recovered attempt changes the version and, except for P, returns to N.
// Keep the original media/config snapshot so a control or rule update cannot
// authorize the next candidate from an obsolete search result.
func (s *v2MovieExecutionSession) refreshAfterRecoveredAttempt(ctx context.Context) (bool, error) {
	current, stop, err := s.currentTarget(ctx)
	if err != nil || stop {
		return stop, err
	}
	expected := subscription.PendingSearch
	if s.target.InternalState == subscription.PendingIdentification {
		expected = subscription.PendingIdentification
	}
	if current.InternalState != expected {
		return true, nil
	}
	projected, _ := subscription.V2State(expected)
	if current.V2State != projected {
		return true, nil
	}
	s.target = current
	return false, nil
}

func (s *v2MovieExecutionSession) verifyBeforeStage(ctx context.Context) (bool, error) {
	current, stop, err := s.currentTarget(ctx)
	if err != nil || stop {
		return stop, err
	}
	allowed := current.InternalState == subscription.PendingSearch ||
		(s.rss && (current.InternalState == subscription.Active || current.InternalState == subscription.PendingIdentification))
	return !allowed || current.InternalState != s.target.InternalState || current.V2State != s.target.V2State ||
		!current.UpdatedAt.Equal(s.target.UpdatedAt), nil
}

func (s *v2MovieExecutionSession) currentTarget(ctx context.Context) (persistedSubscription, bool, error) {
	current, err := s.handler.store.GetActiveSubscription(ctx, s.target.Username, s.target.ID)
	if subscriptionTargetNotFound(err) {
		return persistedSubscription{}, true, nil
	}
	if err != nil {
		return persistedSubscription{}, false, stableContextError(ctx, err, ErrSubscriptionSearchStorage)
	}
	key, err := v2MovieExecutionContextKey(current)
	if err != nil || len(s.contextKey) == 0 {
		return persistedSubscription{}, false, ErrSubscriptionSearchStorage
	}
	if current.CompletedAt != nil || !bytes.Equal(s.contextKey, key) {
		return persistedSubscription{}, true, nil
	}
	return current, false, nil
}

func v2MovieExecutionContextKey(target persistedSubscription) ([]byte, error) {
	target.InternalState = ""
	target.V2State = ""
	target.CompletedAt = nil
	target.CreatedAt = time.Time{}
	target.UpdatedAt = time.Time{}
	return json.Marshal(target)
}

func (s *v2MovieExecutionSession) recordFailure(
	ctx context.Context,
	record v2MovieCandidateRecord,
	errorMessage string,
) {
	failure, ok := subscription.NewV2DownloadFailure(
		v2DownloadFailureTarget(s.target), record.candidate, nil,
		s.downloader.Name, s.source, errorMessage,
	)
	if !ok {
		return
	}
	s.active[failure.Fingerprint] = struct{}{}
	if s.failures == nil {
		return
	}
	if err := s.failures.RecordV2DownloadFailure(
		ctx, failure, time.Now(), subscription.V2DownloadFailureTTL(errorMessage),
	); err != nil {
		slog.WarnContext(ctx, "记录 V2 电影下载失败冷却失败，按原版继续")
	}
}

func (s *v2MovieExecutionSession) successTarget() subscriptionSuccessTarget {
	return subscriptionSuccessTarget{
		Username: s.target.Username, Name: s.target.Name, Year: s.target.Year,
		MediaType: s.target.MediaType, MediaSource: s.target.MediaSource,
		MediaID: s.target.MediaID, VoteAverage: subscriptionConfigVote(s.target.Config),
		PosterURL:   configText(s.target.Config, "poster"),
		BackdropURL: configText(s.target.Config, "backdrop"), Config: s.target.Config,
	}
}

func (s *v2MovieExecutionSession) loadDownloadSettings(ctx context.Context) error {
	if s.settingsLoaded {
		return nil
	}
	configuredDownloader, savePath, err := s.handler.downloadSettings(ctx, s.target.Config)
	if err != nil {
		return err
	}
	s.downloader = configuredDownloader
	s.savePath = savePath
	s.settingsLoaded = true
	return nil
}

func v2MovieContextError(ctx context.Context, err error) error {
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

package subscriptionflow

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/moviepilot-go/moviepilot-go/internal/downloadconfig"
	"github.com/moviepilot-go/moviepilot-go/internal/downloader"
	"github.com/moviepilot-go/moviepilot-go/internal/downloadflow"
	"github.com/moviepilot-go/moviepilot-go/internal/indexer"
	"github.com/moviepilot-go/moviepilot-go/internal/media"
	"github.com/moviepilot-go/moviepilot-go/internal/organization"
	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
	"github.com/moviepilot-go/moviepilot-go/internal/torrentmeta"
)

// DownloadIntentExecution is the consumer-owned synchronous execution contract
// used only by automatic V2 TV candidate loops.
type DownloadIntentExecution interface {
	ExecuteIntent(context.Context, string, uuid.UUID) (downloadflow.IntentExecutionResult, error)
	ExecuteIntentWithMetainfo(context.Context, string, uuid.UUID, []byte) (downloadflow.IntentExecutionResult, error)
}

type stagedTVDownloadSubmission interface {
	StageSubscriptionEpisodes(
		context.Context,
		string,
		int64,
		downloader.Intent,
		persistedDownloadIntentPlan,
		organization.SuccessInput,
		[]subscription.Episode,
	) (uuid.UUID, error)
	StageSubscriptionTargetEpisodes(
		context.Context,
		string,
		int64,
		time.Time,
		downloader.Intent,
		persistedDownloadIntentPlan,
		organization.SuccessInput,
		[]subscription.Episode,
	) (uuid.UUID, error)
}

type stagedRSSTVDownloadSubmission interface {
	StageRSSSubscriptionEpisodes(
		context.Context,
		string,
		int64,
		subscription.State,
		downloader.Intent,
		persistedDownloadIntentPlan,
		organization.SuccessInput,
		[]subscription.Episode,
	) (uuid.UUID, error)
	StageRSSTargetEpisodes(
		context.Context,
		string,
		int64,
		subscription.State,
		time.Time,
		downloader.Intent,
		persistedDownloadIntentPlan,
		organization.SuccessInput,
		[]subscription.Episode,
	) (uuid.UUID, error)
}

type v2TVCandidateRecord struct {
	candidate subscription.Candidate
	resolver  indexer.VersionedDownloadResolver
}

type v2TVProjectedTorrent struct {
	body     []byte
	episodes []subscription.Episode
	failed   bool
}

var v2TVStandaloneFileEpisode = regexp.MustCompile(`(?i)(?:^|[^A-Za-z0-9])(E\d{1,4}(?:\s*-\s*E?\d{1,4})?)(?:$|[^A-Za-z0-9])`)

type v2TVExecutionSession struct {
	handler   *Handler
	target    persistedSubscription
	records   map[string]v2TVCandidateRecord
	projected map[string]v2TVProjectedTorrent
	failures  *v2TVDownloadFailureSession
	rss       bool
	recovered *subscription.TVRecoveredTarget

	rssReadyStaged bool
	settingsLoaded bool
	downloader     downloadconfig.Downloader
	savePath       string
}

func (h *Handler) executeV2TVCandidates(
	ctx context.Context,
	target persistedSubscription,
	query subscription.Query,
	candidates []subscription.Candidate,
	adapters []indexer.Adapter,
	rss bool,
) (subscription.V2TVSelectionResult, error) {
	if subscriptionBestVersionEnabled(target.Config) {
		return h.executeV2TVBestVersionCandidates(ctx, target, candidates, adapters, rss)
	}
	request, records, err := buildV2TVSelectionRequest(target, query, candidates, adapters)
	if err != nil {
		return subscription.V2TVSelectionResult{}, err
	}
	failures, availableCandidates := newV2TVDownloadFailureSession(
		ctx, h.store, target, records, request.Candidates,
	)
	request.Candidates = availableCandidates
	session := &v2TVExecutionSession{
		handler: h, target: target, records: records,
		projected: make(map[string]v2TVProjectedTorrent, len(records)), failures: failures, rss: rss,
	}
	if query.TVRecoveredTarget != nil {
		session.recovered = &subscription.TVRecoveredTarget{
			TotalEpisode:    query.TVRecoveredTarget.TotalEpisode,
			MissingEpisodes: append([]subscription.Episode{}, query.TVRecoveredTarget.MissingEpisodes...),
		}
	}
	result, err := subscription.SelectV2TVCandidates(ctx, request, session.project, session.attempt)
	if err != nil {
		return result, err
	}
	if session.recovered != nil {
		// Acceptance permits the next serial candidate. The outer loop finally
		// moves N to R and persists lefts, even with no accepted candidate.
		alreadyCompleted := len(result.AcceptedCandidateKeys) > 0 && len(result.RemainingEpisodes) == 0 && session.target.InternalState != subscription.PendingIdentification
		if !result.Stopped && !alreadyCompleted {
			if err := h.finishRecoveredTVTarget(ctx, session.target, len(result.RemainingEpisodes)); err != nil && !errors.Is(err, ErrSubscriptionTargetChanged) {
				return result, err
			}
		}
		return result, nil
	}
	if result.Stopped || result.SubscriptionState == subscription.V2TVSubscriptionStatePending ||
		len(result.RemainingEpisodes) == 0 {
		return result, nil
	}
	if err := h.finishTarget(ctx, target, subscription.Active); err != nil {
		return result, err
	}
	return result, nil
}

func buildV2TVSelectionRequest(
	target persistedSubscription,
	query subscription.Query,
	candidates []subscription.Candidate,
	adapters []indexer.Adapter,
) (subscription.V2TVSelectionRequest, map[string]v2TVCandidateRecord, error) {
	state := subscription.V2TVSubscriptionStateReady
	if target.InternalState == subscription.PendingIdentification {
		state = subscription.V2TVSubscriptionStatePending
	}
	projected, records := downloadableV2TVCandidates(candidates, adapters)
	needs, err := v2TVSeasonNeeds(target, query)
	if err != nil {
		return subscription.V2TVSelectionRequest{}, nil, err
	}
	return subscription.V2TVSelectionRequest{
		SubscriptionState: state,
		Seasons:           needs,
		Candidates:        projected,
	}, records, nil
}

func downloadableV2TVCandidates(
	candidates []subscription.Candidate,
	adapters []indexer.Adapter,
) ([]subscription.V2TVCandidate, map[string]v2TVCandidateRecord) {
	resolvers := make(map[int]indexer.VersionedDownloadResolver, len(adapters))
	for _, adapter := range adapters {
		identified, identifiedOK := adapter.(indexer.SiteIDAdapter)
		resolver, resolverOK := adapter.(indexer.VersionedDownloadResolver)
		if identifiedOK && resolverOK && identified.SiteID() > 0 &&
			strings.TrimSpace(resolver.ResolverKind()) != "" && resolver.ResolverVersion() > 0 {
			resolvers[identified.SiteID()] = resolver
		}
	}
	projected := make([]subscription.V2TVCandidate, 0, len(candidates))
	records := make(map[string]v2TVCandidateRecord, len(candidates))
	for _, candidate := range candidates {
		resolver := resolvers[candidate.Resource.SiteID]
		if resolver == nil || strings.TrimSpace(candidate.Resource.DownloadID) == "" {
			continue
		}
		item, err := subscription.NewV2TVCandidate(candidate)
		if err != nil {
			continue
		}
		projected = append(projected, item)
		records[item.Key] = v2TVCandidateRecord{candidate: candidate, resolver: resolver}
	}
	return projected, records
}

func v2TVSeasonNeeds(
	target persistedSubscription,
	query subscription.Query,
) ([]subscription.V2TVSeasonNeed, error) {
	bySeason := make(map[int][]subscription.Episode)
	for _, episode := range query.MissingEpisodes {
		bySeason[episode.Season] = append(bySeason[episode.Season], episode)
	}
	seasons := make([]int, 0, len(bySeason))
	for season := range bySeason {
		seasons = append(seasons, season)
	}
	sort.Ints(seasons)
	result := make([]subscription.V2TVSeasonNeed, 0, len(seasons))
	configuredTotal, hasConfiguredTotal := configInteger(target.Config, "total_episode")
	configuredStart, hasConfiguredStart := configInteger(target.Config, "start_episode")
	for _, season := range seasons {
		episodes := bySeason[season]
		sort.Slice(episodes, func(i, j int) bool { return episodes[i].Episode < episodes[j].Episode })
		total := episodes[len(episodes)-1].Episode
		start := 1
		if len(seasons) == 1 && hasConfiguredTotal && configuredTotal > 0 {
			total = configuredTotal
		}
		if query.TVRecoveredTarget != nil {
			total = query.TVRecoveredTarget.TotalEpisode
		}
		if len(seasons) == 1 && hasConfiguredStart && configuredStart > 0 {
			start = configuredStart
		}
		if total < 1 || total > maxTrackedEpisode || start > total {
			return nil, subscription.ErrInvalidV2TVSelection
		}
		result = append(result, subscription.V2TVSeasonNeed{
			Season: season, MissingEpisodes: append([]subscription.Episode(nil), episodes...),
			TotalEpisodes: total, FullSeason: query.FullSeasonMissing[season],
		})
	}
	return result, nil
}

func v2TVEpisodesCoverRange(items []subscription.Episode, season, start, total int) bool {
	if len(items) != total-start+1 {
		return false
	}
	for index, item := range items {
		if item.Season != season || item.Episode != start+index {
			return false
		}
	}
	return true
}

func (s *v2TVExecutionSession) project(
	ctx context.Context,
	candidate subscription.V2TVCandidate,
) (subscription.V2TVEpisodeProjection, error) {
	projected, err := s.prepareCandidate(ctx, candidate.Key, nil, false)
	if err != nil {
		return subscription.V2TVEpisodeProjection{}, err
	}
	return subscription.V2TVEpisodeProjection{
		Episodes: append([]subscription.Episode(nil), projected.episodes...), Failed: projected.failed,
	}, nil
}

func (s *v2TVExecutionSession) prepareCandidate(
	ctx context.Context,
	candidateKey string,
	selected []subscription.Episode,
	recordInvalidMetainfo bool,
) (v2TVProjectedTorrent, error) {
	if s.failures.isActive(candidateKey) {
		return v2TVProjectedTorrent{failed: true}, nil
	}
	if cached, exists := s.projected[candidateKey]; exists {
		return cached, nil
	}
	record, exists := s.records[candidateKey]
	if !exists {
		return v2TVProjectedTorrent{failed: true}, nil
	}
	download, err := record.resolver.ResolveDownload(ctx, record.candidate.Resource.DownloadID)
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return v2TVProjectedTorrent{}, contextErr
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return v2TVProjectedTorrent{}, err
		}
		s.failures.record(ctx, candidateKey, selected, nil, s.downloader.Name, "下载种子内容为空")
		s.projected[candidateKey] = v2TVProjectedTorrent{failed: true}
		return s.projected[candidateKey], nil
	}
	if len(download.Body) == 0 {
		s.failures.record(ctx, candidateKey, selected, nil, s.downloader.Name, "下载种子内容为空")
		s.projected[candidateKey] = v2TVProjectedTorrent{failed: true}
		return s.projected[candidateKey], nil
	}
	inspection, err := torrentmeta.Inspect(download.Body)
	if err != nil {
		if recordInvalidMetainfo {
			s.failures.record(ctx, candidateKey, selected, nil, s.downloader.Name, "无法读取种子文件")
		}
		s.projected[candidateKey] = v2TVProjectedTorrent{failed: true}
		return s.projected[candidateKey], nil
	}
	candidate, err := subscription.NewV2TVCandidate(record.candidate)
	if err != nil {
		return v2TVProjectedTorrent{}, ErrSubscriptionSubmission
	}
	episodes := v2TVEpisodesFromTorrentFiles(
		inspection.Files,
		splitLines(configText(s.target.Config, "custom_words")),
		candidate.Seasons[0],
	)
	s.projected[candidateKey] = v2TVProjectedTorrent{
		body: download.Body, episodes: append([]subscription.Episode(nil), episodes...),
	}
	return s.projected[candidateKey], nil
}

func v2TVEpisodesFromTorrentFiles(
	files []torrentmeta.File,
	customWords []string,
	season int,
) []subscription.Episode {
	seen := make(map[subscription.Episode]struct{})
	for _, file := range files {
		name := path.Base(strings.ReplaceAll(file.Path, `\`, "/"))
		parsed := media.Parse(name, media.Options{CustomWords: customWords})
		if parsed.BeginEpisode == 0 {
			if match := v2TVStandaloneFileEpisode.FindStringSubmatch(name); match != nil {
				parsed = media.Parse("S"+strconv.Itoa(season)+match[1], media.Options{})
			}
		}
		if parsed.Type != media.TypeTV || parsed.BeginEpisode <= 0 ||
			parsed.BeginEpisode > maxTrackedEpisode || parsed.EndEpisode > maxTrackedEpisode {
			continue
		}
		// TorrentHelper.get_torrent_episodes returns episode numbers only in
		// V2. The title chooses the season, including when files omit Sxx.
		episodes, err := parsed.EpisodeList()
		if err != nil {
			continue
		}
		for _, episode := range episodes {
			seen[subscription.Episode{Season: season, Episode: episode}] = struct{}{}
		}
	}
	result := make([]subscription.Episode, 0, len(seen))
	for episode := range seen {
		result = append(result, episode)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Season != result[j].Season {
			return result[i].Season < result[j].Season
		}
		return result[i].Episode < result[j].Episode
	})
	return result
}

func (s *v2TVExecutionSession) attempt(
	ctx context.Context,
	attempt subscription.V2TVCandidateAttempt,
) (subscription.V2TVCandidateAttemptResult, error) {
	record, exists := s.records[attempt.Candidate.Key]
	if !exists {
		return subscription.V2TVCandidateAttemptResult{}, nil
	}
	projected, err := s.prepareCandidate(
		ctx,
		attempt.Candidate.Key,
		attempt.SelectedEpisodes,
		attempt.Stage == subscription.V2TVStageTitleEpisodes,
	)
	if err != nil {
		return subscription.V2TVCandidateAttemptResult{}, err
	}
	if projected.failed {
		return subscription.V2TVCandidateAttemptResult{}, nil
	}
	// V2 consumes the whole season need after its count check, the title range
	// after a title match, and only selected files after unpacking. File names
	// are not an additional admission gate for the title pass.
	coverage := attempt.ExpectedCoverage
	qualityCoverage := coverage
	if attempt.Stage == subscription.V2TVStageFullSeason {
		qualityCoverage = []subscription.Episode{}
		if len(attempt.Candidate.Seasons) == 1 {
			qualityCoverage = subscription.V2TVProjectedTitleEpisodes(attempt.Candidate.Seasons[0], projected.episodes)
		}
	}
	if len(coverage) == 0 {
		return subscription.V2TVCandidateAttemptResult{}, nil
	}
	intentID, err := s.stage(ctx, record, attempt.SelectedEpisodes, coverage, qualityCoverage, false)
	if err != nil {
		if errors.Is(err, downloadflow.ErrDownloadPlanConflict) {
			return subscription.V2TVCandidateAttemptResult{}, nil
		}
		return subscription.V2TVCandidateAttemptResult{}, err
	}
	result, err := s.execute(ctx, intentID, attempt.Candidate.Key)
	if err != nil {
		return subscription.V2TVCandidateAttemptResult{}, err
	}
	switch result.Outcome {
	case downloadflow.IntentExecutionAccepted:
		covered, coverageErr := s.confirmedCoverage(ctx, coverage)
		if errors.Is(coverageErr, ErrSubscriptionTargetChanged) {
			return subscription.V2TVCandidateAttemptResult{Stop: true}, nil
		}
		if coverageErr != nil {
			return subscription.V2TVCandidateAttemptResult{}, coverageErr
		}
		return subscription.V2TVCandidateAttemptResult{
			HashAcquired: true, CoveredEpisodes: covered,
		}, nil
	case downloadflow.IntentExecutionDefinitiveFailureRecovered:
		var fingerprintEpisodes []subscription.Episode
		if attempt.Stage == subscription.V2TVStageFullSeason && len(attempt.Candidate.Seasons) == 1 {
			fingerprintEpisodes = projected.episodes
		}
		s.failures.record(
			ctx,
			attempt.Candidate.Key,
			attempt.SelectedEpisodes,
			fingerprintEpisodes,
			s.downloader.Name,
			"添加下载任务失败",
		)
		if stop, refreshErr := s.refreshAfterRecoveredAttempt(ctx); refreshErr != nil || stop {
			return subscription.V2TVCandidateAttemptResult{Stop: stop}, refreshErr
		}
		return subscription.V2TVCandidateAttemptResult{}, nil
	case downloadflow.IntentExecutionPermanentCandidateFailure:
		if stop, refreshErr := s.refreshAfterRecoveredAttempt(ctx); refreshErr != nil || stop {
			return subscription.V2TVCandidateAttemptResult{Stop: stop}, refreshErr
		}
		return subscription.V2TVCandidateAttemptResult{}, nil
	case downloadflow.IntentExecutionPendingReconciliation:
		return subscription.V2TVCandidateAttemptResult{Stop: true}, nil
	case downloadflow.IntentExecutionRetryableFailure:
		return subscription.V2TVCandidateAttemptResult{}, ErrSubscriptionSubmission
	default:
		return subscription.V2TVCandidateAttemptResult{}, ErrSubscriptionSubmission
	}
}

func (s *v2TVExecutionSession) stage(
	ctx context.Context,
	record v2TVCandidateRecord,
	selected []subscription.Episode,
	expected []subscription.Episode,
	qualityCoverage []subscription.Episode,
	confirmedFullCoverage bool,
) (uuid.UUID, error) {
	if err := s.loadDownloadSettings(ctx); err != nil {
		return uuid.Nil, err
	}
	resourceID, resourceSecret, err := downloadflow.ProtectResourceLocator(record.candidate.Resource.DownloadID)
	if err != nil {
		return uuid.Nil, ErrSubscriptionSubmission
	}
	paused := len(selected) > 0
	reference := downloadflow.SourceReference{
		SiteID: int64(record.candidate.Resource.SiteID), ResolverKind: record.resolver.ResolverKind(),
		ResolverVersion: record.resolver.ResolverVersion(), ResourceID: resourceID,
		DownloaderID: s.downloader.ID,
	}
	downloadCandidate, err := reference.Candidate(s.savePath, paused)
	if err != nil {
		return uuid.Nil, ErrSubscriptionSubmission
	}
	intent, err := downloader.NewIntent(s.target.Username, downloadCandidate)
	if err != nil {
		return uuid.Nil, ErrSubscriptionSubmission
	}
	plan := persistedDownloadIntentPlan{
		IntentID: intent.ID, SiteID: int64(record.candidate.Resource.SiteID),
		DownloaderID: s.downloader.ID, ResolverKind: record.resolver.ResolverKind(),
		ResolverVersion: record.resolver.ResolverVersion(), ResourceID: resourceID,
		ResourceSecret: resourceSecret, SavePath: s.savePath, Paused: paused,
		TVPriority:     intPointerCopy(record.candidate.Resource.PriOrder),
		TVCoverage:     append([]subscription.Episode{}, qualityCoverage...),
		TVFullCoverage: confirmedFullCoverage,
	}
	if s.recovered != nil {
		plan.TVRecoveredTarget = &subscription.TVRecoveredTarget{
			TotalEpisode:     s.recovered.TotalEpisode,
			MissingEpisodes:  append([]subscription.Episode{}, s.recovered.MissingEpisodes...),
			ExpectedCoverage: append([]subscription.Episode{}, expected...),
		}
	}
	successCandidate := record.candidate
	successCandidate.CoveredEpisodes = append([]subscription.Episode(nil), qualityCoverage...)
	input := s.handler.buildSubscriptionSuccessInput(ctx, s.successTarget(), successCandidate, intent.ID)
	storedID, err := s.persistStage(ctx, intent, plan, input, expected)
	if errors.Is(err, downloadflow.ErrDownloadPlanConflict) {
		return uuid.Nil, err
	}
	if err != nil || storedID != intent.ID {
		return uuid.Nil, stableContextError(ctx, err, ErrSubscriptionSubmission)
	}
	return storedID, nil
}

func (s *v2TVExecutionSession) persistStage(
	ctx context.Context,
	intent downloader.Intent,
	plan persistedDownloadIntentPlan,
	input organization.SuccessInput,
	episodes []subscription.Episode,
) (uuid.UUID, error) {
	if subscriptionBestVersionEnabled(s.target.Config) &&
		(s.target.InternalState == subscription.Active || s.target.InternalState == subscription.PendingIdentification) {
		stager, ok := s.handler.submission.(stagedRSSTVDownloadSubmission)
		if !ok {
			return uuid.Nil, ErrSubscriptionSubmission
		}
		return stager.StageRSSTargetEpisodes(
			ctx, s.target.Username, s.target.ID, s.target.InternalState, s.target.UpdatedAt,
			intent, plan, input, episodes,
		)
	}
	if s.rss && s.target.InternalState == subscription.PendingIdentification {
		stager := s.handler.submission.(stagedRSSTVDownloadSubmission)
		return stager.StageRSSTargetEpisodes(
			ctx, s.target.Username, s.target.ID, subscription.PendingIdentification, s.target.UpdatedAt,
			intent, plan, input, episodes,
		)
	}
	if s.rss && !s.rssReadyStaged {
		stager := s.handler.submission.(stagedRSSTVDownloadSubmission)
		storedID, err := stager.StageRSSTargetEpisodes(
			ctx, s.target.Username, s.target.ID, subscription.Active, s.target.UpdatedAt,
			intent, plan, input, episodes,
		)
		if err == nil {
			s.rssReadyStaged = true
		}
		return storedID, err
	}
	stager := s.handler.submission.(stagedTVDownloadSubmission)
	return stager.StageSubscriptionTargetEpisodes(
		ctx, s.target.Username, s.target.ID, s.target.UpdatedAt, intent, plan, input, episodes,
	)
}

func (s *v2TVExecutionSession) execute(
	ctx context.Context,
	intentID uuid.UUID,
	candidateKey string,
) (downloadflow.IntentExecutionResult, error) {
	if projection, exists := s.projected[candidateKey]; exists && !projection.failed && len(projection.body) > 0 {
		return s.handler.execution.ExecuteIntentWithMetainfo(
			ctx, s.target.Username, intentID, projection.body,
		)
	}
	return s.handler.execution.ExecuteIntent(ctx, s.target.Username, intentID)
}

func (s *v2TVExecutionSession) confirmedCoverage(
	ctx context.Context,
	expected []subscription.Episode,
) ([]subscription.Episode, error) {
	current, found, currentErr := s.currentTarget(ctx)
	var rows []subscription.SubscriptionEpisode
	var err error
	if currentErr != nil {
		return nil, currentErr
	}
	if s.recovered != nil {
		if found {
			s.target = current
		}
		// ExecuteIntent returns accepted only after parent settlement. The
		// immutable recovered plan records selector consumption; old episode
		// rows deliberately do not supply note in this total-zero branch.
		covered := append([]subscription.Episode{}, expected...)
		consumed := make(map[subscription.Episode]struct{}, len(expected))
		for _, episode := range expected {
			consumed[episode] = struct{}{}
		}
		remaining := make([]subscription.Episode, 0, len(s.recovered.MissingEpisodes))
		for _, episode := range s.recovered.MissingEpisodes {
			if _, exists := consumed[episode]; !exists {
				remaining = append(remaining, episode)
			}
		}
		s.recovered.MissingEpisodes = remaining
		return covered, nil
	}
	if found {
		after, _, afterErr := tvTargetSnapshot(current)
		afterTarget, afterDescribeErr := subscription.DescribeTVTarget(after)
		if afterErr != nil || afterDescribeErr != nil {
			return nil, ErrSubscriptionSearchStorage
		}
		s.target = current
		rows, err = s.handler.store.ListSubscriptionEpisodesForTarget(
			ctx, current.Username, current.ID, current.UpdatedAt, afterTarget.TargetEpisodes,
			[]subscription.SubscriptionEpisodeState{subscription.SubscriptionEpisodeMissing}, maxTrackedEpisodes,
		)
	} else {
		rows, err = s.handler.store.ListSubscriptionEpisodes(
			ctx, s.target.Username, s.target.ID,
			[]subscription.SubscriptionEpisodeState{subscription.SubscriptionEpisodeMissing}, maxTrackedEpisodes,
		)
	}
	if err != nil {
		return nil, stableContextError(ctx, err, ErrSubscriptionSearchStorage)
	}
	missing := make(map[subscription.Episode]struct{}, len(rows))
	for _, row := range rows {
		missing[subscription.Episode{Season: row.Season, Episode: row.Episode}] = struct{}{}
	}
	covered := make([]subscription.Episode, 0, len(expected))
	for _, episode := range expected {
		if _, remainsMissing := missing[episode]; !remainsMissing {
			covered = append(covered, episode)
		}
	}
	if len(covered) == 0 {
		return nil, ErrSubscriptionSubmission
	}
	return covered, nil
}

func (s *v2TVExecutionSession) refreshAfterRecoveredAttempt(ctx context.Context) (bool, error) {
	current, found, err := s.currentTarget(ctx)
	if errors.Is(err, ErrSubscriptionTargetChanged) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if !found {
		return true, nil
	}
	s.target = current
	return false, nil
}

func (s *v2TVExecutionSession) currentTarget(
	ctx context.Context,
) (persistedSubscription, bool, error) {
	current, err := s.handler.store.GetActiveSubscription(ctx, s.target.Username, s.target.ID)
	if subscriptionTargetNotFound(err) {
		return persistedSubscription{}, false, nil
	}
	if err != nil {
		return persistedSubscription{}, false, stableContextError(ctx, err, ErrSubscriptionSearchStorage)
	}
	if !sameV2TVExecutionContext(s.target, current) {
		return persistedSubscription{}, false, ErrSubscriptionTargetChanged
	}
	before, _, beforeErr := tvTargetSnapshot(s.target)
	after, _, afterErr := tvTargetSnapshot(current)
	beforeTarget, beforeDescribeErr := subscription.DescribeTVTarget(before)
	afterTarget, afterDescribeErr := subscription.DescribeTVTarget(after)
	if beforeErr != nil || afterErr != nil || beforeDescribeErr != nil || afterDescribeErr != nil {
		return persistedSubscription{}, false, ErrSubscriptionSearchStorage
	}
	if beforeTarget.Knowledge != afterTarget.Knowledge ||
		!reflect.DeepEqual(beforeTarget.TargetEpisodes, afterTarget.TargetEpisodes) {
		return persistedSubscription{}, false, ErrSubscriptionTargetChanged
	}
	return current, true, nil
}

func sameV2TVExecutionContext(before, after persistedSubscription) bool {
	if before.ID != after.ID || before.Username != after.Username || before.Name != after.Name ||
		before.MediaType != after.MediaType || before.Year != after.Year ||
		before.MediaSource != after.MediaSource || before.MediaID != after.MediaID ||
		!sameOptionalInt(before.Season, after.Season) {
		return false
	}
	beforeConfig, beforeErr := json.Marshal(v2TVExecutionStableConfig(before.Config))
	afterConfig, afterErr := json.Marshal(v2TVExecutionStableConfig(after.Config))
	return beforeErr == nil && afterErr == nil && bytes.Equal(beforeConfig, afterConfig)
}

func sameOptionalInt(left, right *int) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func v2TVExecutionStableConfig(config map[string]any) map[string]any {
	stable := make(map[string]any, len(config))
	for key, value := range config {
		switch key {
		case "note", "lack_episode", "current_priority", "episode_priority":
			continue
		default:
			stable[key] = value
		}
	}
	return stable
}

func (s *v2TVExecutionSession) loadDownloadSettings(ctx context.Context) error {
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

func (s *v2TVExecutionSession) successTarget() subscriptionSuccessTarget {
	return subscriptionSuccessTarget{
		Username: s.target.Username, Name: s.target.Name, Year: s.target.Year,
		MediaType: s.target.MediaType, MediaSource: s.target.MediaSource,
		MediaID: s.target.MediaID, VoteAverage: subscriptionConfigVote(s.target.Config),
		PosterURL:   configText(s.target.Config, "poster"),
		BackdropURL: configText(s.target.Config, "backdrop"), Config: s.target.Config,
	}
}

func canExecuteV2TVCandidates(handler *Handler, rss bool) bool {
	if handler == nil || handler.execution == nil {
		return false
	}
	if _, ok := handler.submission.(stagedTVDownloadSubmission); !ok {
		return false
	}
	if rss {
		_, ok := handler.submission.(stagedRSSTVDownloadSubmission)
		return ok
	}
	return true
}

var (
	_ DownloadIntentExecution       = (*downloadflow.Handler)(nil)
	_ stagedTVDownloadSubmission    = (*downloadflow.SubmissionService)(nil)
	_ stagedRSSTVDownloadSubmission = (*downloadflow.SubmissionService)(nil)
)

package subscriptionflow

import (
	"context"
	"errors"

	"github.com/moviepilot-go/moviepilot-go/internal/downloadflow"
	"github.com/moviepilot-go/moviepilot-go/internal/indexer"
	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

// An accepted terminal quality moves the subscription into history. This
// optional owner read distinguishes that durable completion from deletion.
type ownedV2TVSubscriptionStore interface {
	GetOwnedSubscription(context.Context, string, int64) (persistedSubscription, error)
}

// Best-version candidates use quality facts rather than ordinary episode state.
// Lower quality remains pending for later rounds after this batch consumes it.
func (h *Handler) executeV2TVBestVersionCandidates(
	ctx context.Context,
	target persistedSubscription,
	candidates []subscription.Candidate,
	adapters []indexer.Adapter,
	rss bool,
) (subscription.V2TVSelectionResult, error) {
	projected, records := downloadableV2TVCandidates(candidates, adapters)
	failures, projected := newV2TVDownloadFailureSession(ctx, h.store, target, records, projected)
	session := &v2TVExecutionSession{
		handler: h, target: target, records: records, failures: failures, rss: rss,
		projected: make(map[string]v2TVProjectedTorrent, len(records)),
	}
	facts, err := session.bestVersionFacts()
	if err != nil || facts.Complete {
		return session.bestVersionResult(nil, false, err)
	}
	snapshot, _, err := tvTargetSnapshot(target)
	if err != nil {
		return session.bestVersionResult(nil, false, err)
	}
	resolution, err := subscription.DescribeTVTarget(snapshot)
	if err != nil {
		return session.bestVersionResult(nil, false, ErrSubscriptionConfiguration)
	}
	state := subscription.V2TVSubscriptionStateReady
	if target.InternalState == subscription.PendingIdentification {
		state = subscription.V2TVSubscriptionStatePending
	}
	matched := make([]subscription.V2TVCandidate, 0, len(projected))
	packs := make([]subscription.V2TVCandidate, 0, len(projected))
	for _, candidate := range projected {
		if len(candidate.Seasons) != 1 || candidate.Seasons[0] != snapshot.Season {
			continue
		}
		decision, err := subscription.PrepareTVBestVersionCandidate(
			snapshot, records[candidate.Key].candidate.Resource.PriOrder, candidate.TitleEpisodes,
		)
		if err != nil {
			return session.bestVersionResult(nil, false, ErrSubscriptionConfiguration)
		}
		if !decision.Allowed {
			continue
		}
		if !snapshot.BestVersionFull {
			candidate.AllowedEpisodes = append([]subscription.Episode{}, decision.InterestedEpisodes...)
		}
		matched = append(matched, candidate)
		fullTitle := len(candidate.TitleEpisodes) == 0 ||
			v2TVEpisodesInclude(candidate.TitleEpisodes, resolution.TargetEpisodes)
		if fullTitle && (snapshot.BestVersionFull || decision.PreferFullPack) {
			packs = append(packs, candidate)
		}
	}
	// V2 freezes the quality gate before batch_download. The batch removes an
	// accepted episode from this round even if its priority is below 100.
	selectBatch := func(items []subscription.V2TVCandidate, missing []subscription.Episode, complete bool) (subscription.V2TVSelectionResult, error) {
		request := subscription.V2TVSelectionRequest{
			SubscriptionState: state,
			Seasons: []subscription.V2TVSeasonNeed{{
				Season: snapshot.Season, MissingEpisodes: missing, TotalEpisodes: snapshot.TotalEpisode,
				FullSeason: complete, RequireCompleteCoverage: complete,
			}},
			Candidates: items,
		}
		return subscription.SelectV2TVCandidates(ctx, request, session.project,
			func(ctx context.Context, attempt subscription.V2TVCandidateAttempt) (subscription.V2TVCandidateAttemptResult, error) {
				return session.attemptBestVersion(ctx, attempt, complete)
			})
	}
	if len(resolution.TargetEpisodes) > 0 && len(packs) > 0 {
		result, err := selectBatch(packs, resolution.TargetEpisodes, true)
		if err != nil || result.Stopped || len(result.AcceptedCandidateKeys) > 0 {
			// A successful full-pack batch returns immediately in V2. It does not
			// fall through to another quality upgrade in the same round.
			return session.bestVersionResult(result.AcceptedCandidateKeys, result.Stopped, err)
		}
	}
	if !snapshot.BestVersionFull && len(facts.PendingEpisodes) > 0 {
		for index := range matched {
			candidate := &matched[index]
			projection, inspected := session.projected[candidate.Key]
			if inspected && len(candidate.TitleEpisodes) == 0 && len(projection.episodes) > 0 {
				candidate.TitleEpisodes = subscription.V2TVProjectedTitleEpisodes(snapshot.Season, projection.episodes)
			}
		}
		result, err := selectBatch(matched, facts.PendingEpisodes, false)
		if err != nil || result.Stopped || len(result.AcceptedCandidateKeys) > 0 {
			return session.bestVersionResult(result.AcceptedCandidateKeys, result.Stopped, err)
		}
	}
	if session.target.InternalState == subscription.PendingSearch {
		if err := h.finishTarget(ctx, session.target, subscription.Active); err != nil {
			return session.bestVersionResult(nil, false, err)
		}
	}
	return session.bestVersionResult(nil, false, nil)
}

func (s *v2TVExecutionSession) attemptBestVersion(
	ctx context.Context,
	attempt subscription.V2TVCandidateAttempt,
	confirmedFullCoverage bool,
) (subscription.V2TVCandidateAttemptResult, error) {
	record := s.records[attempt.Candidate.Key]
	if err := s.loadDownloadSettings(ctx); err != nil {
		return subscription.V2TVCandidateAttemptResult{}, err
	}
	projected, err := s.prepareCandidate(ctx, attempt.Candidate.Key, attempt.SelectedEpisodes,
		attempt.Stage == subscription.V2TVStageTitleEpisodes)
	if err != nil || projected.failed {
		return subscription.V2TVCandidateAttemptResult{}, err
	}
	current, found, err := s.currentTarget(ctx)
	if errors.Is(err, ErrSubscriptionTargetChanged) || !found && err == nil {
		return subscription.V2TVCandidateAttemptResult{Stop: true}, nil
	}
	if err != nil {
		return subscription.V2TVCandidateAttemptResult{}, err
	}
	if !current.UpdatedAt.Equal(s.target.UpdatedAt) ||
		current.InternalState != s.target.InternalState || current.V2State != s.target.V2State {
		return subscription.V2TVCandidateAttemptResult{Stop: true}, nil
	}
	coverage := attempt.ExpectedCoverage
	qualityCoverage := coverage
	if attempt.Stage == subscription.V2TVStageTitleEpisodes {
		qualityCoverage = attempt.Candidate.TitleEpisodes
	} else if attempt.Stage == subscription.V2TVStageFullSeason {
		qualityCoverage = subscription.V2TVProjectedTitleEpisodes(attempt.Candidate.Seasons[0], projected.episodes)
	}
	intentID, err := s.stage(ctx, record, attempt.SelectedEpisodes, coverage, qualityCoverage, confirmedFullCoverage)
	if errors.Is(err, downloadflow.ErrDownloadPlanConflict) {
		return subscription.V2TVCandidateAttemptResult{}, nil
	}
	if err != nil {
		return subscription.V2TVCandidateAttemptResult{}, err
	}
	result, err := s.execute(ctx, intentID, attempt.Candidate.Key)
	if err != nil {
		return subscription.V2TVCandidateAttemptResult{}, err
	}
	switch result.Outcome {
	case downloadflow.IntentExecutionAccepted:
		stop, err := s.refreshAcceptedBestVersion(ctx, record, qualityCoverage, confirmedFullCoverage)
		if err != nil || stop {
			return subscription.V2TVCandidateAttemptResult{Stop: stop}, err
		}
		return subscription.V2TVCandidateAttemptResult{HashAcquired: true, CoveredEpisodes: coverage}, nil
	case downloadflow.IntentExecutionDefinitiveFailureRecovered:
		var fingerprintEpisodes []subscription.Episode
		if attempt.Stage == subscription.V2TVStageFullSeason {
			fingerprintEpisodes = projected.episodes
		}
		s.failures.record(ctx, attempt.Candidate.Key, attempt.SelectedEpisodes, fingerprintEpisodes,
			s.downloader.Name, "添加下载任务失败")
		stop, err := s.refreshRecoveredBestVersion(ctx)
		return subscription.V2TVCandidateAttemptResult{Stop: stop}, err
	case downloadflow.IntentExecutionPermanentCandidateFailure:
		stop, err := s.refreshRecoveredBestVersion(ctx)
		return subscription.V2TVCandidateAttemptResult{Stop: stop}, err
	case downloadflow.IntentExecutionPendingReconciliation:
		return subscription.V2TVCandidateAttemptResult{Stop: true}, nil
	default:
		return subscription.V2TVCandidateAttemptResult{}, ErrSubscriptionSubmission
	}
}
func (s *v2TVExecutionSession) refreshRecoveredBestVersion(ctx context.Context) (bool, error) {
	wasPending := s.target.InternalState == subscription.PendingIdentification
	stop, err := s.refreshAfterRecoveredAttempt(ctx)
	if stop || err != nil {
		return stop, err
	}
	expected := subscription.PendingSearch
	if wasPending {
		expected = subscription.PendingIdentification
	}
	projected, err := subscription.V2State(expected)
	return s.target.InternalState != expected || s.target.V2State != projected, err
}

func (s *v2TVExecutionSession) refreshAcceptedBestVersion(
	ctx context.Context,
	record v2TVCandidateRecord,
	actual []subscription.Episode,
	fullCoverage bool,
) (bool, error) {
	before, _, err := tvTargetSnapshot(s.target)
	if err != nil {
		return false, err
	}
	priority := record.candidate.Resource.PriOrder
	predicted, err := subscription.RecordTVDownloadPriority(before, &priority, actual, fullCoverage)
	if err != nil {
		return false, ErrSubscriptionSubmission
	}
	predictedFacts, err := subscription.DescribeTVBestVersion(predicted)
	if err != nil {
		return false, ErrSubscriptionSubmission
	}
	current, found, err := s.currentTarget(ctx)
	if errors.Is(err, ErrSubscriptionTargetChanged) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if !found {
		ownerStore, ok := s.handler.store.(ownedV2TVSubscriptionStore)
		if !ok || s.target.InternalState == subscription.PendingIdentification || !predictedFacts.Complete {
			return true, nil
		}
		current, err = ownerStore.GetOwnedSubscription(ctx, s.target.Username, s.target.ID)
		if subscriptionTargetNotFound(err) {
			return true, nil
		}
		if err != nil {
			return false, stableContextError(ctx, err, ErrSubscriptionSearchStorage)
		}
		if current.CompletedAt == nil || current.InternalState != subscription.Completed ||
			current.V2State != subscription.V2TVSubscriptionStateCompleted ||
			!sameV2TVExecutionContext(s.target, current) {
			return true, nil
		}
	}
	after, _, err := tvTargetSnapshot(current)
	if err != nil {
		return false, err
	}
	afterFacts, err := subscription.DescribeTVBestVersion(after)
	if err != nil {
		return false, ErrSubscriptionSearchStorage
	}
	for _, episode := range v2TVEpisodeIntersection(actual, initialMissingEpisodes(s.target)) {
		expectedPriority, predictedKnown := predictedFacts.EpisodePriority[episode.Episode]
		actualPriority, actualKnown := afterFacts.EpisodePriority[episode.Episode]
		if predictedKnown && (!actualKnown || actualPriority < expectedPriority) {
			return false, ErrSubscriptionSubmission
		}
	}
	if before.BestVersionFull && afterFacts.CurrentPriority < predictedFacts.CurrentPriority {
		return false, ErrSubscriptionSubmission
	}
	if !found && !afterFacts.Complete {
		return true, nil
	}
	if s.target.InternalState == subscription.PendingIdentification {
		if current.InternalState != subscription.PendingIdentification {
			return true, nil
		}
	} else if !afterFacts.Complete && current.InternalState != subscription.Active {
		return true, nil
	}
	s.target = current
	return false, nil
}

func (s *v2TVExecutionSession) bestVersionFacts() (subscription.TVBestVersionFacts, error) {
	snapshot, _, err := tvTargetSnapshot(s.target)
	if err != nil {
		return subscription.TVBestVersionFacts{}, err
	}
	facts, err := subscription.DescribeTVBestVersion(snapshot)
	if err != nil {
		return subscription.TVBestVersionFacts{}, ErrSubscriptionConfiguration
	}
	return facts, nil
}

func (s *v2TVExecutionSession) bestVersionResult(
	accepted []string,
	stopped bool,
	resultErr error,
) (subscription.V2TVSelectionResult, error) {
	facts, err := s.bestVersionFacts()
	if resultErr == nil {
		resultErr = err
	}
	remaining := append([]subscription.Episode{}, facts.PendingEpisodes...)
	if snapshot, _, snapshotErr := tvTargetSnapshot(s.target); snapshotErr == nil && snapshot.BestVersionFull {
		remaining = []subscription.Episode{}
		if !facts.Complete {
			remaining = initialMissingEpisodes(s.target)
		}
	}
	state := subscription.V2TVSubscriptionStateReady
	if s.target.InternalState == subscription.PendingIdentification {
		state = subscription.V2TVSubscriptionStatePending
	} else if facts.Complete {
		state = subscription.V2TVSubscriptionStateCompleted
	}
	return subscription.V2TVSelectionResult{
		SubscriptionState: state, AcceptedCandidateKeys: append([]string{}, accepted...),
		RemainingEpisodes: remaining, Stopped: stopped,
	}, resultErr
}

func v2TVEpisodesInclude(actual, expected []subscription.Episode) bool {
	return len(expected) > 0 && len(v2TVEpisodeIntersection(actual, expected)) == len(expected)
}

func v2TVEpisodeIntersection(actual, allowed []subscription.Episode) []subscription.Episode {
	set := make(map[subscription.Episode]struct{}, len(allowed))
	for _, episode := range allowed {
		set[episode] = struct{}{}
	}
	result := make([]subscription.Episode, 0, len(actual))
	for _, episode := range actual {
		if _, exists := set[episode]; exists {
			result = append(result, episode)
		}
	}
	return result
}

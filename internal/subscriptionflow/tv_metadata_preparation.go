package subscriptionflow

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/moviepilot-go/moviepilot-go/internal/metadata"
	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

type tvSearchReadState struct {
	InitialMediaTitle        string
	InitialRecognized        bool
	RecoveredLibraryQuery    ExistingEpisodesQuery
	LegacyWithoutTotal       bool
	InitialRecognitionFailed bool
	MissingContainerEmpty    bool
	PreflightSatisfied       bool
	UnresolvedSeasonFacts    bool
	RecoveredTotal           int
}

// prepareTVSearchTarget keeps the initial recognition and DownloadChain's
// local retry separate. Only the initial detail can refresh config totals.
func (h *Handler) prepareTVSearchTarget(
	ctx context.Context,
	target persistedSubscription,
) (persistedSubscription, subscription.TVTargetResolution, tvSearchReadState, error) {
	snapshot, legacy, err := tvTargetSnapshot(target)
	if err != nil {
		return target, subscription.TVTargetResolution{}, tvSearchReadState{}, err
	}
	state := tvSearchReadState{LegacyWithoutTotal: legacy}
	if snapshot.BestVersion {
		refreshed, resolution, legacyTarget, refreshErr := h.refreshTVTarget(ctx, target)
		state.LegacyWithoutTotal = legacyTarget
		return refreshed, resolution, state, refreshErr
	}
	resolution, _ := subscription.DescribeTVTarget(snapshot)
	initial, recognized, readErr := h.subscriptionMediaFactsRead(ctx, target, true)
	if contextErr := existingEpisodesContextError(ctx, readErr); contextErr != nil {
		return target, resolution, state, contextErr
	}
	if !recognized {
		state.InitialRecognitionFailed = true
		return target, resolution, state, nil
	}
	state.InitialMediaTitle = strings.TrimSpace(initial.Title)
	state.InitialRecognized = true
	if !validTVMetadataSeasons(initial) {
		state.UnresolvedSeasonFacts = true
		return target, resolution, state, nil
	}
	initialEpisodes := targetEpisodesFromMetadata(target.Season, 1, initial)
	total := 0
	if len(initialEpisodes) > 0 {
		total = initialEpisodes[len(initialEpisodes)-1].Episode
	}
	missingDetail := initial
	if !tvMetadataHasSeasons(initial) {
		missingDetail, recognized, readErr = h.subscriptionMediaFactsRead(ctx, target, false)
		if contextErr := existingEpisodesContextError(ctx, readErr); contextErr != nil {
			return target, resolution, state, contextErr
		}
		if !recognized {
			state.MissingContainerEmpty = true
			return target, resolution, state, nil
		}
		if !validTVMetadataSeasons(missingDetail) {
			state.UnresolvedSeasonFacts = true
			return target, resolution, state, nil
		}
		if !tvMetadataHasSeasons(missingDetail) {
			state.MissingContainerEmpty = true
			return target, resolution, state, nil
		}
	}
	if len(targetEpisodesFromMetadata(target.Season, 1, missingDetail)) == 0 {
		// A truthy seasons dictionary with no target episodes does not retry.
		// V2 still reads the library and can report True + {} for another season.
		state.MissingContainerEmpty = true
		state.PreflightSatisfied, err = h.emptyTVMetadataLibrarySatisfied(ctx, target)
		return target, resolution, state, err
	}
	refreshed, resolution, legacyTarget, err := h.refreshTVTargetWithTotal(ctx, target, total, true)
	state.LegacyWithoutTotal = legacyTarget
	if err == nil && !legacyTarget && resolution.Snapshot.TotalEpisode == 0 {
		episodes := targetEpisodesFromMetadata(target.Season, 1, missingDetail)
		state.RecoveredTotal = len(episodes)
		state.RecoveredLibraryQuery = recoveredTVLibraryQuery(target, missingDetail)
	}
	return refreshed, resolution, state, err
}

func tvMetadataHasSeasons(detail metadata.Media) bool {
	return len(detail.SeasonInfo) > 0 || detail.EpisodeCount > 0
}

// These are the existing Go coordinate limits, not a V2 empty season result.
func validTVMetadataSeasons(detail metadata.Media) bool {
	if len(detail.SeasonInfo) == 0 {
		return detail.EpisodeCount >= 0 && detail.EpisodeCount <= maxTrackedEpisode
	}
	seen := make(map[int]struct{}, len(detail.SeasonInfo))
	total := 0
	for _, season := range detail.SeasonInfo {
		if season.SeasonNumber < 0 || season.SeasonNumber > maxTrackedSeason || season.EpisodeCount < 0 || season.EpisodeCount > maxTrackedEpisode || total > maxTrackedEpisodes-season.EpisodeCount {
			return false
		}
		if _, duplicate := seen[season.SeasonNumber]; duplicate {
			return false
		}
		seen[season.SeasonNumber] = struct{}{}
		total += season.EpisodeCount
	}
	return true
}

func (h *Handler) emptyTVMetadataLibrarySatisfied(ctx context.Context, target persistedSubscription) (bool, error) {
	if h.existingEpisodes == nil {
		return false, nil
	}
	episodes, err := h.existingEpisodes.FindExistingEpisodes(ctx, ExistingEpisodesQuery{
		Title: strings.TrimSpace(target.Name), Year: strings.TrimSpace(target.Year),
		MediaSource: strings.TrimSpace(target.MediaSource), MediaID: strings.TrimSpace(target.MediaID),
	})
	if err != nil {
		return false, existingEpisodesContextError(ctx, err)
	}
	if len(episodes) > maxTrackedEpisodes {
		return false, nil
	}
	for _, episode := range episodes {
		if episode.Season < 0 || episode.Season > maxTrackedSeason ||
			episode.Episode < 1 || episode.Episode > maxTrackedEpisode {
			return false, nil
		}
	}
	return len(episodes) > 0, nil
}

type emptyTVSubscriptionProgressStore interface {
	RecordEmptyTVSubscriptionProgress(context.Context, string, int64, subscription.State, time.Time) error
}

// finishEmptyTVMissingContainer reproduces finish_subscribe_or_not's progress
// write before __finish_subscribe's P guard. No download was accepted here.
func (h *Handler) finishEmptyTVMissingContainer(ctx context.Context, target persistedSubscription) error {
	if target.InternalState != subscription.PendingIdentification {
		return h.finishSatisfiedTarget(ctx, target, target.InternalState)
	}
	store, ok := h.store.(emptyTVSubscriptionProgressStore)
	if !ok {
		return ErrSubscriptionSearchStorage
	}
	err := store.RecordEmptyTVSubscriptionProgress(ctx, target.Username, target.ID, target.InternalState, target.UpdatedAt)
	if err == nil || subscriptionTargetNotFound(err) {
		return nil
	}
	if errors.Is(err, errSubscriptionSearchStateConflict) {
		return ErrSubscriptionTargetChanged
	}
	return stableContextError(ctx, err, ErrSubscriptionSearchStorage)
}

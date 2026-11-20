package database

import (
	"strings"

	"github.com/google/uuid"

	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

func buildUpsertSubscriptionEpisodes(
	username string,
	subscriptionID int64,
	episodes []subscription.Episode,
) (string, []any, error) {
	username, normalized, err := normalizeSubscriptionEpisodeInput(username, subscriptionID, episodes)
	if err != nil {
		return "", nil, err
	}
	seasons, episodeNumbers := subscriptionEpisodeArrays(normalized)
	return upsertSubscriptionEpisodesSQL, []any{
		subscriptionID,
		username,
		seasons,
		episodeNumbers,
		maxSubscriptionEpisodes,
	}, nil
}

func buildListSubscriptionEpisodes(
	username string,
	subscriptionID int64,
	states []subscription.SubscriptionEpisodeState,
	limit int,
) (string, []any, error) {
	username = strings.TrimSpace(username)
	normalizedStates, err := normalizeSubscriptionEpisodeStates(states)
	if username == "" || subscriptionID <= 0 || err != nil || limit < 1 || limit > maxSubscriptionEpisodes {
		return "", nil, ErrInvalidSubscriptionEpisode
	}
	return listSubscriptionEpisodesSQL, []any{
		subscriptionID,
		username,
		normalizedStates,
		limit,
	}, nil
}

func buildCompareAndSwapSubscriptionEpisodeState(
	username string,
	subscriptionID int64,
	episode subscription.Episode,
	current subscription.SubscriptionEpisodeState,
	next subscription.SubscriptionEpisodeState,
) (string, []any, error) {
	username = strings.TrimSpace(username)
	if username == "" || subscriptionID <= 0 || !validSubscriptionEpisode(episode) ||
		ValidateSubscriptionEpisodeTransition(current, next) != nil {
		return "", nil, ErrInvalidSubscriptionEpisode
	}
	return compareAndSwapSubscriptionEpisodeStateSQL, []any{
		subscriptionID,
		username,
		episode.Season,
		episode.Episode,
		next,
		current,
	}, nil
}

func buildBindSubscriptionEpisodes(
	username string,
	subscriptionID int64,
	intentID uuid.UUID,
	episodes []subscription.Episode,
) (string, []any, int64, error) {
	username, normalized, err := normalizeSubscriptionEpisodeInput(username, subscriptionID, episodes)
	if err != nil || intentID == uuid.Nil {
		return "", nil, 0, ErrInvalidSubscriptionEpisode
	}
	seasons, episodeNumbers := subscriptionEpisodeArrays(normalized)
	return bindSubscriptionEpisodesSQL, []any{
		subscriptionID,
		username,
		intentID,
		seasons,
		episodeNumbers,
	}, int64(len(normalized)), nil
}

func buildVerifySubscriptionEpisodesBoundToIntent(
	username string,
	subscriptionID int64,
	intentID uuid.UUID,
	episodes []subscription.Episode,
) (string, []any, int64, error) {
	username, normalized, err := normalizeSubscriptionEpisodeInput(username, subscriptionID, episodes)
	if err != nil || intentID == uuid.Nil {
		return "", nil, 0, ErrInvalidSubscriptionEpisode
	}
	seasons, episodeNumbers := subscriptionEpisodeArrays(normalized)
	return verifySubscriptionEpisodesBoundToIntentSQL, []any{
		subscriptionID,
		username,
		intentID,
		seasons,
		episodeNumbers,
	}, int64(len(normalized)), nil
}

func buildTransitionSubscriptionEpisodesByIntent(
	username string,
	intentID uuid.UUID,
	fromStates []subscription.SubscriptionEpisodeState,
	next subscription.SubscriptionEpisodeState,
) (string, []any, error) {
	username = strings.TrimSpace(username)
	normalizedStates, err := normalizeSubscriptionEpisodeStates(fromStates)
	if username == "" || intentID == uuid.Nil || err != nil {
		return "", nil, ErrInvalidSubscriptionEpisode
	}
	for _, current := range normalizedStates {
		if ValidateSubscriptionEpisodeTransition(subscription.SubscriptionEpisodeState(current), next) != nil {
			return "", nil, ErrInvalidSubscriptionEpisode
		}
	}
	compatibleStates := compatibleSubscriptionEpisodeStates(normalizedStates, next)
	return transitionSubscriptionEpisodesByIntentSQL, []any{
		intentID,
		username,
		normalizedStates,
		next,
		compatibleStates,
	}, nil
}

func compatibleSubscriptionEpisodeStates(
	fromStates []string,
	next subscription.SubscriptionEpisodeState,
) []string {
	states := make([]subscription.SubscriptionEpisodeState, 0, 7)
	switch next {
	case subscription.SubscriptionEpisodeMissing:
		states = append(states, subscription.SubscriptionEpisodeMissing)
	case subscription.SubscriptionEpisodeAcquired:
		states = append(states,
			subscription.SubscriptionEpisodeAcquired,
			subscription.SubscriptionEpisodeDownloading,
			subscription.SubscriptionEpisodePendingOrganization,
			subscription.SubscriptionEpisodeOrganizing,
			subscription.SubscriptionEpisodePendingIndex,
			subscription.SubscriptionEpisodeIndexed,
		)
	case subscription.SubscriptionEpisodeDownloading:
		states = append(states,
			subscription.SubscriptionEpisodeDownloading,
			subscription.SubscriptionEpisodePendingOrganization,
			subscription.SubscriptionEpisodeOrganizing,
			subscription.SubscriptionEpisodePendingIndex,
			subscription.SubscriptionEpisodeIndexed,
		)
	case subscription.SubscriptionEpisodePendingOrganization:
		states = append(states,
			subscription.SubscriptionEpisodePendingOrganization,
			subscription.SubscriptionEpisodeOrganizing,
			subscription.SubscriptionEpisodePendingIndex,
			subscription.SubscriptionEpisodeIndexed,
		)
	case subscription.SubscriptionEpisodeOrganizing:
		states = append(states,
			subscription.SubscriptionEpisodeOrganizing,
			subscription.SubscriptionEpisodePendingIndex,
			subscription.SubscriptionEpisodeIndexed,
		)
	case subscription.SubscriptionEpisodePendingIndex:
		states = append(states,
			subscription.SubscriptionEpisodePendingIndex,
			subscription.SubscriptionEpisodeIndexed,
		)
	case subscription.SubscriptionEpisodeIndexed:
		states = append(states, subscription.SubscriptionEpisodeIndexed)
	}

	seen := make(map[string]struct{}, len(fromStates)+len(states))
	compatible := make([]string, 0, len(fromStates)+len(states))
	for _, state := range fromStates {
		if _, exists := seen[state]; exists {
			continue
		}
		seen[state] = struct{}{}
		compatible = append(compatible, state)
	}
	for _, state := range states {
		value := string(state)
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		compatible = append(compatible, value)
	}
	return compatible
}

func buildSummarizeSubscriptionEpisodes(
	username string,
	subscriptionID int64,
) (string, []any, error) {
	username = strings.TrimSpace(username)
	if username == "" || subscriptionID <= 0 {
		return "", nil, ErrInvalidSubscriptionEpisode
	}
	return summarizeSubscriptionEpisodesSQL, []any{subscriptionID, username}, nil
}

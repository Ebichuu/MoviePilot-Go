package database

import (
	"sort"
	"strings"

	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

// ValidateSubscriptionEpisodeTransition 校验逐集状态机的一次转换；同状态重放有效。
func ValidateSubscriptionEpisodeTransition(current, next subscription.SubscriptionEpisodeState) error {
	if !validSubscriptionEpisodeState(current) || !validSubscriptionEpisodeState(next) {
		return ErrInvalidSubscriptionEpisode
	}
	if current == next {
		return nil
	}
	allowed := false
	switch current {
	case subscription.SubscriptionEpisodeMissing:
		allowed = next == subscription.SubscriptionEpisodeAcquired
	case subscription.SubscriptionEpisodeAcquired:
		allowed = next == subscription.SubscriptionEpisodeDownloading || next == subscription.SubscriptionEpisodeMissing
	case subscription.SubscriptionEpisodeDownloading:
		allowed = next == subscription.SubscriptionEpisodePendingOrganization || next == subscription.SubscriptionEpisodeMissing
	case subscription.SubscriptionEpisodePendingOrganization:
		allowed = next == subscription.SubscriptionEpisodeOrganizing
	case subscription.SubscriptionEpisodeOrganizing:
		allowed = next == subscription.SubscriptionEpisodePendingIndex || next == subscription.SubscriptionEpisodeIndexed || next == subscription.SubscriptionEpisodePendingOrganization
	case subscription.SubscriptionEpisodePendingIndex:
		allowed = next == subscription.SubscriptionEpisodeIndexed
	case subscription.SubscriptionEpisodeIndexed:
		allowed = false
	}
	if !allowed {
		return ErrInvalidSubscriptionEpisode
	}
	return nil
}

func normalizeSubscriptionEpisodeInput(
	username string,
	subscriptionID int64,
	episodes []subscription.Episode,
) (string, []subscription.Episode, error) {
	username = strings.TrimSpace(username)
	if username == "" || subscriptionID <= 0 || len(episodes) == 0 || len(episodes) > maxSubscriptionEpisodes {
		return "", nil, ErrInvalidSubscriptionEpisode
	}
	normalized := append([]subscription.Episode(nil), episodes...)
	for _, episode := range normalized {
		if !validSubscriptionEpisode(episode) {
			return "", nil, ErrInvalidSubscriptionEpisode
		}
	}
	sort.Slice(normalized, func(i, j int) bool {
		if normalized[i].Season == normalized[j].Season {
			return normalized[i].Episode < normalized[j].Episode
		}
		return normalized[i].Season < normalized[j].Season
	})
	deduplicated := normalized[:0]
	for _, episode := range normalized {
		if len(deduplicated) > 0 && deduplicated[len(deduplicated)-1] == episode {
			continue
		}
		deduplicated = append(deduplicated, episode)
	}
	return username, deduplicated, nil
}

func subscriptionEpisodeArrays(episodes []subscription.Episode) ([]int32, []int32) {
	seasons := make([]int32, len(episodes))
	episodeNumbers := make([]int32, len(episodes))
	for index, episode := range episodes {
		seasons[index] = int32(episode.Season)
		episodeNumbers[index] = int32(episode.Episode)
	}
	return seasons, episodeNumbers
}

func validSubscriptionEpisode(episode subscription.Episode) bool {
	return episode.Season >= 0 && episode.Season <= maxSubscriptionEpisodeSeason &&
		episode.Episode > 0 && episode.Episode <= maxSubscriptionEpisodeNumber
}

func validSubscriptionEpisodeState(state subscription.SubscriptionEpisodeState) bool {
	switch state {
	case subscription.SubscriptionEpisodeMissing,
		subscription.SubscriptionEpisodeAcquired,
		subscription.SubscriptionEpisodeDownloading,
		subscription.SubscriptionEpisodePendingOrganization,
		subscription.SubscriptionEpisodeOrganizing,
		subscription.SubscriptionEpisodePendingIndex,
		subscription.SubscriptionEpisodeIndexed:
		return true
	default:
		return false
	}
}

func normalizeSubscriptionEpisodeStates(states []subscription.SubscriptionEpisodeState) ([]string, error) {
	if len(states) == 0 || len(states) > 7 {
		return nil, ErrInvalidSubscriptionEpisode
	}
	normalized := make([]string, 0, len(states))
	seen := make(map[subscription.SubscriptionEpisodeState]struct{}, len(states))
	for _, state := range states {
		if !validSubscriptionEpisodeState(state) {
			return nil, ErrInvalidSubscriptionEpisode
		}
		if _, exists := seen[state]; exists {
			continue
		}
		seen[state] = struct{}{}
		normalized = append(normalized, string(state))
	}
	return normalized, nil
}

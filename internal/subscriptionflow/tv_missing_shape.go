package subscriptionflow

import (
	"sort"

	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

// ordinaryTVFullSeasonMissing carries the empty-list shape before V2 expands
// it into coordinates. The native V2 providers and the current metadata
// boundary both expand counts from 1; synthetic non-1 inputs stay separate.
func ordinaryTVFullSeasonMissing(
	resolution subscription.TVTargetResolution,
	stored []subscription.SubscriptionEpisode,
	existing existingEpisodesResult,
) map[int]bool {
	result := make(map[int]bool)
	snapshot := resolution.Snapshot
	if resolution.Knowledge != subscription.TVTargetKnown || snapshot.BestVersion ||
		snapshot.StartEpisode > 1 || snapshot.TotalEpisode < 1 ||
		len(snapshot.Note) > 0 ||
		existing.ExplicitSeasons[snapshot.Season] {
		return result
	}
	for _, item := range stored {
		if item.Season == snapshot.Season && item.State != subscription.SubscriptionEpisodeMissing {
			return result
		}
	}
	episodes := append([]subscription.Episode(nil), existing.Episodes...)
	sort.Slice(episodes, func(i, j int) bool { return episodes[i].Episode < episodes[j].Episode })
	if v2TVEpisodesCoverRange(episodes, snapshot.Season, 1, snapshot.TotalEpisode) {
		result[snapshot.Season] = true
	}
	return result
}

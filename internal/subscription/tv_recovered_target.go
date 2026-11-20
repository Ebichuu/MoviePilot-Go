package subscription

import (
	"slices"
	"sort"
)

// TVRecoveredTarget freezes DownloadChain's local season total and missing
// episodes when the ordinary subscription itself still has total_episode=0.
// ExpectedCoverage is the selector's consumption, independent of file metadata
// coverage; V2's count-only whole-season pass can consume different coordinates.
type TVRecoveredTarget struct {
	TotalEpisode     int       `json:"total_episode"`
	MissingEpisodes  []Episode `json:"missing_episodes"`
	ExpectedCoverage []Episode `json:"expected_coverage"`
}

// NormalizeTVRecoveredTarget validates and copies a staged recovered target.
// Nil preserves the existing ordinary and best-version plan contracts.
func NormalizeTVRecoveredTarget(input *TVRecoveredTarget) (*TVRecoveredTarget, error) {
	if input == nil {
		return nil, nil
	}
	if input.TotalEpisode < 1 || input.TotalEpisode > maxTVTargetEpisode ||
		len(input.MissingEpisodes) == 0 || len(input.MissingEpisodes) > maxTVTargetFacts ||
		len(input.ExpectedCoverage) == 0 || len(input.ExpectedCoverage) > maxTVTargetFacts {
		return nil, ErrInvalidTVTarget
	}
	result := &TVRecoveredTarget{TotalEpisode: input.TotalEpisode}
	season := input.MissingEpisodes[0].Season
	if season < 0 || season > maxTVTargetSeason {
		return nil, ErrInvalidTVTarget
	}
	for _, pair := range []struct {
		source      []Episode
		destination *[]Episode
	}{
		{input.MissingEpisodes, &result.MissingEpisodes},
		{input.ExpectedCoverage, &result.ExpectedCoverage},
	} {
		for _, episode := range pair.source {
			if episode.Season != season || episode.Episode < 1 || episode.Episode > input.TotalEpisode {
				return nil, ErrInvalidTVTarget
			}
		}
		items := slices.Clone(pair.source)
		sort.Slice(items, func(i, j int) bool { return items[i].Episode < items[j].Episode })
		*pair.destination = slices.Compact(items)
	}
	for _, episode := range result.ExpectedCoverage {
		if !slices.Contains(result.MissingEpisodes, episode) {
			return nil, ErrInvalidTVTarget
		}
	}
	return result, nil
}

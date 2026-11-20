package subscription

import "sort"

// TVBestVersionCandidateDecision contains only quality admission facts. An
// accepted candidate still needs real torrent-file and download coverage checks.
type TVBestVersionCandidateDecision struct {
	Allowed            bool      `json:"allowed"`
	InterestedEpisodes []Episode `json:"interested_episodes"`
	PreferFullPack     bool      `json:"prefer_full_pack"`
}

// PrepareTVBestVersionCandidate applies V2's TV quality gate after filters have
// assigned priority. Empty candidateEpisodes means an unspecified episode range
// at admission time; it is not confirmed full coverage for the download writer.
func PrepareTVBestVersionCandidate(
	input TVTargetSnapshot,
	priority int,
	candidateEpisodes []Episode,
) (TVBestVersionCandidateDecision, error) {
	if err := validateTVTargetSnapshot(input); err != nil || len(candidateEpisodes) > maxTVTargetFacts {
		return TVBestVersionCandidateDecision{}, ErrInvalidTVTarget
	}
	decision := TVBestVersionCandidateDecision{InterestedEpisodes: []Episode{}}
	if !input.BestVersion {
		decision.Allowed = true
		return decision, nil
	}
	if input.BestVersionFull {
		baseline := 0
		if input.CurrentPriority != nil {
			baseline = *input.CurrentPriority
		}
		decision.Allowed = priority > baseline
		return decision, nil
	}

	facts, err := DescribeTVBestVersion(input)
	if err != nil {
		return TVBestVersionCandidateDecision{}, err
	}
	available := make(map[Episode]struct{}, len(candidateEpisodes))
	for _, episode := range candidateEpisodes {
		available[episode] = struct{}{}
	}
	targets := tvTargetEpisodes(input)
	decision.PreferFullPack = len(targets) > 0
	for _, episode := range targets {
		baseline, known := facts.EpisodePriority[episode.Episode]
		if priority <= baseline {
			decision.PreferFullPack = false
		}
		if len(candidateEpisodes) > 0 {
			if _, exists := available[episode]; !exists {
				continue
			}
		}
		if !known || priority > baseline {
			decision.InterestedEpisodes = append(decision.InterestedEpisodes, episode)
		}
	}
	decision.Allowed = len(decision.InterestedEpisodes) > 0
	return decision, nil
}

// RecordTVDownloadPriority records one accepted download's real selected or
// confirmed episodes. Nil priority records note only; it never invents quality
// for a legacy plan. Ordinary and per-episode modes leave CurrentPriority and
// LackEpisode to their separate progress entry points, as V2 does.
//
// The caller resolves selected episodes before metadata episodes, as V2 does.
// When both are empty, a confirmed full-coverage download falls back to the
// current target. Full-mode overall quality trusts that confirmation flag.
func RecordTVDownloadPriority(
	input TVTargetSnapshot,
	priority *int,
	actual []Episode,
	fullCoverage bool,
) (TVTargetSnapshot, error) {
	if err := validateTVTargetSnapshot(input); err != nil || len(actual) > maxTVTargetFacts {
		return TVTargetSnapshot{}, ErrInvalidTVTarget
	}
	result := cloneTVTargetSnapshot(input)
	targets := tvTargetEpisodes(input)
	if len(actual) == 0 && fullCoverage {
		actual = targets
	}
	allowed := make(map[Episode]struct{}, len(targets))
	for _, episode := range targets {
		allowed[episode] = struct{}{}
	}
	covered := make(map[Episode]struct{}, len(actual))
	for _, episode := range actual {
		// V2's writer receives episode numbers and applies the current
		// subscription range; season matching belongs to candidate selection.
		episode.Season = input.Season
		if _, exists := allowed[episode]; exists {
			covered[episode] = struct{}{}
		}
	}
	if len(covered) == 0 {
		return result, nil
	}

	facts, err := DescribeTVBestVersion(input)
	if err != nil {
		return TVTargetSnapshot{}, err
	}
	notes := make(map[int]struct{}, len(input.Note)+len(covered))
	for _, episode := range input.Note {
		// V2's persisted note union keeps only str(episode).isdigit() values.
		if episode >= 0 {
			notes[episode] = struct{}{}
		}
	}
	for episode := range covered {
		notes[episode.Episode] = struct{}{}
		if priority == nil {
			continue
		}
		baseline, known := facts.EpisodePriority[episode.Episode]
		if !known || *priority > baseline {
			facts.EpisodePriority[episode.Episode] = *priority
		}
	}
	if len(notes) > maxTVTargetFacts || len(facts.EpisodePriority) > maxTVTargetFacts {
		return TVTargetSnapshot{}, ErrInvalidTVTarget
	}
	result.Note = make([]int, 0, len(notes))
	for episode := range notes {
		result.Note = append(result.Note, episode)
	}
	sort.Ints(result.Note)
	result.EpisodePriority = facts.EpisodePriority

	if input.BestVersion && input.BestVersionFull && fullCoverage && priority != nil {
		baseline := 0
		if input.CurrentPriority != nil {
			baseline = *input.CurrentPriority
		}
		if *priority > baseline {
			acceptedPriority := *priority
			result.CurrentPriority = &acceptedPriority
		}
	}
	return result, nil
}

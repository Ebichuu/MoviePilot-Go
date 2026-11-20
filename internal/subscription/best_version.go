package subscription

// TVBestVersionFacts keeps any-version download progress separate from the
// priority-100 completion rule. It contains no downloader or persisted state.
type TVBestVersionFacts struct {
	EpisodePriority    map[int]int `json:"episode_priority"`
	DownloadedEpisodes []Episode   `json:"downloaded_episodes"`
	PendingEpisodes    []Episode   `json:"pending_episodes"`
	CurrentPriority    int         `json:"current_priority"`
	Complete           bool        `json:"complete"`
}

// DescribeTVBestVersion derives V2 quality facts without mutating the input.
// Only an empty per-episode upgrade snapshot inherits a legacy overall quality.
func DescribeTVBestVersion(input TVTargetSnapshot) (TVBestVersionFacts, error) {
	if err := validateTVTargetSnapshot(input); err != nil {
		return TVBestVersionFacts{}, err
	}
	return describeTVBestVersion(input), nil
}

func describeTVBestVersion(input TVTargetSnapshot) TVBestVersionFacts {
	priorities := effectiveTVEpisodePriority(input, input.TotalEpisode)
	targets := tvTargetEpisodes(input)
	facts := TVBestVersionFacts{
		EpisodePriority:    priorities,
		DownloadedEpisodes: make([]Episode, 0, len(targets)),
		PendingEpisodes:    make([]Episode, 0, len(targets)),
	}
	if input.CurrentPriority != nil {
		facts.CurrentPriority = *input.CurrentPriority
	}
	notes := make(map[int]struct{}, len(input.Note))
	for _, episode := range input.Note {
		notes[episode] = struct{}{}
	}
	for _, episode := range targets {
		_, noted := notes[episode.Episode]
		if noted || priorities[episode.Episode] > 0 {
			facts.DownloadedEpisodes = append(facts.DownloadedEpisodes, episode)
		}
		if priorities[episode.Episode] != 100 {
			facts.PendingEpisodes = append(facts.PendingEpisodes, episode)
		}
	}
	if input.BestVersion && !input.BestVersionFull && len(targets) > 0 {
		facts.CurrentPriority = minimumTVTargetPriority(input, priorities)
	}
	if input.BestVersion {
		if input.BestVersionFull || len(targets) == 0 {
			facts.Complete = input.CurrentPriority != nil && *input.CurrentPriority == 100
		} else {
			facts.Complete = len(facts.PendingEpisodes) == 0
		}
	}
	return facts
}

// PendingTVBestVersionEpisodes uses an explicit override when it is non-nil.
// An empty override remains empty; it must not inherit the stored priorities.
func PendingTVBestVersionEpisodes(input TVTargetSnapshot, override map[int]int) ([]Episode, error) {
	if err := validateTVTargetSnapshot(input); err != nil || len(override) > maxTVTargetFacts {
		return nil, ErrInvalidTVTarget
	}
	priorities := override
	if priorities == nil {
		priorities = effectiveTVEpisodePriority(input, input.TotalEpisode)
	}
	pending := make([]Episode, 0)
	for _, episode := range tvTargetEpisodes(input) {
		if priorities[episode.Episode] != 100 {
			pending = append(pending, episode)
		}
	}
	return pending, nil
}

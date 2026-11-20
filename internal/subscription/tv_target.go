package subscription

import (
	"errors"
	"reflect"
	"sort"
)

const (
	maxTVTargetSeason  = 999
	maxTVTargetEpisode = 5000
	maxTVTargetFacts   = 10_000
)

var ErrInvalidTVTarget = errors.New("电视剧订阅目标参数无效")

// TVTargetKnowledge distinguishes an unknown total from a known target whose
// effective episode range is empty (for example start_episode > total_episode).
type TVTargetKnowledge string

const (
	TVTargetUnknown TVTargetKnowledge = "unknown"
	TVTargetKnown   TVTargetKnowledge = "known"
)

// TVTargetSnapshot contains only the V2 facts needed to resolve a TV target.
// Download-intent state is deliberately absent: an accepted plan owns an
// immutable episode snapshot and is coordinated by the persistence layer.
type TVTargetSnapshot struct {
	Season             int         `json:"season"`
	StartEpisode       int         `json:"start_episode"`
	TotalEpisode       int         `json:"total_episode"`
	ManualTotalEpisode bool        `json:"manual_total_episode"`
	LackEpisode        int         `json:"lack_episode"`
	Note               []int       `json:"note"`
	BestVersion        bool        `json:"best_version"`
	BestVersionFull    bool        `json:"best_version_full"`
	CurrentPriority    *int        `json:"current_priority"`
	EpisodePriority    map[int]int `json:"episode_priority"`
}

// TVTargetResolution is the pure result consumed by later persistence and
// flow slices. TargetEpisodes is empty for both unknown and known-empty
// targets; Knowledge keeps those cases separate.
type TVTargetResolution struct {
	Snapshot       TVTargetSnapshot  `json:"snapshot"`
	Knowledge      TVTargetKnowledge `json:"knowledge"`
	TargetEpisodes []Episode         `json:"target_episodes"`
	Changed        bool              `json:"changed"`
}

// TVPublicTargetUpdate models the target fields accepted by V2's public
// subscription update. Nil means the field was not part of this domain change.
type TVPublicTargetUpdate struct {
	Season       *int `json:"season"`
	StartEpisode *int `json:"start_episode"`
	TotalEpisode *int `json:"total_episode"`
}

// TVTargetShrinkEvidence is the bounded result of checking the old target
// before accepting a metadata decrease. Unavailable or malformed evidence
// falls back to the metadata candidate, matching V2.
type TVTargetShrinkEvidence struct {
	Available       bool  `json:"available"`
	Satisfied       bool  `json:"satisfied"`
	MissingEpisodes []int `json:"missing_episodes"`
}

// ResolveTVCreationTarget applies V2's creation rule: a positive requested
// total wins; otherwise a positive metadata total is used. A zero result is an
// explicit unknown target which the API layer may reject for identified TV.
func ResolveTVCreationTarget(input TVTargetSnapshot, metadataTotal int) (TVTargetResolution, error) {
	if err := validateTVTargetSnapshot(input); err != nil || metadataTotal < 0 || metadataTotal > maxTVTargetEpisode {
		return TVTargetResolution{}, ErrInvalidTVTarget
	}
	before := cloneTVTargetSnapshot(input)
	result := cloneTVTargetSnapshot(input)
	if result.TotalEpisode <= 0 && metadataTotal > 0 {
		result.TotalEpisode = metadataTotal
	}
	if result.LackEpisode == 0 && result.TotalEpisode > 0 {
		result.LackEpisode = result.TotalEpisode
	}
	return resolveTVTarget(before, result), nil
}

// ApplyTVPublicTargetUpdate preserves V2's public update quirk: changing total
// locks automatic refresh, and only an increase immediately adds its delta to
// lack_episode. Season/start changes do not erase download facts.
func ApplyTVPublicTargetUpdate(input TVTargetSnapshot, update TVPublicTargetUpdate) (TVTargetResolution, error) {
	if err := validateTVTargetSnapshot(input); err != nil || !validOptionalTVTargetFields(update) {
		return TVTargetResolution{}, ErrInvalidTVTarget
	}
	before := cloneTVTargetSnapshot(input)
	result := cloneTVTargetSnapshot(input)
	if update.Season != nil {
		result.Season = *update.Season
	}
	if update.StartEpisode != nil {
		result.StartEpisode = *update.StartEpisode
	}
	if update.TotalEpisode != nil && *update.TotalEpisode != result.TotalEpisode {
		oldTotal := result.TotalEpisode
		result.TotalEpisode = *update.TotalEpisode
		result.ManualTotalEpisode = true
		if result.TotalEpisode > 0 && result.TotalEpisode > oldTotal {
			if result.LackEpisode > maxTVTargetFacts-(result.TotalEpisode-oldTotal) {
				return TVTargetResolution{}, ErrInvalidTVTarget
			}
			result.LackEpisode += result.TotalEpisode - oldTotal
		}
	}
	return resolveTVTarget(before, result), nil
}

// ResolveTVAutomaticTarget follows a positive metadata total only while the
// subscription is not manually locked. It performs the V2 total-change
// calculations but does not mutate any download intent or persisted episode.
func ResolveTVAutomaticTarget(
	input TVTargetSnapshot,
	metadataTotal int,
	evidence TVTargetShrinkEvidence,
) (TVTargetResolution, error) {
	if err := validateTVTargetSnapshot(input); err != nil || metadataTotal < 0 || metadataTotal > maxTVTargetEpisode {
		return TVTargetResolution{}, ErrInvalidTVTarget
	}
	before := cloneTVTargetSnapshot(input)
	result := cloneTVTargetSnapshot(input)
	if result.ManualTotalEpisode || metadataTotal == 0 {
		return resolveTVTarget(before, result), nil
	}

	newTotal := metadataTotal
	if newTotal < result.TotalEpisode && !(result.BestVersion && result.BestVersionFull) {
		newTotal = resolveTVTargetDecrease(result, newTotal, evidence)
	}
	if newTotal == result.TotalEpisode {
		return resolveTVTarget(before, result), nil
	}

	oldTotal := result.TotalEpisode
	if !result.BestVersion {
		delta := newTotal - oldTotal
		if delta > 0 && result.LackEpisode > maxTVTargetFacts-delta {
			return TVTargetResolution{}, ErrInvalidTVTarget
		}
		result.TotalEpisode = newTotal
		result.LackEpisode = max(result.LackEpisode+delta, 0)
		return resolveTVTarget(before, result), nil
	}

	priorities := effectiveTVEpisodePriority(result, oldTotal)
	result.TotalEpisode = newTotal
	priorities = filterTVEpisodePriority(priorities, result)
	result.EpisodePriority = priorities
	if result.BestVersionFull {
		if newTotal > oldTotal {
			zero := 0
			result.CurrentPriority = &zero
		}
	} else {
		priority := minimumTVTargetPriority(result, priorities)
		result.CurrentPriority = &priority
	}
	result.LackEpisode = bestVersionTVLackEpisode(result)
	return resolveTVTarget(before, result), nil
}

// ResetTVTarget applies the target-related portion of V2's reset endpoint.
func ResetTVTarget(input TVTargetSnapshot) (TVTargetResolution, error) {
	if err := validateTVTargetSnapshot(input); err != nil {
		return TVTargetResolution{}, err
	}
	before := cloneTVTargetSnapshot(input)
	result := cloneTVTargetSnapshot(input)
	result.ManualTotalEpisode = false
	result.LackEpisode = result.TotalEpisode
	result.Note = []int{}
	result.CurrentPriority = nil
	result.EpisodePriority = map[int]int{}
	return resolveTVTarget(before, result), nil
}

// DescribeTVTarget resolves the effective target without changing its facts.
func DescribeTVTarget(input TVTargetSnapshot) (TVTargetResolution, error) {
	if err := validateTVTargetSnapshot(input); err != nil {
		return TVTargetResolution{}, err
	}
	copy := cloneTVTargetSnapshot(input)
	return resolveTVTarget(copy, copy), nil
}

func resolveTVTarget(before, after TVTargetSnapshot) TVTargetResolution {
	episodes := tvTargetEpisodes(after)
	return TVTargetResolution{
		Snapshot:       after,
		Knowledge:      tvTargetKnowledge(after),
		TargetEpisodes: episodes,
		Changed:        !reflect.DeepEqual(before, after),
	}
}

func tvTargetKnowledge(target TVTargetSnapshot) TVTargetKnowledge {
	if target.TotalEpisode > 0 {
		return TVTargetKnown
	}
	return TVTargetUnknown
}

func tvTargetEpisodes(target TVTargetSnapshot) []Episode {
	start := target.StartEpisode
	if start <= 0 {
		start = 1
	}
	episodes := make([]Episode, 0)
	if target.TotalEpisode <= 0 || start > target.TotalEpisode {
		return episodes
	}
	for episode := start; episode <= target.TotalEpisode; episode++ {
		episodes = append(episodes, Episode{Season: target.Season, Episode: episode})
	}
	return episodes
}

func resolveTVTargetDecrease(input TVTargetSnapshot, candidate int, evidence TVTargetShrinkEvidence) int {
	if !evidence.Available {
		return candidate
	}
	if evidence.Satisfied {
		return input.TotalEpisode
	}
	start := input.StartEpisode
	if start <= 0 {
		start = 1
	}
	if len(evidence.MissingEpisodes) == 0 {
		return candidate
	}
	missing := make(map[int]struct{}, len(evidence.MissingEpisodes))
	for _, episode := range evidence.MissingEpisodes {
		if episode < start || episode > input.TotalEpisode {
			return candidate
		}
		missing[episode] = struct{}{}
	}
	confirmedMaximum := 0
	for episode := start; episode <= input.TotalEpisode; episode++ {
		if _, absent := missing[episode]; !absent {
			confirmedMaximum = episode
		}
	}
	return max(candidate, confirmedMaximum)
}

func effectiveTVEpisodePriority(input TVTargetSnapshot, total int) map[int]int {
	priorities := make(map[int]int, len(input.EpisodePriority))
	for episode, priority := range input.EpisodePriority {
		priorities[episode] = priority
	}
	if len(priorities) > 0 || !input.BestVersion || input.BestVersionFull || input.CurrentPriority == nil {
		return priorities
	}
	legacy := input
	legacy.TotalEpisode = total
	for _, episode := range tvTargetEpisodes(legacy) {
		priorities[episode.Episode] = *input.CurrentPriority
	}
	return priorities
}

func filterTVEpisodePriority(priorities map[int]int, target TVTargetSnapshot) map[int]int {
	allowed := make(map[int]struct{})
	for _, episode := range tvTargetEpisodes(target) {
		allowed[episode.Episode] = struct{}{}
	}
	filtered := make(map[int]int)
	for episode, priority := range priorities {
		if _, ok := allowed[episode]; ok {
			filtered[episode] = priority
		}
	}
	return filtered
}

func minimumTVTargetPriority(target TVTargetSnapshot, priorities map[int]int) int {
	episodes := tvTargetEpisodes(target)
	if len(episodes) == 0 {
		return 0
	}
	minimum := 0
	for index, episode := range episodes {
		priority := priorities[episode.Episode]
		if index == 0 || priority < minimum {
			minimum = priority
		}
	}
	return minimum
}

func bestVersionTVLackEpisode(target TVTargetSnapshot) int {
	// The automatic-target resolver validates the snapshot before this call.
	facts := describeTVBestVersion(target)
	return len(tvTargetEpisodes(target)) - len(facts.DownloadedEpisodes)
}

// TargetNoteEpisodes returns the sorted note facts inside the current target.
// It lets later flow code ignore facts from a previous season/range without
// mutating V2's persisted note field during a boundary-only update.
func TargetNoteEpisodes(target TVTargetSnapshot) ([]Episode, error) {
	if err := validateTVTargetSnapshot(target); err != nil {
		return nil, err
	}
	allowed := make(map[int]struct{})
	for _, episode := range tvTargetEpisodes(target) {
		allowed[episode.Episode] = struct{}{}
	}
	seen := make(map[int]struct{})
	for _, episode := range target.Note {
		if _, ok := allowed[episode]; ok {
			seen[episode] = struct{}{}
		}
	}
	numbers := make([]int, 0, len(seen))
	for episode := range seen {
		numbers = append(numbers, episode)
	}
	sort.Ints(numbers)
	result := make([]Episode, 0, len(numbers))
	for _, episode := range numbers {
		result = append(result, Episode{Season: target.Season, Episode: episode})
	}
	return result, nil
}

func validOptionalTVTargetFields(update TVPublicTargetUpdate) bool {
	return (update.Season == nil || (*update.Season >= 0 && *update.Season <= maxTVTargetSeason)) &&
		(update.StartEpisode == nil || (*update.StartEpisode >= 0 && *update.StartEpisode <= maxTVTargetEpisode)) &&
		(update.TotalEpisode == nil || (*update.TotalEpisode >= 0 && *update.TotalEpisode <= maxTVTargetEpisode))
}

func validateTVTargetSnapshot(target TVTargetSnapshot) error {
	if target.Season < 0 || target.Season > maxTVTargetSeason ||
		target.StartEpisode < 0 || target.StartEpisode > maxTVTargetEpisode ||
		target.TotalEpisode < 0 || target.TotalEpisode > maxTVTargetEpisode ||
		target.LackEpisode < 0 || target.LackEpisode > maxTVTargetFacts ||
		len(target.Note) > maxTVTargetFacts || len(target.EpisodePriority) > maxTVTargetFacts {
		return ErrInvalidTVTarget
	}
	return nil
}

func cloneTVTargetSnapshot(input TVTargetSnapshot) TVTargetSnapshot {
	result := input
	result.Note = append([]int{}, input.Note...)
	result.EpisodePriority = make(map[int]int, len(input.EpisodePriority))
	for episode, priority := range input.EpisodePriority {
		result.EpisodePriority[episode] = priority
	}
	if input.CurrentPriority != nil {
		priority := *input.CurrentPriority
		result.CurrentPriority = &priority
	}
	return result
}

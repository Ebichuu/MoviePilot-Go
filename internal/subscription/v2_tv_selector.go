package subscription

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/moviepilot-go/moviepilot-go/internal/media"
)

const (
	V2TVSubscriptionStateCompleted = ""
	V2TVSubscriptionStateReady     = "R"
	V2TVSubscriptionStatePending   = "P"

	maxV2TVSeasons    = 1_000
	maxV2TVCandidates = 10_000
)

var ErrInvalidV2TVSelection = errors.New("invalid V2 TV candidate selection")

// V2TVStage identifies one pass of the V2 TV greedy selector.
type V2TVStage string

const (
	V2TVStageFullSeason    V2TVStage = "full_season"
	V2TVStageTitleEpisodes V2TVStage = "title_episodes"
	V2TVStageUnpack        V2TVStage = "unpack"
)

// V2TVSeasonNeed describes one season's current missing-episode facts.
// FullSeason preserves V2's empty missing-episode-list sentinel. An explicit
// range may contain every episode without carrying this sentinel.
type V2TVSeasonNeed struct {
	Season                  int
	MissingEpisodes         []Episode
	TotalEpisodes           int
	FullSeason              bool
	RequireCompleteCoverage bool
}

// V2TVCandidate is an already sorted candidate. The selector never reorders it.
// An empty TitleEpisodes slice means that the title has no explicit episode.
type V2TVCandidate struct {
	Key           string
	Seasons       []int
	TitleEpisodes []Episode
	// AllowedEpisodes is the frozen V2 quality gate for this batch. Nil leaves
	// the need unconstrained; an empty slice explicitly allows no episodes.
	AllowedEpisodes []Episode
}

// NewV2TVCandidate projects a filtered candidate without narrowing the title's
// explicit episode range to the current missing set.
func NewV2TVCandidate(candidate Candidate) (V2TVCandidate, error) {
	seasons := candidate.Parsed.SeasonList()
	if candidate.Parsed.BeginSeason == 0 && explicitSeasonZero(candidate.Resource.Title) {
		seasons = []int{0}
	}
	titleEpisodes, err := V2TVEpisodeCoordinates(candidate.Resource.Title, candidate.Parsed)
	if err != nil {
		return V2TVCandidate{}, err
	}
	return normalizeV2TVCandidate(V2TVCandidate{
		Key: candidate.Key, Seasons: seasons, TitleEpisodes: titleEpisodes,
	})
}

// V2TVEpisodeCoordinates expands one parsed title into season/episode pairs.
func V2TVEpisodeCoordinates(title string, parsed media.ParseResult) ([]Episode, error) {
	episodeNumbers, err := parsed.EpisodeList()
	if err != nil {
		return nil, ErrInvalidV2TVSelection
	}
	if len(episodeNumbers) == 0 {
		return []Episode{}, nil
	}
	seasons := parsed.SeasonList()
	if parsed.BeginSeason == 0 && explicitSeasonZero(title) {
		seasons = []int{0}
	}
	if len(seasons) == 0 || len(seasons) > maxV2TVSeasons ||
		len(episodeNumbers) > 10_000/len(seasons) {
		return nil, ErrInvalidV2TVSelection
	}
	result := make([]Episode, 0, len(seasons)*len(episodeNumbers))
	for _, season := range seasons {
		for _, episode := range episodeNumbers {
			result = append(result, Episode{Season: season, Episode: episode})
		}
	}
	seasonSet := make(map[int]struct{}, len(seasons))
	for _, season := range seasons {
		seasonSet[season] = struct{}{}
	}
	return normalizeV2TVEpisodes(result, seasonSet)
}

// V2TVEpisodeProjection is the bounded episode projection of real torrent files.
// Failed marks an explicit candidate failure; Stop terminates the whole selection.
type V2TVEpisodeProjection struct {
	Episodes []Episode
	Failed   bool
	Stop     bool
}

// V2TVCandidateAttempt describes exactly one serial downloader submission.
// SelectedEpisodes is empty for a whole-torrent Add.
type V2TVCandidateAttempt struct {
	Candidate        V2TVCandidate
	Stage            V2TVStage
	SelectedEpisodes []Episode
	ExpectedCoverage []Episode
}

// V2TVCandidateAttemptResult separates an explicit miss from an accepted Hash
// and from a terminal result. CoveredEpisodes must be confirmed durable facts.
type V2TVCandidateAttemptResult struct {
	HashAcquired    bool
	CoveredEpisodes []Episode
	Stop            bool
}

type V2TVProjectEpisodesFunc func(context.Context, V2TVCandidate) (V2TVEpisodeProjection, error)

type V2TVAttemptCandidateFunc func(context.Context, V2TVCandidateAttempt) (V2TVCandidateAttemptResult, error)

type V2TVSelectionRequest struct {
	SubscriptionState string
	Seasons           []V2TVSeasonNeed
	Candidates        []V2TVCandidate
}

type V2TVSelectionResult struct {
	SubscriptionState     string
	AcceptedCandidateKeys []string
	RemainingEpisodes     []Episode
	Stopped               bool
}

// SelectV2TVCandidates runs V2's three serial greedy passes over candidates in
// caller-provided order. It performs no I/O except through the two callbacks.
func SelectV2TVCandidates(
	ctx context.Context,
	request V2TVSelectionRequest,
	project V2TVProjectEpisodesFunc,
	attempt V2TVAttemptCandidateFunc,
) (V2TVSelectionResult, error) {
	selection, err := prepareV2TVSelection(ctx, request, project, attempt)
	if err != nil {
		return V2TVSelectionResult{}, err
	}
	if len(selection.remaining) == 0 {
		return selection.result(false), nil
	}

	for index, candidate := range selection.candidates {
		if len(candidate.TitleEpisodes) != 0 || selection.exhausted[index] {
			continue
		}
		expected, eligible := selection.fullSeasonCoverage(candidate)
		if !eligible {
			continue
		}
		if len(candidate.Seasons) == 1 {
			projection, stop, projectionErr := selection.project(index)
			if projectionErr != nil || stop {
				return selection.result(true), projectionErr
			}
			if projection.Failed {
				selection.exhausted[index] = true
				continue
			}
			if len(projection.Episodes) > 0 {
				// V2 replaces a single-season pack's metadata by the real-file
				// minimum/maximum before the count or complete-coverage check.
				selection.candidates[index].TitleEpisodes = V2TVProjectedTitleEpisodes(candidate.Seasons[0], projection.Episodes)
			}
			if !selection.fullSeasonProjectionMatches(candidate.Seasons[0], projection.Episodes) {
				continue
			}
		}
		stop, attemptErr := selection.attempt(index, V2TVStageFullSeason, nil, expected)
		if attemptErr != nil || stop {
			return selection.result(true), attemptErr
		}
		if len(selection.remaining) == 0 {
			return selection.result(false), nil
		}
	}

	for index, candidate := range selection.candidates {
		if len(candidate.TitleEpisodes) == 0 || len(candidate.Seasons) != 1 || selection.exhausted[index] {
			continue
		}
		expected, eligible := selection.titleEpisodeCoverage(candidate)
		if !eligible {
			continue
		}
		stop, attemptErr := selection.attempt(
			index,
			V2TVStageTitleEpisodes,
			nil,
			expected,
		)
		if attemptErr != nil || stop {
			return selection.result(true), attemptErr
		}
		if len(selection.remaining) == 0 {
			return selection.result(false), nil
		}
	}

	for index, candidate := range selection.candidates {
		if selection.exhausted[index] || len(candidate.Seasons) != 1 {
			continue
		}
		season := candidate.Seasons[0]
		need := selection.needs[season]
		if need == nil || need.RequireCompleteCoverage || !selection.hasRemainingSeason(season) {
			continue
		}
		if len(candidate.TitleEpisodes) > 0 && !episodesIntersect(candidate.TitleEpisodes, selection.remaining) {
			continue
		}
		projection, stop, projectionErr := selection.project(index)
		if projectionErr != nil || stop {
			return selection.result(true), projectionErr
		}
		if projection.Failed {
			selection.exhausted[index] = true
			continue
		}
		selected := episodeIntersection(projection.Episodes, selection.effectiveNeed(candidate))
		if len(selected) == 0 {
			continue
		}
		stop, attemptErr := selection.attempt(index, V2TVStageUnpack, selected, selected)
		if attemptErr != nil || stop {
			return selection.result(true), attemptErr
		}
		if len(selection.remaining) == 0 {
			return selection.result(false), nil
		}
	}

	return selection.result(false), nil
}

// V2TVProjectedTitleEpisodes reproduces MetaBase.set_episodes(min, max) after
// single-season torrent inspection. It intentionally preserves the whole range.
func V2TVProjectedTitleEpisodes(season int, projected []Episode) []Episode {
	if len(projected) == 0 {
		return []Episode{}
	}
	begin, end := projected[0].Episode, projected[0].Episode
	for _, episode := range projected[1:] {
		begin = min(begin, episode.Episode)
		end = max(end, episode.Episode)
	}
	result := make([]Episode, 0, end-begin+1)
	for episode := begin; episode <= end; episode++ {
		result = append(result, Episode{Season: season, Episode: episode})
	}
	return result
}

type preparedV2TVSelection struct {
	ctx        context.Context
	state      string
	needs      map[int]*V2TVSeasonNeed
	candidates []V2TVCandidate
	remaining  map[Episode]struct{}
	exhausted  []bool
	accepted   []string
	projector  V2TVProjectEpisodesFunc
	attempter  V2TVAttemptCandidateFunc
}

func prepareV2TVSelection(
	ctx context.Context,
	request V2TVSelectionRequest,
	project V2TVProjectEpisodesFunc,
	attempt V2TVAttemptCandidateFunc,
) (*preparedV2TVSelection, error) {
	if ctx == nil || project == nil || attempt == nil ||
		(request.SubscriptionState != V2TVSubscriptionStateReady && request.SubscriptionState != V2TVSubscriptionStatePending) ||
		len(request.Seasons) > maxV2TVSeasons || len(request.Candidates) > maxV2TVCandidates {
		return nil, ErrInvalidV2TVSelection
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	selection := &preparedV2TVSelection{
		ctx: ctx, state: request.SubscriptionState,
		needs:      make(map[int]*V2TVSeasonNeed, len(request.Seasons)),
		remaining:  make(map[Episode]struct{}),
		candidates: make([]V2TVCandidate, len(request.Candidates)),
		exhausted:  make([]bool, len(request.Candidates)),
		accepted:   []string{}, projector: project, attempter: attempt,
	}
	for _, input := range request.Seasons {
		if input.Season < 0 || input.Season > 999 || input.TotalEpisodes < 0 || input.TotalEpisodes > 5000 {
			return nil, ErrInvalidV2TVSelection
		}
		if _, exists := selection.needs[input.Season]; exists {
			return nil, ErrInvalidV2TVSelection
		}
		missing, err := normalizeV2TVEpisodes(input.MissingEpisodes, map[int]struct{}{input.Season: {}})
		if err != nil {
			return nil, err
		}
		need := input
		need.MissingEpisodes = missing
		selection.needs[input.Season] = &need
		for _, episode := range missing {
			selection.remaining[episode] = struct{}{}
		}
	}
	for index, input := range request.Candidates {
		candidate, err := normalizeV2TVCandidate(input)
		if err != nil {
			return nil, err
		}
		selection.candidates[index] = candidate
	}
	return selection, nil
}

func normalizeV2TVCandidate(input V2TVCandidate) (V2TVCandidate, error) {
	if strings.TrimSpace(input.Key) == "" || len(input.Seasons) == 0 || len(input.Seasons) > maxV2TVSeasons {
		return V2TVCandidate{}, ErrInvalidV2TVSelection
	}
	seasonSet := make(map[int]struct{}, len(input.Seasons))
	seasons := make([]int, 0, len(input.Seasons))
	for _, season := range input.Seasons {
		if season < 0 || season > 999 {
			return V2TVCandidate{}, ErrInvalidV2TVSelection
		}
		if _, exists := seasonSet[season]; exists {
			return V2TVCandidate{}, ErrInvalidV2TVSelection
		}
		seasonSet[season] = struct{}{}
		seasons = append(seasons, season)
	}
	sort.Ints(seasons)
	titleEpisodes, err := normalizeV2TVEpisodes(input.TitleEpisodes, seasonSet)
	if err != nil {
		return V2TVCandidate{}, err
	}
	var allowedEpisodes []Episode
	if input.AllowedEpisodes != nil {
		allowedEpisodes, err = normalizeV2TVEpisodes(input.AllowedEpisodes, seasonSet)
		if err != nil {
			return V2TVCandidate{}, err
		}
	}
	return V2TVCandidate{Key: input.Key, Seasons: seasons, TitleEpisodes: titleEpisodes, AllowedEpisodes: allowedEpisodes}, nil
}

func normalizeV2TVEpisodes(items []Episode, seasons map[int]struct{}) ([]Episode, error) {
	if len(items) > 10_000 {
		return nil, ErrInvalidV2TVSelection
	}
	seen := make(map[Episode]struct{}, len(items))
	result := make([]Episode, 0, len(items))
	for _, item := range items {
		if item.Season < 0 || item.Season > 999 || item.Episode <= 0 || item.Episode > 5000 {
			return nil, ErrInvalidV2TVSelection
		}
		if seasons != nil {
			if _, exists := seasons[item.Season]; !exists {
				return nil, ErrInvalidV2TVSelection
			}
		}
		if _, exists := seen[item]; exists {
			continue
		}
		seen[item] = struct{}{}
		result = append(result, item)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Season != result[j].Season {
			return result[i].Season < result[j].Season
		}
		return result[i].Episode < result[j].Episode
	})
	if result == nil {
		return []Episode{}, nil
	}
	return result, nil
}

func (selection *preparedV2TVSelection) fullSeasonCoverage(candidate V2TVCandidate) ([]Episode, bool) {
	for _, season := range candidate.Seasons {
		need := selection.needs[season]
		if need == nil || !need.FullSeason || !selection.hasRemainingSeason(season) {
			return nil, false
		}
	}
	expected := make([]Episode, 0)
	for episode := range selection.remaining {
		if containsV2TVSeason(candidate.Seasons, episode.Season) {
			expected = append(expected, episode)
		}
	}
	sortV2TVEpisodes(expected)
	return expected, len(expected) > 0
}

func (selection *preparedV2TVSelection) fullSeasonProjectionMatches(season int, projected []Episode) bool {
	need := selection.needs[season]
	if need == nil || len(projected) == 0 {
		return false
	}
	projectedSet := make(map[Episode]struct{}, len(projected))
	for _, episode := range projected {
		if episode.Season == season {
			projectedSet[episode] = struct{}{}
		}
	}
	if need.RequireCompleteCoverage {
		for _, required := range need.MissingEpisodes {
			if _, exists := projectedSet[required]; !exists {
				return false
			}
		}
		return len(need.MissingEpisodes) > 0
	}
	return need.TotalEpisodes == 0 || len(projectedSet) >= need.TotalEpisodes
}

func (selection *preparedV2TVSelection) titleEpisodeCoverage(candidate V2TVCandidate) ([]Episode, bool) {
	need := selection.needs[candidate.Seasons[0]]
	if need == nil {
		return nil, false
	}
	if len(selection.effectiveNeed(candidate)) == 0 {
		return nil, false
	}
	if need.RequireCompleteCoverage {
		if !episodesSubsetOfSlice(need.MissingEpisodes, candidate.TitleEpisodes) {
			return nil, false
		}
		expected := episodeIntersection(need.MissingEpisodes, selection.remaining)
		return expected, len(expected) > 0
	}
	if !episodesSubsetOf(candidate.TitleEpisodes, selection.effectiveNeed(candidate)) {
		return nil, false
	}
	return candidate.TitleEpisodes, true
}

func (selection *preparedV2TVSelection) effectiveNeed(candidate V2TVCandidate) map[Episode]struct{} {
	if candidate.AllowedEpisodes == nil {
		return selection.remaining
	}
	effective := make(map[Episode]struct{}, len(candidate.AllowedEpisodes))
	for _, episode := range candidate.AllowedEpisodes {
		if _, remains := selection.remaining[episode]; remains {
			effective[episode] = struct{}{}
		}
	}
	return effective
}

func (selection *preparedV2TVSelection) project(index int) (V2TVEpisodeProjection, bool, error) {
	if err := selection.ctx.Err(); err != nil {
		return V2TVEpisodeProjection{}, true, err
	}
	projection, err := selection.projector(selection.ctx, cloneV2TVCandidate(selection.candidates[index]))
	if err != nil {
		if contextErr := selection.ctx.Err(); contextErr != nil {
			return V2TVEpisodeProjection{}, true, contextErr
		}
		return V2TVEpisodeProjection{}, true, err
	}
	seasonSet := make(map[int]struct{}, len(selection.candidates[index].Seasons))
	for _, season := range selection.candidates[index].Seasons {
		seasonSet[season] = struct{}{}
	}
	projection.Episodes, err = normalizeV2TVEpisodes(projection.Episodes, seasonSet)
	if err != nil {
		return V2TVEpisodeProjection{}, true, err
	}
	if projection.Stop {
		return projection, true, nil
	}
	return projection, false, nil
}

func (selection *preparedV2TVSelection) attempt(
	index int,
	stage V2TVStage,
	selected []Episode,
	expected []Episode,
) (bool, error) {
	if err := selection.ctx.Err(); err != nil {
		return true, err
	}
	selection.exhausted[index] = true
	result, err := selection.attempter(selection.ctx, V2TVCandidateAttempt{
		Candidate: cloneV2TVCandidate(selection.candidates[index]), Stage: stage,
		SelectedEpisodes: append([]Episode(nil), selected...),
		ExpectedCoverage: append([]Episode(nil), expected...),
	})
	if err != nil {
		if contextErr := selection.ctx.Err(); contextErr != nil {
			return true, contextErr
		}
		return true, err
	}
	if !result.HashAcquired {
		if len(result.CoveredEpisodes) != 0 {
			return true, fmt.Errorf("%w: coverage without acquired hash", ErrInvalidV2TVSelection)
		}
		return result.Stop, nil
	}
	covered, normalizeErr := normalizeV2TVEpisodes(result.CoveredEpisodes, nil)
	if normalizeErr != nil || len(covered) == 0 || !episodesSubsetOfSlice(covered, expected) {
		return true, fmt.Errorf("%w: invalid confirmed coverage", ErrInvalidV2TVSelection)
	}
	for _, episode := range covered {
		delete(selection.remaining, episode)
	}
	selection.accepted = append(selection.accepted, selection.candidates[index].Key)
	return result.Stop, nil
}

func (selection *preparedV2TVSelection) hasRemainingSeason(season int) bool {
	for episode := range selection.remaining {
		if episode.Season == season {
			return true
		}
	}
	return false
}

func (selection *preparedV2TVSelection) result(stopped bool) V2TVSelectionResult {
	state := selection.state
	if state == V2TVSubscriptionStateReady && len(selection.remaining) == 0 {
		state = V2TVSubscriptionStateCompleted
	}
	remaining := make([]Episode, 0, len(selection.remaining))
	for episode := range selection.remaining {
		remaining = append(remaining, episode)
	}
	sortV2TVEpisodes(remaining)
	accepted := make([]string, len(selection.accepted))
	copy(accepted, selection.accepted)
	return V2TVSelectionResult{
		SubscriptionState: state, AcceptedCandidateKeys: accepted,
		RemainingEpisodes: remaining, Stopped: stopped,
	}
}

func episodesSubsetOf(items []Episode, set map[Episode]struct{}) bool {
	if len(items) == 0 {
		return false
	}
	for _, item := range items {
		if _, exists := set[item]; !exists {
			return false
		}
	}
	return true
}

func episodesSubsetOfSlice(items, allowed []Episode) bool {
	set := make(map[Episode]struct{}, len(allowed))
	for _, item := range allowed {
		set[item] = struct{}{}
	}
	return episodesSubsetOf(items, set)
}

func episodesIntersect(items []Episode, set map[Episode]struct{}) bool {
	for _, item := range items {
		if _, exists := set[item]; exists {
			return true
		}
	}
	return false
}

func episodeIntersection(items []Episode, set map[Episode]struct{}) []Episode {
	result := make([]Episode, 0, len(items))
	for _, item := range items {
		if _, exists := set[item]; exists {
			result = append(result, item)
		}
	}
	sortV2TVEpisodes(result)
	return result
}

func containsV2TVSeason(seasons []int, target int) bool {
	for _, season := range seasons {
		if season == target {
			return true
		}
	}
	return false
}

func sortV2TVEpisodes(items []Episode) {
	sort.Slice(items, func(i, j int) bool {
		if items[i].Season != items[j].Season {
			return items[i].Season < items[j].Season
		}
		return items[i].Episode < items[j].Episode
	})
}

func cloneV2TVCandidate(candidate V2TVCandidate) V2TVCandidate {
	candidate.Seasons = append([]int(nil), candidate.Seasons...)
	candidate.TitleEpisodes = append([]Episode(nil), candidate.TitleEpisodes...)
	if candidate.AllowedEpisodes != nil {
		candidate.AllowedEpisodes = append([]Episode{}, candidate.AllowedEpisodes...)
	}
	return candidate
}

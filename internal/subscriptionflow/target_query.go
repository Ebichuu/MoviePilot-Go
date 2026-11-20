package subscriptionflow

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/moviepilot-go/moviepilot-go/internal/media"
	"github.com/moviepilot-go/moviepilot-go/internal/metadata"
	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

func tvTargetSnapshot(target persistedSubscription) (subscription.TVTargetSnapshot, bool, error) {
	season := 1
	if target.Season != nil {
		season = *target.Season
	}
	snapshot := subscription.TVTargetSnapshot{Season: season}
	var err error
	for key, destination := range map[string]*int{
		"start_episode": &snapshot.StartEpisode,
		"total_episode": &snapshot.TotalEpisode,
		"lack_episode":  &snapshot.LackEpisode,
	} {
		value, exists := target.Config[key]
		if !exists || value == nil {
			continue
		}
		if *destination, err = requiredConfigInteger(value); err != nil {
			return subscription.TVTargetSnapshot{}, false, ErrSubscriptionConfiguration
		}
	}
	for key, destination := range map[string]*bool{
		"manual_total_episode": &snapshot.ManualTotalEpisode,
		"best_version":         &snapshot.BestVersion,
		"best_version_full":    &snapshot.BestVersionFull,
	} {
		value, exists := target.Config[key]
		if !exists || value == nil {
			continue
		}
		if key == "best_version" || key == "best_version_full" {
			if _, isBool := value.(bool); !isBool {
				integer, parseErr := requiredConfigInteger(value)
				if parseErr != nil {
					return subscription.TVTargetSnapshot{}, false, ErrSubscriptionConfiguration
				}
				*destination = integer != 0
				continue
			}
		}
		if *destination, err = configFlag(value); err != nil {
			return subscription.TVTargetSnapshot{}, false, ErrSubscriptionConfiguration
		}
	}
	if snapshot.Note, err = targetConfigIntegerSlice(target.Config["note"]); err != nil {
		return subscription.TVTargetSnapshot{}, false, ErrSubscriptionConfiguration
	}
	if value, exists := target.Config["current_priority"]; exists && value != nil {
		priority, parseErr := requiredConfigInteger(value)
		if parseErr != nil {
			return subscription.TVTargetSnapshot{}, false, ErrSubscriptionConfiguration
		}
		snapshot.CurrentPriority = &priority
	}
	if snapshot.EpisodePriority, err = targetConfigEpisodePriority(target.Config["episode_priority"]); err != nil {
		return subscription.TVTargetSnapshot{}, false, ErrSubscriptionConfiguration
	}
	resolution, err := subscription.DescribeTVTarget(snapshot)
	if err != nil {
		return subscription.TVTargetSnapshot{}, false, ErrSubscriptionConfiguration
	}
	_, hasTotal := target.Config["total_episode"]
	return resolution.Snapshot, !hasTotal, nil
}

func (h *Handler) refreshTVTarget(
	ctx context.Context,
	target persistedSubscription,
) (persistedSubscription, subscription.TVTargetResolution, bool, error) {
	total, available := 0, false
	snapshot, _, err := tvTargetSnapshot(target)
	if err != nil {
		return persistedSubscription{}, subscription.TVTargetResolution{}, false, err
	}
	if !snapshot.ManualTotalEpisode {
		total, available, err = h.metadataTVTargetTotal(ctx, target)
		if err != nil {
			return persistedSubscription{}, subscription.TVTargetResolution{}, false, err
		}
	}
	return h.refreshTVTargetWithTotal(ctx, target, total, available)
}

func (h *Handler) refreshTVTargetWithTotal(
	ctx context.Context,
	target persistedSubscription,
	total int,
	available bool,
) (persistedSubscription, subscription.TVTargetResolution, bool, error) {
	snapshot, legacyWithoutTotal, err := tvTargetSnapshot(target)
	if err != nil {
		return persistedSubscription{}, subscription.TVTargetResolution{}, false, err
	}
	resolution, err := subscription.DescribeTVTarget(snapshot)
	if err != nil {
		return persistedSubscription{}, subscription.TVTargetResolution{}, false, ErrSubscriptionConfiguration
	}
	if !snapshot.ManualTotalEpisode {
		if available {
			evidence := subscription.TVTargetShrinkEvidence{}
			if total > 0 && total < snapshot.TotalEpisode {
				evidence = h.tvTargetShrinkEvidence(ctx, target, snapshot)
			}
			resolution, err = subscription.ResolveTVAutomaticTarget(snapshot, total, evidence)
		}
		if err != nil {
			return persistedSubscription{}, subscription.TVTargetResolution{}, false, ErrSubscriptionConfiguration
		}
	}
	if legacyWithoutTotal && resolution.Knowledge == subscription.TVTargetUnknown {
		return target, resolution, true, nil
	}
	nextVersion, err := h.reconcileTVTarget(ctx, target, resolution)
	if err != nil {
		if errors.Is(err, ErrSubscriptionTargetChanged) {
			return persistedSubscription{}, subscription.TVTargetResolution{}, false, ErrSubscriptionTargetChanged
		}
		return persistedSubscription{}, subscription.TVTargetResolution{}, false, err
	}
	target.Season = intPointerCopy(resolution.Snapshot.Season)
	target.Config = applyTVTargetSnapshot(target.Config, resolution.Snapshot)
	target.UpdatedAt = nextVersion
	return target, resolution, false, nil
}

func (h *Handler) tvTargetShrinkEvidence(
	ctx context.Context,
	target persistedSubscription,
	snapshot subscription.TVTargetSnapshot,
) subscription.TVTargetShrinkEvidence {
	if h == nil || h.existingEpisodes == nil || ctx == nil {
		return subscription.TVTargetShrinkEvidence{}
	}
	existing, err := h.existingEpisodes.FindExistingEpisodes(ctx, ExistingEpisodesQuery{
		Title: strings.TrimSpace(target.Name), Year: strings.TrimSpace(target.Year),
		MediaSource: strings.TrimSpace(target.MediaSource), MediaID: strings.TrimSpace(target.MediaID),
	})
	if err != nil || len(existing) > maxTrackedEpisodes {
		return subscription.TVTargetShrinkEvidence{}
	}
	found := make(map[int]struct{})
	for _, episode := range existing {
		if episode.Season < 0 || episode.Season > maxTrackedSeason ||
			episode.Episode < 1 || episode.Episode > maxTrackedEpisode {
			return subscription.TVTargetShrinkEvidence{}
		}
		if episode.Season == snapshot.Season {
			found[episode.Episode] = struct{}{}
		}
	}
	for _, episode := range snapshot.Note {
		found[episode] = struct{}{}
	}
	start := snapshot.StartEpisode
	if start < 1 {
		start = 1
	}
	missing := make([]int, 0)
	for episode := start; episode <= snapshot.TotalEpisode; episode++ {
		if _, exists := found[episode]; !exists {
			missing = append(missing, episode)
		}
	}
	return subscription.TVTargetShrinkEvidence{
		Available: true, Satisfied: len(missing) == 0, MissingEpisodes: missing,
	}
}

func (h *Handler) metadataTVTargetTotal(ctx context.Context, target persistedSubscription) (int, bool, error) {
	if h == nil || h.mediaDetails == nil || ctx == nil ||
		strings.TrimSpace(target.MediaSource) == "" || strings.TrimSpace(target.MediaID) == "" {
		return 0, false, nil
	}
	detail, available, err := h.subscriptionMediaFacts(ctx, target)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
			return 0, false, err
		}
	}
	if !available {
		return 0, false, nil
	}
	season := target.Season
	episodes := targetEpisodesFromMetadata(season, 1, detail)
	if len(episodes) == 0 {
		mediaType := strings.ToLower(strings.TrimSpace(detail.Type))
		if mediaType == "tv" || mediaType == "anime" {
			return 0, true, nil
		}
		return 0, false, nil
	}
	return episodes[len(episodes)-1].Episode, true, nil
}

func applyTVTargetSnapshot(config map[string]any, snapshot subscription.TVTargetSnapshot) map[string]any {
	result := make(map[string]any, len(config)+7)
	for key, value := range config {
		result[key] = value
	}
	result["total_episode"] = snapshot.TotalEpisode
	result["start_episode"] = snapshot.StartEpisode
	result["manual_total_episode"] = snapshot.ManualTotalEpisode
	result["lack_episode"] = snapshot.LackEpisode
	result["note"] = append([]int(nil), snapshot.Note...)
	result["current_priority"] = nil
	if snapshot.CurrentPriority != nil {
		result["current_priority"] = *snapshot.CurrentPriority
	}
	priorities := make(map[int]int, len(snapshot.EpisodePriority))
	for episode, priority := range snapshot.EpisodePriority {
		priorities[episode] = priority
	}
	result["episode_priority"] = priorities
	return result
}

func requiredConfigInteger(value any) (int, error) {
	if parsed, ok := integerValue(value); ok {
		return parsed, nil
	}
	return 0, ErrSubscriptionConfiguration
}

func configFlag(value any) (bool, error) {
	if flag, ok := value.(bool); ok {
		return flag, nil
	}
	integer, ok := integerValue(value)
	if !ok || (integer != 0 && integer != 1) {
		return false, ErrSubscriptionConfiguration
	}
	return integer == 1, nil
}

func targetConfigIntegerSlice(value any) ([]int, error) {
	if value == nil {
		return []int{}, nil
	}
	items, ok := value.([]any)
	if !ok {
		if typed, typedOK := value.([]int); typedOK {
			return append([]int(nil), typed...), nil
		}
		return nil, ErrSubscriptionConfiguration
	}
	result := make([]int, 0, len(items))
	for _, item := range items {
		integer, err := requiredConfigInteger(item)
		if err != nil {
			return nil, err
		}
		result = append(result, integer)
	}
	return result, nil
}

func targetConfigEpisodePriority(value any) (map[int]int, error) {
	result, err := subscription.NormalizeEpisodePriorities(value)
	if err != nil {
		return nil, ErrSubscriptionConfiguration
	}
	return result, nil
}

func intPointerCopy(value int) *int {
	copy := value
	return &copy
}

func candidateQuery(target persistedSubscription) (subscription.Query, string, error) {
	mediaType := media.MediaType(strings.TrimSpace(target.MediaType))
	if mediaType != media.TypeMovie && mediaType != media.TypeTV {
		return subscription.Query{}, "", subscription.ErrInvalidCandidateQuery
	}
	keyword := configText(target.Config, "keyword")
	if keyword == "" {
		keyword = strings.TrimSpace(strings.Join([]string{target.Name, target.Year}, " "))
	}
	query := subscription.Query{
		Title:       target.Name,
		Aliases:     configTextSlice(target.Config, "aliases"),
		Year:        target.Year,
		MediaType:   mediaType,
		Season:      target.Season,
		SiteIDs:     configIntSlice(target.Config, "sites"),
		Include:     v2AdditionalPattern(configText(target.Config, "include")),
		Exclude:     v2AdditionalPattern(configText(target.Config, "exclude")),
		Filter:      configText(target.Config, "filter"),
		Quality:     configText(target.Config, "quality"),
		Resolution:  configText(target.Config, "resolution"),
		Effect:      configText(target.Config, "effect"),
		CustomWords: splitLines(configText(target.Config, "custom_words")),
		Limit:       subscription.MaxCandidateLimit,
	}
	if mediaType == media.TypeTV {
		query.MissingEpisodes = initialMissingEpisodes(target)
	}
	return query, keyword, nil
}

func initialMissingEpisodes(target persistedSubscription) []subscription.Episode {
	total, ok := configInteger(target.Config, "total_episode")
	if !ok || total <= 0 || total > 5000 {
		return nil
	}
	start, ok := configInteger(target.Config, "start_episode")
	if !ok || start < 1 {
		start = 1
	}
	if start > total {
		return nil
	}
	season := 1
	if target.Season != nil {
		season = *target.Season
	}
	episodes := make([]subscription.Episode, 0, total-start+1)
	for episode := start; episode <= total; episode++ {
		episodes = append(episodes, subscription.Episode{Season: season, Episode: episode})
	}
	return episodes
}

func (h *Handler) metadataTargetEpisodes(
	ctx context.Context,
	target persistedSubscription,
) ([]subscription.Episode, error) {
	if h == nil || h.mediaDetails == nil || ctx == nil ||
		strings.TrimSpace(target.MediaSource) == "" || strings.TrimSpace(target.MediaID) == "" {
		return nil, nil
	}
	detail, available, err := h.subscriptionMediaFacts(ctx, target)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
			return nil, err
		}
	}
	if !available {
		return nil, nil
	}
	start, ok := configInteger(target.Config, "start_episode")
	if !ok || start < 1 {
		start = 1
	}
	return targetEpisodesFromMetadata(target.Season, start, detail), nil
}

func targetEpisodesFromMetadata(
	targetSeason *int,
	start int,
	detail metadata.Media,
) []subscription.Episode {
	mediaType := strings.ToLower(strings.TrimSpace(detail.Type))
	if mediaType != "tv" && mediaType != "anime" {
		return nil
	}
	season := 1
	if targetSeason != nil {
		season = *targetSeason
	}
	if season < 0 || season > maxTrackedSeason {
		return nil
	}
	if start < 1 {
		start = 1
	}

	episodeCount := detail.EpisodeCount
	if len(detail.SeasonInfo) > 0 {
		seen := make(map[int]struct{}, len(detail.SeasonInfo))
		total := 0
		found := false
		for _, info := range detail.SeasonInfo {
			if info.SeasonNumber < 0 || info.SeasonNumber > maxTrackedSeason ||
				info.EpisodeCount < 0 || info.EpisodeCount > maxTrackedEpisode {
				return nil
			}
			if _, exists := seen[info.SeasonNumber]; exists || total > maxTrackedEpisodes-info.EpisodeCount {
				return nil
			}
			seen[info.SeasonNumber] = struct{}{}
			total += info.EpisodeCount
			if info.SeasonNumber == season {
				episodeCount = info.EpisodeCount
				found = true
			}
		}
		if !found {
			return nil
		}
	}
	if episodeCount < 1 || episodeCount > maxTrackedEpisode || start > episodeCount {
		return nil
	}
	episodes := make([]subscription.Episode, 0, episodeCount-start+1)
	for episode := start; episode <= episodeCount; episode++ {
		episodes = append(episodes, subscription.Episode{Season: season, Episode: episode})
	}
	return episodes
}

func configText(config map[string]any, key string) string {
	value, _ := config[key].(string)
	return strings.TrimSpace(value)
}

func configTextSlice(config map[string]any, key string) []string {
	value := config[key]
	switch typed := value.(type) {
	case []string:
		return append([]string(nil), typed...)
	case []any:
		result := make([]string, 0, len(typed))
		for _, item := range typed {
			if text, ok := item.(string); ok && strings.TrimSpace(text) != "" {
				result = append(result, strings.TrimSpace(text))
			}
		}
		return result
	default:
		return nil
	}
}

func configIntSlice(config map[string]any, key string) []int {
	value := config[key]
	result := make([]int, 0)
	switch typed := value.(type) {
	case []int:
		return append(result, typed...)
	case []any:
		for _, item := range typed {
			if number, ok := integerValue(item); ok {
				result = append(result, number)
			}
		}
	}
	return result
}

func configInteger(config map[string]any, key string) (int, bool) {
	return integerValue(config[key])
}

func integerValue(value any) (int, bool) {
	switch typed := value.(type) {
	case int:
		return typed, true
	case int32:
		return int(typed), true
	case int64:
		return int(typed), true
	case float64:
		integer := int(typed)
		return integer, typed == float64(integer)
	case json.Number:
		integer, err := typed.Int64()
		return int(integer), err == nil
	default:
		return 0, false
	}
}

func splitTerms(value string) []string {
	return splitBy(value, func(character rune) bool { return character == ',' || character == '\n' || character == '\r' })
}

func splitLines(value string) []string {
	return splitBy(value, func(character rune) bool { return character == '\n' || character == '\r' })
}

func splitBy(value string, separator func(rune) bool) []string {
	fields := strings.FieldsFunc(value, separator)
	result := make([]string, 0, len(fields))
	for _, field := range fields {
		if field = strings.TrimSpace(field); field != "" {
			result = append(result, field)
		}
	}
	return result
}

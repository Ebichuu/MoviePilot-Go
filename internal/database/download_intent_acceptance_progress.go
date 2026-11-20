package database

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"

	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

func decodeDownloadIntentAcceptanceConfig(raw []byte) (map[string]any, error) {
	if len(raw) == 0 {
		return nil, ErrDownloadIntentPersistence
	}
	config := make(map[string]any)
	if err := json.Unmarshal(raw, &config); err != nil {
		return nil, ErrDownloadIntentPersistence
	}
	if config == nil {
		return nil, ErrDownloadIntentPersistence
	}
	return config, nil
}

func buildDownloadIntentAcceptanceProgressPatch(
	mediaType string,
	config map[string]any,
	consumedEpisodes []subscription.Episode,
	missing int64,
) ([]byte, error) {
	if missing < 0 || missing > maxSubscriptionEpisodes {
		return nil, ErrDownloadIntentPersistence
	}
	mediaType = strings.ToLower(strings.TrimSpace(mediaType))
	if mediaType != "电影" && mediaType != "movie" && mediaType != "电视剧" && mediaType != "tv" {
		return nil, ErrDownloadIntentPersistence
	}
	if downloadIntentAcceptanceBestVersion(config) {
		return []byte("{}"), nil
	}

	note, err := downloadIntentAcceptanceNote(config["note"])
	if err != nil {
		return nil, err
	}
	if mediaType == "电影" || mediaType == "movie" {
		note[1] = struct{}{}
		missing = 0
	} else {
		for _, episode := range consumedEpisodes {
			if !validSubscriptionEpisode(episode) {
				return nil, ErrDownloadIntentPersistence
			}
			note[episode.Episode] = struct{}{}
		}
	}

	ordered := make([]int, 0, len(note))
	for episode := range note {
		ordered = append(ordered, episode)
	}
	sort.Ints(ordered)
	encoded, err := json.Marshal(map[string]any{
		"note":         ordered,
		"lack_episode": int(missing),
	})
	if err != nil {
		return nil, ErrDownloadIntentPersistence
	}
	return encoded, nil
}

func downloadIntentAcceptanceBestVersion(config map[string]any) bool {
	value, exists := config["best_version"]
	if !exists {
		return false
	}
	if enabled, ok := value.(bool); ok {
		return enabled
	}
	integer, ok := subscriptionInteger(value)
	return ok && integer != 0
}

func downloadIntentAcceptanceNote(value any) (map[int]struct{}, error) {
	note := make(map[int]struct{})
	appendEpisode := func(value any) error {
		episode, ok := subscriptionInteger(value)
		if !ok {
			if text, textOK := value.(string); textOK {
				parsed, err := strconv.Atoi(strings.TrimSpace(text))
				if err == nil {
					episode, ok = parsed, true
				}
			}
		}
		if !ok {
			return nil
		}
		if episode < 1 || episode > maxSubscriptionEpisodeNumber {
			return nil
		}
		note[episode] = struct{}{}
		return nil
	}

	switch values := value.(type) {
	case nil:
		return note, nil
	case []any:
		for _, item := range values {
			if err := appendEpisode(item); err != nil {
				return nil, err
			}
		}
	case []int:
		for _, item := range values {
			if err := appendEpisode(item); err != nil {
				return nil, err
			}
		}
	default:
		return note, nil
	}
	if len(note) > maxSubscriptionEpisodes {
		return nil, ErrDownloadIntentPersistence
	}
	return note, nil
}

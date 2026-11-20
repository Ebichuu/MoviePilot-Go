package database

import (
	"encoding/json"
	"strings"

	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

func isDownloadIntentAcceptanceMovie(mediaType string) bool {
	switch strings.ToLower(strings.TrimSpace(mediaType)) {
	case "电影", "movie":
		return true
	default:
		return false
	}
}

func downloadIntentAcceptanceMovieState(config map[string]any, recordedPriority *int) (subscription.State, error) {
	if !downloadIntentAcceptanceBestVersion(config) {
		return subscription.Completed, nil
	}
	priority := recordedPriority
	if priority == nil && config["current_priority"] != nil {
		current, ok := subscriptionInteger(config["current_priority"])
		if !ok {
			return "", ErrDownloadIntentPersistence
		}
		priority = &current
	}
	if subscription.MovieBestVersionComplete(true, priority) {
		return subscription.Completed, nil
	}
	return subscription.Active, nil
}

// Add only the immutable selected quality. Unknown legacy plans leave it untouched.
// TV parents may have changed identity since staging and must ignore movie facts.
func addDownloadIntentAcceptanceMoviePriority(mediaType string, progressPatch []byte, recordedPriority *int) ([]byte, error) {
	if !isDownloadIntentAcceptanceMovie(mediaType) || recordedPriority == nil {
		return progressPatch, nil
	}
	patch := make(map[string]any)
	if err := json.Unmarshal(progressPatch, &patch); err != nil || patch == nil {
		return nil, ErrDownloadIntentPersistence
	}
	patch["current_priority"] = *recordedPriority
	encoded, err := json.Marshal(patch)
	if err != nil {
		return nil, ErrDownloadIntentPersistence
	}
	return encoded, nil
}

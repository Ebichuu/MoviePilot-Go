package subscriptionflow

import (
	"strings"

	"github.com/moviepilot-go/moviepilot-go/internal/media"
	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

func filterMovieUpgradeCandidates(target persistedSubscription, candidates []subscription.Candidate) ([]subscription.Candidate, error) {
	if strings.TrimSpace(target.MediaType) != string(media.TypeMovie) || !subscriptionBestVersionEnabled(target.Config) {
		return append([]subscription.Candidate(nil), candidates...), nil
	}
	var current *int
	if value := target.Config["current_priority"]; value != nil {
		priority, err := requiredConfigInteger(value)
		if err != nil {
			return nil, err
		}
		current = &priority
	}
	result := make([]subscription.Candidate, 0, len(candidates))
	for _, candidate := range candidates {
		if subscription.MovieUpgradeAllowed(true, current, candidate.Resource.PriOrder) {
			result = append(result, candidate)
		}
	}
	return result, nil
}

func movieCandidatePriority(target persistedSubscription, candidate subscription.Candidate) *int {
	if strings.TrimSpace(target.MediaType) != string(media.TypeMovie) {
		return nil
	}
	return intPointerCopy(candidate.Resource.PriOrder)
}

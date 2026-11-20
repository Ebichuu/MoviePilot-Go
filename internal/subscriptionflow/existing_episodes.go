package subscriptionflow

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

// ExistingEpisodesQuery is the V2 media identity used to locate an Emby
// Series. The source may use the TMDB identity when available and otherwise
// falls back to the exact title and year behavior of V2.
type ExistingEpisodesQuery struct {
	Title       string
	Year        string
	MediaSource string
	MediaID     string
}

// ExistingEpisodesSource is the consumer-owned media library boundary used by
// both precise search and RSS matching.
type ExistingEpisodesSource interface {
	FindExistingEpisodes(context.Context, ExistingEpisodesQuery) ([]subscription.Episode, error)
}

// ExistingMovieQuery is the V2 media identity used to locate an Emby Movie.
type ExistingMovieQuery struct {
	Title       string
	Year        string
	MediaSource string
	MediaID     string
}

// ExistingMovieSource is the consumer-owned movie-library boundary used by
// both precise search and RSS matching.
type ExistingMovieSource interface {
	FindExistingMovie(context.Context, ExistingMovieQuery) (bool, error)
}

// SatisfiedSubscriptionStore completes a normal subscription whose target is
// already satisfied by the media library and recorded download history.
type SatisfiedSubscriptionStore interface {
	CompleteSatisfiedTVSubscription(context.Context, string, int64, subscription.State, time.Time) error
}

// SatisfiedMovieSubscriptionStore completes a movie satisfied by its ordinary
// library check or its accepted terminal best-version quality.
type SatisfiedMovieSubscriptionStore interface {
	CompleteSatisfiedMovieSubscription(context.Context, string, int64, subscription.State, time.Time) error
}

// SetExistingEpisodesSource attaches the bounded media library lookup before
// subscription workers start.
func (h *Handler) SetExistingEpisodesSource(source ExistingEpisodesSource) {
	if h != nil {
		h.existingEpisodes = source
	}
}

// SetExistingMovieSource attaches the bounded movie-library lookup before
// subscription workers start.
func (h *Handler) SetExistingMovieSource(source ExistingMovieSource) {
	if h != nil {
		h.existingMovies = source
	}
}

func (h *Handler) existingMovieSatisfied(
	ctx context.Context,
	targetTitle string,
	targetYear string,
	mediaSource string,
	mediaID string,
	config map[string]any,
) (bool, error) {
	if h == nil {
		return false, nil
	}
	if subscriptionBestVersionEnabled(config) {
		priority, ok := configInteger(config, "current_priority")
		return ok && subscription.MovieBestVersionComplete(true, &priority), nil
	}
	downloaded := len(subscriptionNoteEpisodes(config)) > 0
	if h.existingMovies == nil {
		return downloaded, nil
	}
	found, err := h.existingMovies.FindExistingMovie(ctx, ExistingMovieQuery{
		Title:       strings.TrimSpace(targetTitle),
		Year:        strings.TrimSpace(targetYear),
		MediaSource: strings.TrimSpace(mediaSource),
		MediaID:     strings.TrimSpace(mediaID),
	})
	if err != nil {
		if contextErr := existingEpisodesContextError(ctx, err); contextErr != nil {
			return false, contextErr
		}
		// V2 treats an unavailable media server as no library hit.
		return downloaded, nil
	}
	return found || downloaded, nil
}

func (h *Handler) excludeExistingEpisodes(
	ctx context.Context,
	query ExistingEpisodesQuery,
	season *int,
	config map[string]any,
	pending []subscription.Episode,
) ([]subscription.Episode, error) {
	result, err := h.excludeExistingEpisodesWithShape(ctx, query, season, config, pending)
	return result.Episodes, err
}

type existingEpisodesResult struct {
	Episodes        []subscription.Episode
	ExplicitSeasons map[int]bool
}

func (h *Handler) excludeExistingEpisodesWithShape(
	ctx context.Context,
	query ExistingEpisodesQuery,
	season *int,
	config map[string]any,
	pending []subscription.Episode,
) (existingEpisodesResult, error) {
	if len(pending) == 0 || h == nil || subscriptionBestVersionEnabled(config) {
		return existingEpisodesResult{Episodes: append([]subscription.Episode(nil), pending...)}, nil
	}
	existing := []subscription.Episode(nil)
	if h.existingEpisodes != nil {
		var err error
		query.Title = strings.TrimSpace(query.Title)
		query.Year = strings.TrimSpace(query.Year)
		query.MediaSource = strings.TrimSpace(query.MediaSource)
		query.MediaID = strings.TrimSpace(query.MediaID)
		existing, err = h.existingEpisodes.FindExistingEpisodes(ctx, query)
		if err != nil {
			if contextErr := existingEpisodesContextError(ctx, err); contextErr != nil {
				return existingEpisodesResult{}, contextErr
			}
			// V2 treats an unavailable media server as no library hit.
			existing = nil
		}
	}
	if len(existing) > maxTrackedEpisodes {
		existing = nil
	}
	existingSet := make(map[subscription.Episode]struct{}, len(existing))
	for _, episode := range existing {
		if episode.Season < 0 || episode.Season > maxTrackedSeason ||
			episode.Episode < 1 || episode.Episode > maxTrackedEpisode {
			existingSet = nil
			break
		}
		existingSet[episode] = struct{}{}
	}
	note := subscriptionNoteEpisodes(config)
	noteSeason := 1
	if season != nil {
		noteSeason = *season
	}
	var explicitSeasons map[int]bool
	if len(existingSet) > 0 || len(note) > 0 {
		explicitSeasons = make(map[int]bool)
		for episode := range existingSet {
			explicitSeasons[episode.Season] = true
		}
		if len(note) > 0 {
			explicitSeasons[noteSeason] = true
		}
	}
	result := make([]subscription.Episode, 0, len(pending))
	for _, episode := range pending {
		if _, found := existingSet[episode]; found {
			continue
		}
		if episode.Season == noteSeason {
			if _, downloaded := note[episode.Episode]; downloaded {
				continue
			}
		}
		result = append(result, episode)
	}
	return existingEpisodesResult{Episodes: result, ExplicitSeasons: explicitSeasons}, nil
}

func subscriptionBestVersionEnabled(config map[string]any) bool {
	value, exists := config["best_version"]
	if !exists {
		return false
	}
	if enabled, ok := value.(bool); ok {
		return enabled
	}
	integer, ok := integerValue(value)
	return ok && integer != 0
}

func subscriptionNoteEpisodes(config map[string]any) map[int]struct{} {
	result := make(map[int]struct{})
	appendEpisode := func(value any) {
		episode, ok := integerValue(value)
		if !ok {
			if text, textOK := value.(string); textOK {
				parsed, err := strconv.Atoi(strings.TrimSpace(text))
				if err == nil {
					episode, ok = parsed, true
				}
			}
		}
		if ok && episode >= 1 && episode <= maxTrackedEpisode {
			result[episode] = struct{}{}
		}
	}
	switch values := config["note"].(type) {
	case []any:
		for _, value := range values {
			appendEpisode(value)
		}
	case []int:
		for _, value := range values {
			appendEpisode(value)
		}
	}
	return result
}

func existingEpisodesContextError(ctx context.Context, err error) error {
	if ctx != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return contextErr
		}
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return nil
}

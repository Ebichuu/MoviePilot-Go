package subscriptionflow

import (
	"context"
	"math"
	"strings"

	"github.com/google/uuid"

	"github.com/moviepilot-go/moviepilot-go/internal/indexer"
	"github.com/moviepilot-go/moviepilot-go/internal/media"
	"github.com/moviepilot-go/moviepilot-go/internal/metadata"
	"github.com/moviepilot-go/moviepilot-go/internal/organization"
	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

type subscriptionSuccessTarget struct {
	Username    string
	Name        string
	Year        string
	MediaType   string
	MediaSource string
	MediaID     string
	VoteAverage float64
	PosterURL   string
	BackdropURL string
	Config      map[string]any
}

// MediaDetailResolver is the consumer-owned metadata detail contract used to
// freeze V2 notification fields before the download submission transaction.
type MediaDetailResolver interface {
	Detail(context.Context, string, string) (metadata.Media, error)
}

// CategoryResolver selects the first configured media category for metadata facts.
type CategoryResolver interface {
	Category(string, metadata.Media) string
}

// SetMediaDetailResolver attaches the bounded metadata coordinator before the
// subscription workers start. Resolver failures leave target-less TV subscriptions active.
func (h *Handler) SetMediaDetailResolver(resolver MediaDetailResolver) {
	if h != nil {
		h.mediaDetails = resolver
	}
}

// SetCategoryResolver attaches the runtime category.yaml snapshot used by V2 filters.
func (h *Handler) SetCategoryResolver(resolver CategoryResolver) {
	if h != nil {
		h.categoryResolver = resolver
	}
}

func subscriptionSuccessInput(
	target subscriptionSuccessTarget,
	selected subscription.Candidate,
	intentID uuid.UUID,
) organization.SuccessInput {
	return organization.SuccessInput{
		IntentID:   intentID,
		Version:    organization.CurrentSuccessVersion,
		Username:   target.Username,
		Background: true,
		Media: organization.MediaContext{
			Source:      target.MediaSource,
			MediaID:     target.MediaID,
			TitleYear:   subscriptionTitleYear(target.Name, target.Year),
			MediaType:   target.MediaType,
			Category:    configText(target.Config, "media_category"),
			VoteAverage: target.VoteAverage,
			PosterURL:   target.PosterURL,
			BackdropURL: target.BackdropURL,
		},
		ResourceTerm: subscriptionResourceTerm(
			selected.Resource,
			splitLines(configText(target.Config, "custom_words")),
		),
		Episodes: organizationEpisodes(selected.CoveredEpisodes),
	}
}

func (h *Handler) buildSubscriptionSuccessInput(
	ctx context.Context,
	target subscriptionSuccessTarget,
	selected subscription.Candidate,
	intentID uuid.UUID,
) organization.SuccessInput {
	input := subscriptionSuccessInput(target, selected, intentID)
	if h == nil || h.mediaDetails == nil || ctx == nil || strings.TrimSpace(target.MediaSource) == "" ||
		strings.TrimSpace(target.MediaID) == "" {
		return input
	}
	detailID := subscriptionMetadataDetailID(target.MediaSource, target.MediaID, target.MediaType)
	detail, err := h.mediaDetails.Detail(ctx, target.MediaSource, detailID)
	if err != nil {
		return input
	}
	candidate := input.Clone()
	if !math.IsNaN(detail.VoteAverage) && !math.IsInf(detail.VoteAverage, 0) &&
		detail.VoteAverage > 0 && detail.VoteAverage <= 10 {
		candidate.Media.VoteAverage = detail.VoteAverage
	}
	if value := strings.TrimSpace(detail.PosterURL); value != "" {
		candidate.Media.PosterURL = value
	}
	if value := strings.TrimSpace(detail.BackdropURL); value != "" {
		candidate.Media.BackdropURL = value
	}
	normalized, err := organization.NormalizeSuccessInput(candidate)
	if err != nil {
		return input
	}
	return normalized
}

func subscriptionMetadataDetailID(source, id, mediaType string) string {
	id = strings.TrimSpace(id)
	if id == "" || strings.Contains(id, ":") {
		return id
	}
	switch strings.ToLower(strings.TrimSpace(source)) {
	case "tmdb", "themoviedb", "douban":
		switch media.MediaType(strings.TrimSpace(mediaType)) {
		case media.TypeMovie:
			return "movie:" + id
		case media.TypeTV:
			return "tv:" + id
		}
	}
	return id
}

func subscriptionConfigVote(config map[string]any) float64 {
	value, ok := config["vote"]
	if !ok {
		return 0
	}
	var vote float64
	switch number := value.(type) {
	case float64:
		vote = number
	case float32:
		vote = float64(number)
	case int:
		vote = float64(number)
	case int64:
		vote = float64(number)
	default:
		return 0
	}
	if math.IsNaN(vote) || math.IsInf(vote, 0) || vote < 0 || vote > 10 {
		return 0
	}
	return vote
}

func subscriptionTitleYear(title, year string) string {
	title = strings.TrimSpace(title)
	year = strings.TrimSpace(year)
	if year == "" {
		return title
	}
	if title == "" {
		return year
	}
	return title + " (" + year + ")"
}

func subscriptionResourceTerm(resource indexer.Resource, customWords []string) string {
	parsed := media.Parse(
		strings.TrimSpace(resource.Title+" "+resource.Description),
		media.Options{CustomWords: customWords},
	)
	terms := make([]string, 0, 3)
	for _, term := range []string{parsed.ResourceType, parsed.ResourceEffect, parsed.Resolution} {
		if term = strings.TrimSpace(term); term != "" {
			terms = append(terms, term)
		}
	}
	return strings.Join(terms, " ")
}

func organizationEpisodes(episodes []subscription.Episode) []organization.Episode {
	if len(episodes) == 0 {
		return nil
	}
	result := make([]organization.Episode, len(episodes))
	for index, episode := range episodes {
		result[index] = organization.Episode{Season: episode.Season, Episode: episode.Episode}
	}
	return result
}

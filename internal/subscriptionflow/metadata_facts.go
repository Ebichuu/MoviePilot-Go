package subscriptionflow

import (
	"context"
	"strings"

	"github.com/moviepilot-go/moviepilot-go/internal/metadata"
)

// MediaSupplementer adds bounded TMDB facts without replacing the primary media identity.
type MediaSupplementer interface {
	Supplement(context.Context, metadata.TMDBSupplementInput) (metadata.Media, bool, error)
}

// SetMediaSupplementer attaches the optional TMDB auxiliary resolver used by subscription facts.
func (h *Handler) SetMediaSupplementer(supplementer MediaSupplementer) {
	if h != nil {
		h.mediaSupplementer = supplementer
	}
}

// subscriptionMediaFacts reads the primary provider first, then fills missing TMDB facts.
// A supplement failure returns the successful primary detail together with the error so each
// consumer can preserve its existing context-cancellation and fail-open behavior.
func (h *Handler) subscriptionMediaFacts(
	ctx context.Context,
	target persistedSubscription,
) (metadata.Media, bool, error) {
	return h.subscriptionMediaFactsRead(ctx, target, false)
}

type freshMediaDetailResolver interface {
	DetailFresh(context.Context, string, string) (metadata.Media, error)
}

func (h *Handler) subscriptionMediaFactsRead(
	ctx context.Context,
	target persistedSubscription,
	fresh bool,
) (metadata.Media, bool, error) {
	if h == nil || h.mediaDetails == nil || ctx == nil ||
		strings.TrimSpace(target.MediaSource) == "" || strings.TrimSpace(target.MediaID) == "" {
		return metadata.Media{}, false, nil
	}
	detailID := subscriptionMetadataDetailID(target.MediaSource, target.MediaID, target.MediaType)
	var primary metadata.Media
	var err error
	if reader, ok := h.mediaDetails.(freshMediaDetailResolver); fresh && ok {
		primary, err = reader.DetailFresh(ctx, target.MediaSource, detailID)
	} else {
		primary, err = h.mediaDetails.Detail(ctx, target.MediaSource, detailID)
	}
	if err != nil {
		return metadata.Media{}, false, err
	}
	if h.mediaSupplementer == nil {
		return primary, true, nil
	}

	supplemented, found, err := h.mediaSupplementer.Supplement(ctx, metadata.TMDBSupplementInput{
		Media: primary, Season: target.Season,
		SeasonYear: subscriptionSeasonYear(primary, target.Season, target.Year),
	})
	if err != nil || !found {
		return primary, true, err
	}
	// V2 _merge_tmdb_auxiliary never merges seasons, including an empty primary.
	supplemented.EpisodeCount = primary.EpisodeCount
	supplemented.SeasonInfo = append([]metadata.SeasonInfo(nil), primary.SeasonInfo...)
	return supplemented, true, nil
}

func subscriptionSeasonYear(primary metadata.Media, season *int, fallback string) int {
	if season != nil {
		for _, item := range primary.SeasonInfo {
			if item.SeasonNumber != *season || len(item.AirDate) < 4 {
				continue
			}
			if year := parseMetadataYear(item.AirDate[:4]); year > 0 {
				return year
			}
		}
	}
	if primary.Year > 0 {
		return primary.Year
	}
	return parseMetadataYear(fallback)
}

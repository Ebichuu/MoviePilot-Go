package subscriptionflow

import (
	"context"
	"strconv"
	"strings"

	"github.com/moviepilot-go/moviepilot-go/internal/indexer"
	"github.com/moviepilot-go/moviepilot-go/internal/media"
	"github.com/moviepilot-go/moviepilot-go/internal/metadata"
	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

// RSSMediaRecognizer is the consumer-owned boundary for V2's per-resource
// media recognition. Keeping the one method here avoids coupling RSS flow to
// a concrete metadata provider or coordinator.
type RSSMediaRecognizer interface {
	Recognize(context.Context, metadata.Query) (metadata.Match, bool, error)
}

// SetMediaRecognizer attaches the shared bounded metadata coordinator used by
// RSS refresh and matching. A nil value leaves legacy caches usable without
// inventing a media title for them.
func (h *RSSHandler) SetMediaRecognizer(recognizer RSSMediaRecognizer) {
	if h != nil {
		h.mediaRecognizer = recognizer
	}
}

func (h *RSSHandler) recognizeRSSResources(ctx context.Context, items []indexer.RSSItem) error {
	if h == nil || h.mediaRecognizer == nil {
		return nil
	}
	for index := range items {
		resource := &items[index].Resource
		if strings.TrimSpace(resource.RecognizedMediaTitle) != "" {
			continue
		}
		match, found, err := h.mediaRecognizer.Recognize(ctx, rssMetadataQuery(*resource, nil))
		if err != nil {
			if ctx != nil && ctx.Err() != nil {
				return ctx.Err()
			}
			continue
		}
		if found && strings.TrimSpace(match.Media.Title) != "" {
			resource.RecognizedMediaTitle = match.Media.Title
		}
	}
	return nil
}

func (h *RSSHandler) refreshRSSCandidateMediaTitles(
	ctx context.Context,
	candidates []subscription.Candidate,
) error {
	if h == nil || h.mediaRecognizer == nil {
		return nil
	}
	for index := range candidates {
		candidate := &candidates[index]
		// V2 only re-runs recognition for a candidate when subscription custom
		// words actually changed its parsed metadata.
		if len(candidate.Parsed.AppliedWords) == 0 {
			continue
		}
		match, found, err := h.mediaRecognizer.Recognize(ctx, rssMetadataQuery(candidate.Resource, &candidate.Parsed))
		if err != nil {
			if ctx != nil && ctx.Err() != nil {
				return ctx.Err()
			}
			continue
		}
		if found && strings.TrimSpace(match.Media.Title) != "" {
			candidate.MediaTitle = match.Media.Title
			candidate.Resource.RecognizedMediaTitle = candidate.MediaTitle
		}
	}
	return nil
}

func (h *RSSHandler) rssTargetMediaTitle(
	ctx context.Context,
	target persistedSubscription,
	query subscription.Query,
) string {
	if h == nil || h.mediaRecognizer == nil {
		return ""
	}
	if query.TVMediaRecognized {
		return query.TVMediaTitle
	}
	if h.subscription != nil && h.subscription.mediaDetails != nil &&
		strings.TrimSpace(target.MediaSource) != "" && strings.TrimSpace(target.MediaID) != "" {
		detailID := subscriptionMetadataDetailID(target.MediaSource, target.MediaID, target.MediaType)
		if detail, err := h.subscription.mediaDetails.Detail(ctx, target.MediaSource, detailID); err == nil {
			if strings.TrimSpace(detail.Title) != "" {
				return detail.Title
			}
		}
	}
	match, found, err := h.mediaRecognizer.Recognize(ctx, metadata.Query{
		Keyword: strings.TrimSpace(query.Title),
		Type:    metadataMediaType(query.MediaType),
		Year:    parseMetadataYear(target.Year),
		Source:  strings.TrimSpace(target.MediaSource),
	})
	if err != nil || !found {
		return ""
	}
	if strings.TrimSpace(match.Media.Title) == "" {
		return ""
	}
	return match.Media.Title
}

func rssMetadataQuery(resource indexer.Resource, parsed *media.ParseResult) metadata.Query {
	result := media.ParseResult{}
	if parsed != nil {
		result = *parsed
	} else {
		result = media.Parse(resource.Title, media.Options{})
		if result.Type != media.TypeTV && isRSSCategoryTV(resource.Category) {
			result.Type = media.TypeTV
		}
	}
	keyword := strings.TrimSpace(result.Name())
	if keyword == "" {
		keyword = strings.TrimSpace(resource.Title)
	}
	return metadata.Query{
		Keyword: keyword,
		Type:    metadataMediaType(result.Type),
		Year:    parseMetadataYear(result.Year),
	}
}

func metadataMediaType(value media.MediaType) string {
	switch value {
	case media.TypeMovie:
		return "movie"
	case media.TypeTV:
		return "tv"
	default:
		return ""
	}
}

func parseMetadataYear(value string) int {
	year, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || year < 0 || year > 9999 {
		return 0
	}
	return year
}

func isRSSCategoryTV(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "tv", "电视剧", "电视", "剧集", "anime":
		return true
	default:
		return false
	}
}

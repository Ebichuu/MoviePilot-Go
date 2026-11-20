package organizationflow

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/moviepilot-go/moviepilot-go/internal/mediaindex"
)

func (h *Handler) scrapeNFOFiles(ctx context.Context, plan mediaindex.CompletionPlan) {
	if h.nfoScraper == nil {
		return
	}

	// Use a detached context with bounded timeout to avoid blocking the worker.
	scrapeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()

	for _, file := range plan.Files {
		if err := h.scrapeOneNFO(scrapeCtx, file); err != nil {
			// Log the error but do not fail the organization batch.
			// NFO scraping is a best-effort enhancement.
			continue
		}
	}
}

func (h *Handler) scrapeOneNFO(ctx context.Context, file mediaindex.TargetFile) error {
	tmdbID, err := parseTMDBID(file.ProviderID)
	if err != nil {
		return err
	}

	switch file.MediaType {
	case "movie":
		return h.nfoScraper.ScrapeMovieNFO(ctx, tmdbID, file.LocalPath)
	case "tv":
		if file.Episode > 0 {
			return h.nfoScraper.ScrapeEpisodeNFO(ctx, tmdbID, file.Season, file.Episode, file.LocalPath)
		}
		return h.nfoScraper.ScrapeTVShowNFO(ctx, tmdbID, file.LocalPath)
	default:
		return fmt.Errorf("unsupported media type: %s", file.MediaType)
	}
}

func parseTMDBID(providerID string) (int, error) {
	if providerID == "" {
		return 0, errors.New("empty provider ID")
	}

	// Support both "tmdb:123" and "tmdb.123" formats
	var idStr string
	if idx := strings.Index(providerID, ":"); idx >= 0 {
		if !strings.HasPrefix(providerID, "tmdb:") {
			return 0, fmt.Errorf("unsupported provider: %s", providerID)
		}
		idStr = providerID[idx+1:]
	} else if idx := strings.Index(providerID, "."); idx >= 0 {
		if !strings.HasPrefix(providerID, "tmdb.") {
			return 0, fmt.Errorf("unsupported provider: %s", providerID)
		}
		idStr = providerID[idx+1:]
	} else {
		return 0, fmt.Errorf("invalid provider ID format: %s", providerID)
	}

	tmdbID, err := strconv.Atoi(idStr)
	if err != nil || tmdbID <= 0 {
		return 0, fmt.Errorf("invalid TMDB ID: %s", idStr)
	}

	return tmdbID, nil
}

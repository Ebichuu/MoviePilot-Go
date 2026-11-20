package scrapeflow

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
)

const (
	leaseDuration = 2 * time.Minute
	retryDelay    = 2 * time.Minute
)

// Service executes one claimed NFO request and persists its outcome.
type Service struct {
	store   Store
	scraper Scraper
	now     func() time.Time
}

// NewService creates an NFO request executor.
func NewService(store Store, scraper Scraper) (*Service, error) {
	if store == nil || scraper == nil {
		return nil, ErrInvalidPlan
	}
	return &Service{store: store, scraper: scraper, now: time.Now}, nil
}

// Handle performs at most one external scrape attempt.
func (s *Service) Handle(ctx context.Context, requestID uuid.UUID) error {
	if ctx == nil || s == nil || s.store == nil || s.scraper == nil || s.now == nil || requestID == uuid.Nil {
		return ErrInvalidPlan
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	now := s.now().UTC()
	lease, claimed, err := s.store.ClaimScrape(ctx, requestID, now, leaseDuration)
	if err != nil {
		return ErrPersistence
	}
	if !claimed {
		return nil
	}
	if !lease.Valid(requestID) {
		return ErrPersistence
	}
	if err := s.execute(ctx, lease.Request); err != nil {
		if contextErr := stableContextError(ctx, err); contextErr != nil {
			return contextErr
		}
		if err := s.store.RescheduleScrape(ctx, requestID, lease.Token, now.Add(retryDelay), "scrape_failed"); err != nil {
			return ErrPersistence
		}
		return nil
	}
	if err := s.store.CompleteScrape(ctx, requestID, lease.Token, now); err != nil {
		return ErrPersistence
	}
	return nil
}

// ImageScraper 是可选能力：在 NFO 写入成功后继续写入媒体图片（V2 图片刮削）。
type ImageScraper interface {
	ScrapeMovieImages(ctx context.Context, tmdbID int, mediaPath string) error
	ScrapeTVShowImages(ctx context.Context, tmdbID int, mediaPath string) error
}

// EpisodeImageScraper 是可选的单集图片刮削能力。
type EpisodeImageScraper interface {
	ScrapeEpisodeImages(ctx context.Context, tmdbID, season, episode int, mediaPath string) error
}

func (s *Service) execute(ctx context.Context, request Request) error {
	switch request.Kind {
	case KindMovie:
		if err := s.scraper.ScrapeMovieNFO(ctx, request.TMDBID, request.MediaPath); err != nil {
			return err
		}
		if contextErr := stableContextError(ctx, nil); contextErr != nil {
			return contextErr
		}
		if images, ok := s.scraper.(ImageScraper); ok {
			// V2 图片失败不阻塞 NFO 结果，单独记录由 scraper 负责。
			if contextErr := stableContextError(ctx,
				images.ScrapeMovieImages(ctx, request.TMDBID, request.MediaPath)); contextErr != nil {
				return contextErr
			}
		}
		return nil
	case KindTVShow:
		if err := s.scraper.ScrapeTVShowNFO(ctx, request.TMDBID, request.MediaPath); err != nil {
			return err
		}
		if contextErr := stableContextError(ctx, nil); contextErr != nil {
			return contextErr
		}
		if images, ok := s.scraper.(ImageScraper); ok {
			if contextErr := stableContextError(ctx,
				images.ScrapeTVShowImages(ctx, request.TMDBID, request.MediaPath)); contextErr != nil {
				return contextErr
			}
		}
		return nil
	case KindEpisode:
		if err := s.scraper.ScrapeEpisodeNFO(ctx, request.TMDBID, request.Season, request.Episode, request.MediaPath); err != nil {
			return err
		}
		if contextErr := stableContextError(ctx, nil); contextErr != nil {
			return contextErr
		}
		if images, ok := s.scraper.(EpisodeImageScraper); ok {
			if contextErr := stableContextError(ctx,
				images.ScrapeEpisodeImages(ctx, request.TMDBID, request.Season, request.Episode, request.MediaPath)); contextErr != nil {
				return contextErr
			}
		}
		return nil
	default:
		return ErrExecution
	}
}

// SweepHandler recovers expired leases and enqueues a bounded due page.
type SweepHandler struct {
	store    SweepStore
	enqueuer Enqueuer
	now      func() time.Time
}

// NewSweepHandler creates a bounded PostgreSQL recovery sweep.
func NewSweepHandler(store SweepStore, enqueuer Enqueuer) (*SweepHandler, error) {
	if store == nil || enqueuer == nil {
		return nil, ErrInvalidPlan
	}
	return &SweepHandler{store: store, enqueuer: enqueuer, now: time.Now}, nil
}

// Handle performs one recovery pass.
func (h *SweepHandler) Handle(ctx context.Context) error {
	if ctx == nil || h == nil || h.store == nil || h.enqueuer == nil || h.now == nil {
		return ErrInvalidPlan
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	now := h.now().UTC()
	if _, err := h.store.RecoverExpiredScrapes(ctx, now, SweepPageSize); err != nil {
		return ErrPersistence
	}
	ids, err := h.store.ListDueScrapes(ctx, now, SweepPageSize)
	if err != nil || len(ids) > SweepPageSize {
		return ErrPersistence
	}
	seen := make(map[uuid.UUID]struct{}, len(ids))
	for _, id := range ids {
		if id == uuid.Nil {
			return ErrPersistence
		}
		if _, exists := seen[id]; exists {
			return ErrPersistence
		}
		seen[id] = struct{}{}
		if err := h.enqueuer.EnqueueScrape(ctx, id); err != nil {
			if contextErr := stableContextError(ctx, err); contextErr != nil {
				return contextErr
			}
			return ErrPersistence
		}
	}
	return nil
}

func stableContextError(ctx context.Context, err error) error {
	if ctx != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return nil
}

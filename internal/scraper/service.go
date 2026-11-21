package scraper

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// MetadataProvider provides metadata for movies and TV shows.
type MetadataProvider interface {
	// GetMovieDetails returns movie details including cast, crew, and images.
	GetMovieDetails(ctx context.Context, tmdbID int) (*MovieDetails, error)

	// GetTVShowDetails returns TV show details including cast and images.
	GetTVShowDetails(ctx context.Context, tmdbID int) (*TVShowDetails, error)

	// GetEpisodeDetails returns episode details for a specific season and episode.
	GetEpisodeDetails(ctx context.Context, tmdbID int, season, episode int) (*EpisodeDetails, error)
}

// SeasonPosterProvider 是可选的季海报查询能力，不要求其它元数据源实现。
type SeasonPosterProvider interface {
	GetSeasonPosterURL(ctx context.Context, tmdbID, season int) (string, error)
}

// MovieDetails holds detailed movie metadata.
type MovieDetails struct {
	Title         string
	OriginalTitle string
	Year          int
	Rating        float64
	Plot          string
	Tagline       string
	Runtime       int
	Premiered     time.Time
	Genres        []string
	Directors     []string
	Writers       []string
	Cast          []CastMember
	PosterURL     string
	BackdropURL   string
	LogoURL       string
	ClearArtURL   string
	BannerURL     string
	ThumbURL      string
	DiscURL       string
	TMDBID        int
	IMDBID        string
}

// TVShowDetails holds detailed TV show metadata.
type TVShowDetails struct {
	Title         string
	OriginalTitle string
	Year          int
	Rating        float64
	Plot          string
	Tagline       string
	Premiered     time.Time
	Genres        []string
	Cast          []CastMember
	PosterURL     string
	BackdropURL   string
	LogoURL       string
	ClearArtURL   string
	BannerURL     string
	ThumbURL      string
	TMDBID        int
	IMDBID        string
	TVDBID        int
}

// EpisodeDetails holds detailed episode metadata.
type EpisodeDetails struct {
	Title      string
	Season     int
	Episode    int
	Rating     float64
	Plot       string
	Runtime    int
	EpisodeAir time.Time
	Directors  []string
	Writers    []string
	Cast       []CastMember
	PosterURL  string
	TMDBID     int
	TVDBID     int
}

// ScraperService coordinates metadata fetching and NFO generation.
type ScraperService struct {
	provider MetadataProvider
	writer   *NFOWriter
	images   *ImageWriter
	logger   *slog.Logger
}

// NewScraperService creates a new scraper service.
func NewScraperService(provider MetadataProvider, writer *NFOWriter, logger *slog.Logger) *ScraperService {
	if logger == nil {
		logger = slog.Default()
	}
	return &ScraperService{
		provider: provider,
		writer:   writer,
		images:   NewImageWriter(),
		logger:   logger,
	}
}

// ScrapeMovieImages 复刻 V2 电影图片刮削：在电影目录写入 poster/backdrop。
// 图片失败逐个记录，不阻塞 NFO；`PosterURL`/`BackdropURL` 为空时跳过。
func (s *ScraperService) ScrapeMovieImages(ctx context.Context, tmdbID int, mediaPath string) error {
	if s.provider == nil {
		return fmt.Errorf("metadata provider is not configured")
	}
	details, err := s.provider.GetMovieDetails(ctx, tmdbID)
	if err != nil {
		return fmt.Errorf("fetch movie details: %w", err)
	}
	return s.writeImages(ctx, mediaPath, map[string]string{
		"poster":   details.PosterURL,
		"backdrop": details.BackdropURL,
		"logo":     details.LogoURL,
		"clearart": details.ClearArtURL,
		"banner":   details.BannerURL,
		"thumb":    details.ThumbURL,
		"disc":     details.DiscURL,
	})
}

// ScrapeTVShowImages 复刻 V2 电视剧图片刮削：写入 poster/backdrop 以及可选的 Fanart 高清艺术图。
func (s *ScraperService) ScrapeTVShowImages(ctx context.Context, tmdbID int, mediaPath string) error {
	if s.provider == nil {
		return fmt.Errorf("metadata provider is not configured")
	}
	details, err := s.provider.GetTVShowDetails(ctx, tmdbID)
	if err != nil {
		return fmt.Errorf("fetch TV show details: %w", err)
	}
	return s.writeImages(ctx, mediaPath, map[string]string{
		"poster":   details.PosterURL,
		"backdrop": details.BackdropURL,
		"logo":     details.LogoURL,
		"clearart": details.ClearArtURL,
		"banner":   details.BannerURL,
		"thumb":    details.ThumbURL,
	})
}

// ScrapeEpisodeImages 获取并保存单集剧照与该季海报。
func (s *ScraperService) ScrapeEpisodeImages(ctx context.Context, tmdbID, season, episode int, mediaPath string) error {
	if s.provider == nil {
		return fmt.Errorf("metadata provider is not configured")
	}
	if s.images == nil {
		return nil
	}
	details, err := s.provider.GetEpisodeDetails(ctx, tmdbID, season, episode)
	if err != nil {
		if contextErr := imageContextError(ctx, err); contextErr != nil {
			return contextErr
		}
		s.logger.WarnContext(ctx, "单集图片元数据查询失败",
			"season", season, "episode", episode, "path", mediaPath, "error", err)
	} else if details != nil && strings.TrimSpace(details.PosterURL) != "" {
		if _, err := s.images.WriteEpisodeImage(ctx, mediaPath, details.PosterURL); err != nil {
			if contextErr := imageContextError(ctx, err); contextErr != nil {
				return contextErr
			}
			s.logger.WarnContext(ctx, "单集图片刮削失败",
				"path", mediaPath, "error", err)
		}
	}
	provider, ok := s.provider.(SeasonPosterProvider)
	if !ok {
		return nil
	}
	posterURL, err := provider.GetSeasonPosterURL(ctx, tmdbID, season)
	if err != nil {
		if contextErr := imageContextError(ctx, err); contextErr != nil {
			return contextErr
		}
		s.logger.WarnContext(ctx, "季海报查询失败",
			"season", season, "path", mediaPath, "error", err)
		return nil
	}
	if strings.TrimSpace(posterURL) == "" {
		return nil
	}
	if _, err := s.images.WriteSeasonImage(ctx, mediaPath, season, posterURL); err != nil {
		if contextErr := imageContextError(ctx, err); contextErr != nil {
			return contextErr
		}
		s.logger.WarnContext(ctx, "季海报刮削失败",
			"season", season, "path", mediaPath, "error", err)
	}
	return nil
}

func imageContextError(ctx context.Context, err error) error {
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

func (s *ScraperService) writeImages(ctx context.Context, mediaPath string, images map[string]string) error {
	if s.images == nil {
		return nil
	}
	for imageType, sourceURL := range images {
		if strings.TrimSpace(sourceURL) == "" {
			continue
		}
		if _, err := s.images.WriteMediaImage(ctx, mediaPath, imageType, sourceURL); err != nil {
			if contextErr := ctx.Err(); contextErr != nil {
				return contextErr
			}
			s.logger.WarnContext(ctx, "媒体图片刮削失败",
				"type", imageType, "path", mediaPath, "error", err)
		}
	}
	return nil
}

// ScrapeMovieNFO fetches movie metadata and writes NFO file.
func (s *ScraperService) ScrapeMovieNFO(ctx context.Context, tmdbID int, mediaPath string) error {
	if s.provider == nil {
		return fmt.Errorf("metadata provider is not configured")
	}

	s.logger.Info("fetching movie metadata", slog.Int("tmdb_id", tmdbID))
	details, err := s.provider.GetMovieDetails(ctx, tmdbID)
	if err != nil {
		return fmt.Errorf("fetch movie details: %w", err)
	}

	meta := NFOMetadata{
		Type:          NFOTypeMovie,
		Title:         details.Title,
		OriginalTitle: details.OriginalTitle,
		Year:          details.Year,
		Rating:        details.Rating,
		Plot:          details.Plot,
		Tagline:       details.Tagline,
		Runtime:       details.Runtime,
		Premiered:     details.Premiered,
		Genres:        details.Genres,
		Directors:     details.Directors,
		Writers:       details.Writers,
		Cast:          details.Cast,
		PosterURL:     details.PosterURL,
		BackdropURL:   details.BackdropURL,
		TMDBID:        details.TMDBID,
		IMDBID:        details.IMDBID,
	}

	if err := s.writer.WriteNFOForMedia(ctx, mediaPath, meta); err != nil {
		return fmt.Errorf("write movie NFO: %w", err)
	}

	s.logger.Info("movie NFO created", slog.String("path", mediaPath))
	return nil
}

// ScrapeTVShowNFO fetches TV show metadata and writes NFO file.
func (s *ScraperService) ScrapeTVShowNFO(ctx context.Context, tmdbID int, mediaPath string) error {
	if s.provider == nil {
		return fmt.Errorf("metadata provider is not configured")
	}

	s.logger.Info("fetching TV show metadata", slog.Int("tmdb_id", tmdbID))
	details, err := s.provider.GetTVShowDetails(ctx, tmdbID)
	if err != nil {
		return fmt.Errorf("fetch TV show details: %w", err)
	}

	meta := NFOMetadata{
		Type:          NFOTypeTVShow,
		Title:         details.Title,
		OriginalTitle: details.OriginalTitle,
		Year:          details.Year,
		Rating:        details.Rating,
		Plot:          details.Plot,
		Tagline:       details.Tagline,
		Premiered:     details.Premiered,
		Genres:        details.Genres,
		Cast:          details.Cast,
		PosterURL:     details.PosterURL,
		BackdropURL:   details.BackdropURL,
		TMDBID:        details.TMDBID,
		IMDBID:        details.IMDBID,
		TVDBID:        details.TVDBID,
	}

	if err := s.writer.WriteNFOForMedia(ctx, mediaPath, meta); err != nil {
		return fmt.Errorf("write TV show NFO: %w", err)
	}

	s.logger.Info("TV show NFO created", slog.String("path", mediaPath))
	return nil
}

// ScrapeEpisodeNFO fetches episode metadata and writes NFO file.
func (s *ScraperService) ScrapeEpisodeNFO(ctx context.Context, tmdbID int, season, episode int, mediaPath string) error {
	if s.provider == nil {
		return fmt.Errorf("metadata provider is not configured")
	}

	s.logger.Info("fetching episode metadata",
		slog.Int("tmdb_id", tmdbID),
		slog.Int("season", season),
		slog.Int("episode", episode))

	details, err := s.provider.GetEpisodeDetails(ctx, tmdbID, season, episode)
	if err != nil {
		return fmt.Errorf("fetch episode details: %w", err)
	}

	meta := NFOMetadata{
		Type:       NFOTypeEpisode,
		Title:      details.Title,
		Season:     details.Season,
		Episode:    details.Episode,
		Rating:     details.Rating,
		Plot:       details.Plot,
		Runtime:    details.Runtime,
		EpisodeAir: details.EpisodeAir,
		Directors:  details.Directors,
		Writers:    details.Writers,
		Cast:       details.Cast,
		PosterURL:  details.PosterURL,
		TMDBID:     details.TMDBID,
		TVDBID:     details.TVDBID,
	}

	if err := s.writer.WriteNFOForMedia(ctx, mediaPath, meta); err != nil {
		return fmt.Errorf("write episode NFO: %w", err)
	}

	s.logger.Info("episode NFO created", slog.String("path", mediaPath))
	return nil
}

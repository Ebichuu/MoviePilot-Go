package scraper

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Mock metadata provider for testing.
type mockMetadataProvider struct {
	movieDetails   *MovieDetails
	tvShowDetails  *TVShowDetails
	episodeDetails *EpisodeDetails
	err            error
}

func (m *mockMetadataProvider) GetMovieDetails(ctx context.Context, tmdbID int) (*MovieDetails, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.movieDetails, nil
}

func (m *mockMetadataProvider) GetTVShowDetails(ctx context.Context, tmdbID int) (*TVShowDetails, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.tvShowDetails, nil
}

func (m *mockMetadataProvider) GetEpisodeDetails(ctx context.Context, tmdbID int, season, episode int) (*EpisodeDetails, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.episodeDetails, nil
}

func TestScraperService_ScrapeMovieNFO(t *testing.T) {
	tempDir := t.TempDir()
	mediaPath := filepath.Join(tempDir, "Movie (2010)", "Movie (2010).mkv")

	// Create parent directory.
	if err := os.MkdirAll(filepath.Dir(mediaPath), 0755); err != nil {
		t.Fatalf("Failed to create test directory: %v", err)
	}

	provider := &mockMetadataProvider{
		movieDetails: &MovieDetails{
			Title:         "Inception",
			OriginalTitle: "Inception",
			Year:          2010,
			Rating:        8.8,
			Plot:          "A thief who steals corporate secrets.",
			Tagline:       "Your mind is the scene of the crime.",
			Runtime:       148,
			Premiered:     time.Date(2010, 7, 16, 0, 0, 0, 0, time.UTC),
			Genres:        []string{"Action", "Sci-Fi"},
			Directors:     []string{"Christopher Nolan"},
			Writers:       []string{"Christopher Nolan"},
			Cast: []CastMember{
				{Name: "Leonardo DiCaprio", Role: "Cobb", Order: 0},
			},
			PosterURL:   "https://image.tmdb.org/poster.jpg",
			BackdropURL: "https://image.tmdb.org/backdrop.jpg",
			TMDBID:      27205,
			IMDBID:      "tt1375666",
		},
	}

	writer := NewNFOWriter()
	service := NewScraperService(provider, writer, slog.Default())

	ctx := context.Background()
	if err := service.ScrapeMovieNFO(ctx, 27205, mediaPath); err != nil {
		t.Fatalf("ScrapeMovieNFO failed: %v", err)
	}

	// Verify NFO file was created.
	nfoPath := filepath.Join(tempDir, "Movie (2010)", "Movie (2010).nfo")
	if _, err := os.Stat(nfoPath); os.IsNotExist(err) {
		t.Fatalf("NFO file was not created at %s", nfoPath)
	}

	// Verify NFO content.
	nfoData, err := os.ReadFile(nfoPath)
	if err != nil {
		t.Fatalf("Failed to read NFO file: %v", err)
	}

	content := string(nfoData)
	if !strings.Contains(content, "<title>Inception</title>") {
		t.Error("NFO missing title")
	}
	if !strings.Contains(content, "<year>2010</year>") {
		t.Error("NFO missing year")
	}
	if !strings.Contains(content, `<uniqueid type="tmdb" default="true">27205</uniqueid>`) {
		t.Error("NFO missing TMDB ID")
	}
}

func TestScraperService_ScrapeTVShowNFO(t *testing.T) {
	tempDir := t.TempDir()
	mediaPath := filepath.Join(tempDir, "Show", "tvshow.nfo")

	provider := &mockMetadataProvider{
		tvShowDetails: &TVShowDetails{
			Title:         "Game of Thrones",
			OriginalTitle: "Game of Thrones",
			Year:          2011,
			Rating:        9.3,
			Plot:          "Nine noble families fight for control.",
			Premiered:     time.Date(2011, 4, 17, 0, 0, 0, 0, time.UTC),
			Genres:        []string{"Drama", "Fantasy"},
			Cast: []CastMember{
				{Name: "Emilia Clarke", Role: "Daenerys Targaryen", Order: 0},
			},
			PosterURL:   "https://image.tmdb.org/poster.jpg",
			BackdropURL: "https://image.tmdb.org/backdrop.jpg",
			TMDBID:      1399,
			TVDBID:      121361,
		},
	}

	writer := NewNFOWriter()
	service := NewScraperService(provider, writer, slog.Default())

	ctx := context.Background()
	if err := service.ScrapeTVShowNFO(ctx, 1399, mediaPath); err != nil {
		t.Fatalf("ScrapeTVShowNFO failed: %v", err)
	}

	// Verify NFO file was created.
	if _, err := os.Stat(mediaPath); os.IsNotExist(err) {
		t.Fatalf("NFO file was not created at %s", mediaPath)
	}

	// Verify NFO content.
	nfoData, err := os.ReadFile(mediaPath)
	if err != nil {
		t.Fatalf("Failed to read NFO file: %v", err)
	}

	content := string(nfoData)
	if !strings.Contains(content, "<title>Game of Thrones</title>") {
		t.Error("NFO missing title")
	}
	if !strings.Contains(content, `<uniqueid type="tvdb">121361</uniqueid>`) {
		t.Error("NFO missing TVDB ID")
	}
}

func TestScraperService_ScrapeEpisodeNFO(t *testing.T) {
	tempDir := t.TempDir()
	mediaPath := filepath.Join(tempDir, "Show", "Season 01", "Show S01E01.mp4")

	// Create parent directory.
	if err := os.MkdirAll(filepath.Dir(mediaPath), 0755); err != nil {
		t.Fatalf("Failed to create test directory: %v", err)
	}

	provider := &mockMetadataProvider{
		episodeDetails: &EpisodeDetails{
			Title:      "Winter Is Coming",
			Season:     1,
			Episode:    1,
			Rating:     8.9,
			Plot:       "Eddard Stark is torn between his family and an old friend.",
			Runtime:    62,
			EpisodeAir: time.Date(2011, 4, 17, 0, 0, 0, 0, time.UTC),
			Directors:  []string{"Tim Van Patten"},
			Writers:    []string{"David Benioff", "D. B. Weiss"},
			Cast: []CastMember{
				{Name: "Sean Bean", Role: "Eddard Stark", Order: 0},
			},
			PosterURL: "https://image.tmdb.org/episode.jpg",
			TMDBID:    63056,
			TVDBID:    3254641,
		},
	}

	writer := NewNFOWriter()
	service := NewScraperService(provider, writer, slog.Default())

	ctx := context.Background()
	if err := service.ScrapeEpisodeNFO(ctx, 1399, 1, 1, mediaPath); err != nil {
		t.Fatalf("ScrapeEpisodeNFO failed: %v", err)
	}

	// Verify NFO file was created.
	nfoPath := filepath.Join(tempDir, "Show", "Season 01", "Show S01E01.nfo")
	if _, err := os.Stat(nfoPath); os.IsNotExist(err) {
		t.Fatalf("NFO file was not created at %s", nfoPath)
	}

	// Verify NFO content.
	nfoData, err := os.ReadFile(nfoPath)
	if err != nil {
		t.Fatalf("Failed to read NFO file: %v", err)
	}

	content := string(nfoData)
	if !strings.Contains(content, "<title>Winter Is Coming</title>") {
		t.Error("NFO missing title")
	}
	if !strings.Contains(content, "<season>1</season>") {
		t.Error("NFO missing season")
	}
	if !strings.Contains(content, "<episode>1</episode>") {
		t.Error("NFO missing episode")
	}
}

func TestScraperService_ProviderError(t *testing.T) {
	tempDir := t.TempDir()
	mediaPath := filepath.Join(tempDir, "Movie.mkv")

	provider := &mockMetadataProvider{
		err: errors.New("provider failed"),
	}

	writer := NewNFOWriter()
	service := NewScraperService(provider, writer, slog.Default())

	ctx := context.Background()
	err := service.ScrapeMovieNFO(ctx, 999, mediaPath)
	if err == nil {
		t.Error("Expected error from provider, got nil")
	}
	if !strings.Contains(err.Error(), "fetch movie details") {
		t.Errorf("Error = %q, want to contain 'fetch movie details'", err.Error())
	}
}

func TestScraperService_NoProvider(t *testing.T) {
	tempDir := t.TempDir()
	mediaPath := filepath.Join(tempDir, "Movie.mkv")

	writer := NewNFOWriter()
	service := NewScraperService(nil, writer, slog.Default())

	ctx := context.Background()
	err := service.ScrapeMovieNFO(ctx, 999, mediaPath)
	if err == nil {
		t.Error("Expected error for nil provider, got nil")
	}
	if !strings.Contains(err.Error(), "metadata provider is not configured") {
		t.Errorf("Error = %q, want to contain 'metadata provider is not configured'", err.Error())
	}
}

func TestNewScraperService_NilLogger(t *testing.T) {
	provider := &mockMetadataProvider{}
	writer := NewNFOWriter()

	// Should not panic with nil logger.
	service := NewScraperService(provider, writer, nil)
	if service == nil {
		t.Error("NewScraperService returned nil")
	}
	if service.logger == nil {
		t.Error("Logger was not set to default")
	}
}

package organizationflow

import (
	"context"
	"errors"
	"testing"

	"github.com/moviepilot-go/moviepilot-go/internal/mediaindex"
)

type mockNFOScraper struct {
	movieCalls   []movieNFOCall
	tvShowCalls  []tvShowNFOCall
	episodeCalls []episodeNFOCall
	movieErr     error
	tvShowErr    error
	episodeErr   error
}

type movieNFOCall struct {
	tmdbID    int
	mediaPath string
}

type tvShowNFOCall struct {
	tmdbID    int
	mediaPath string
}

type episodeNFOCall struct {
	tmdbID    int
	season    int
	episode   int
	mediaPath string
}

func (m *mockNFOScraper) ScrapeMovieNFO(ctx context.Context, tmdbID int, mediaPath string) error {
	m.movieCalls = append(m.movieCalls, movieNFOCall{tmdbID: tmdbID, mediaPath: mediaPath})
	return m.movieErr
}

func (m *mockNFOScraper) ScrapeTVShowNFO(ctx context.Context, tmdbID int, mediaPath string) error {
	m.tvShowCalls = append(m.tvShowCalls, tvShowNFOCall{tmdbID: tmdbID, mediaPath: mediaPath})
	return m.tvShowErr
}

func (m *mockNFOScraper) ScrapeEpisodeNFO(ctx context.Context, tmdbID int, season, episode int, mediaPath string) error {
	m.episodeCalls = append(m.episodeCalls, episodeNFOCall{
		tmdbID: tmdbID, season: season, episode: episode, mediaPath: mediaPath,
	})
	return m.episodeErr
}

func TestParseTMDBID(t *testing.T) {
	tests := []struct {
		name       string
		providerID string
		want       int
		wantErr    bool
	}{
		{
			name:       "colon format",
			providerID: "tmdb:123",
			want:       123,
			wantErr:    false,
		},
		{
			name:       "dot format",
			providerID: "tmdb.456",
			want:       456,
			wantErr:    false,
		},
		{
			name:       "empty",
			providerID: "",
			want:       0,
			wantErr:    true,
		},
		{
			name:       "invalid provider",
			providerID: "imdb:123",
			want:       0,
			wantErr:    true,
		},
		{
			name:       "invalid format",
			providerID: "tmdb",
			want:       0,
			wantErr:    true,
		},
		{
			name:       "non-numeric ID",
			providerID: "tmdb:abc",
			want:       0,
			wantErr:    true,
		},
		{
			name:       "zero ID",
			providerID: "tmdb:0",
			want:       0,
			wantErr:    true,
		},
		{
			name:       "negative ID",
			providerID: "tmdb:-1",
			want:       0,
			wantErr:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseTMDBID(tt.providerID)
			if (err != nil) != tt.wantErr {
				t.Errorf("parseTMDBID() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("parseTMDBID() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestScrapeOneNFO(t *testing.T) {
	ctx := context.Background()

	t.Run("movie", func(t *testing.T) {
		scraper := &mockNFOScraper{}
		h := &Handler{nfoScraper: scraper}

		file := mediaindex.TargetFile{
			LocalPath:  "/media/Movie.mkv",
			ProviderID: "tmdb:123",
			MediaType:  "movie",
		}

		err := h.scrapeOneNFO(ctx, file)
		if err != nil {
			t.Fatalf("scrapeOneNFO() error = %v", err)
		}

		if len(scraper.movieCalls) != 1 {
			t.Fatalf("expected 1 movie call, got %d", len(scraper.movieCalls))
		}

		call := scraper.movieCalls[0]
		if call.tmdbID != 123 {
			t.Errorf("tmdbID = %d, want 123", call.tmdbID)
		}
		if call.mediaPath != "/media/Movie.mkv" {
			t.Errorf("mediaPath = %s, want /media/Movie.mkv", call.mediaPath)
		}
	})

	t.Run("tv show", func(t *testing.T) {
		scraper := &mockNFOScraper{}
		h := &Handler{nfoScraper: scraper}

		file := mediaindex.TargetFile{
			LocalPath:  "/media/Show/tvshow.nfo",
			ProviderID: "tmdb:456",
			MediaType:  "tv",
			Season:     0,
			Episode:    0,
		}

		err := h.scrapeOneNFO(ctx, file)
		if err != nil {
			t.Fatalf("scrapeOneNFO() error = %v", err)
		}

		if len(scraper.tvShowCalls) != 1 {
			t.Fatalf("expected 1 tv show call, got %d", len(scraper.tvShowCalls))
		}

		call := scraper.tvShowCalls[0]
		if call.tmdbID != 456 {
			t.Errorf("tmdbID = %d, want 456", call.tmdbID)
		}
		if call.mediaPath != "/media/Show/tvshow.nfo" {
			t.Errorf("mediaPath = %s, want /media/Show/tvshow.nfo", call.mediaPath)
		}
	})

	t.Run("episode", func(t *testing.T) {
		scraper := &mockNFOScraper{}
		h := &Handler{nfoScraper: scraper}

		file := mediaindex.TargetFile{
			LocalPath:  "/media/Show/S01E02.mkv",
			ProviderID: "tmdb.789",
			MediaType:  "tv",
			Season:     1,
			Episode:    2,
		}

		err := h.scrapeOneNFO(ctx, file)
		if err != nil {
			t.Fatalf("scrapeOneNFO() error = %v", err)
		}

		if len(scraper.episodeCalls) != 1 {
			t.Fatalf("expected 1 episode call, got %d", len(scraper.episodeCalls))
		}

		call := scraper.episodeCalls[0]
		if call.tmdbID != 789 {
			t.Errorf("tmdbID = %d, want 789", call.tmdbID)
		}
		if call.season != 1 {
			t.Errorf("season = %d, want 1", call.season)
		}
		if call.episode != 2 {
			t.Errorf("episode = %d, want 2", call.episode)
		}
		if call.mediaPath != "/media/Show/S01E02.mkv" {
			t.Errorf("mediaPath = %s, want /media/Show/S01E02.mkv", call.mediaPath)
		}
	})

	t.Run("invalid provider ID", func(t *testing.T) {
		scraper := &mockNFOScraper{}
		h := &Handler{nfoScraper: scraper}

		file := mediaindex.TargetFile{
			LocalPath:  "/media/Movie.mkv",
			ProviderID: "imdb:tt123456",
			MediaType:  "movie",
		}

		err := h.scrapeOneNFO(ctx, file)
		if err == nil {
			t.Fatal("expected error for invalid provider ID")
		}
	})

	t.Run("unsupported media type", func(t *testing.T) {
		scraper := &mockNFOScraper{}
		h := &Handler{nfoScraper: scraper}

		file := mediaindex.TargetFile{
			LocalPath:  "/media/Music.mp3",
			ProviderID: "tmdb:123",
			MediaType:  "music",
		}

		err := h.scrapeOneNFO(ctx, file)
		if err == nil {
			t.Fatal("expected error for unsupported media type")
		}
	})

	t.Run("scraper returns error", func(t *testing.T) {
		scraper := &mockNFOScraper{movieErr: errors.New("tmdb API error")}
		h := &Handler{nfoScraper: scraper}

		file := mediaindex.TargetFile{
			LocalPath:  "/media/Movie.mkv",
			ProviderID: "tmdb:123",
			MediaType:  "movie",
		}

		err := h.scrapeOneNFO(ctx, file)
		if err == nil {
			t.Fatal("expected error from scraper")
		}
	})
}

func TestScrapeNFOFiles(t *testing.T) {
	ctx := context.Background()

	t.Run("multiple files", func(t *testing.T) {
		scraper := &mockNFOScraper{}
		h := &Handler{nfoScraper: scraper}

		plan := mediaindex.CompletionPlan{
			Files: []mediaindex.TargetFile{
				{LocalPath: "/media/Movie1.mkv", ProviderID: "tmdb:100", MediaType: "movie"},
				{LocalPath: "/media/Movie2.mkv", ProviderID: "tmdb:200", MediaType: "movie"},
				{LocalPath: "/media/Show/S01E01.mkv", ProviderID: "tmdb:300", MediaType: "tv", Season: 1, Episode: 1},
			},
		}

		h.scrapeNFOFiles(ctx, plan)

		if len(scraper.movieCalls) != 2 {
			t.Errorf("expected 2 movie calls, got %d", len(scraper.movieCalls))
		}
		if len(scraper.episodeCalls) != 1 {
			t.Errorf("expected 1 episode call, got %d", len(scraper.episodeCalls))
		}
	})

	t.Run("nil scraper", func(t *testing.T) {
		h := &Handler{nfoScraper: nil}

		plan := mediaindex.CompletionPlan{
			Files: []mediaindex.TargetFile{
				{LocalPath: "/media/Movie.mkv", ProviderID: "tmdb:123", MediaType: "movie"},
			},
		}

		// Should not panic
		h.scrapeNFOFiles(ctx, plan)
	})

	t.Run("continues on error", func(t *testing.T) {
		scraper := &mockNFOScraper{movieErr: errors.New("API error")}
		h := &Handler{nfoScraper: scraper}

		plan := mediaindex.CompletionPlan{
			Files: []mediaindex.TargetFile{
				{LocalPath: "/media/Movie1.mkv", ProviderID: "tmdb:100", MediaType: "movie"},
				{LocalPath: "/media/Movie2.mkv", ProviderID: "tmdb:200", MediaType: "movie"},
			},
		}

		// Should not panic and should try all files
		h.scrapeNFOFiles(ctx, plan)

		if len(scraper.movieCalls) != 2 {
			t.Errorf("expected 2 movie calls despite errors, got %d", len(scraper.movieCalls))
		}
	})
}

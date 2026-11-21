package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/moviepilot-go/moviepilot-go/internal/auth"
	"github.com/moviepilot-go/moviepilot-go/internal/config"
)

type fakeManualScraper struct {
	movieNFOs   []int
	movieImgs   []int
	tvNFOs      []int
	episodeNFOs []string
}

func (f *fakeManualScraper) ScrapeMovieNFO(_ context.Context, tmdbID int, _ string) error {
	f.movieNFOs = append(f.movieNFOs, tmdbID)
	return nil
}
func (f *fakeManualScraper) ScrapeMovieImages(_ context.Context, tmdbID int, _ string) error {
	f.movieImgs = append(f.movieImgs, tmdbID)
	return nil
}
func (f *fakeManualScraper) ScrapeTVShowNFO(_ context.Context, tmdbID int, _ string) error {
	f.tvNFOs = append(f.tvNFOs, tmdbID)
	return nil
}
func (f *fakeManualScraper) ScrapeTVShowImages(_ context.Context, _ int, _ string) error {
	return nil
}
func (f *fakeManualScraper) ScrapeEpisodeNFO(_ context.Context, tmdbID, season, episode int, _ string) error {
	f.episodeNFOs = append(f.episodeNFOs, filepath.Base(strings.Join([]string{"ep", string(rune('0' + season)), string(rune('0' + episode))}, "-")))
	_ = tmdbID
	return nil
}
func (f *fakeManualScraper) ScrapeEpisodeImages(_ context.Context, _, _, _ int, _ string) error {
	return nil
}

type fakeMediaServerRefresher struct {
	calls int
}

func (f *fakeMediaServerRefresher) RefreshAllMediaServers(_ context.Context) error {
	f.calls++
	return nil
}

func TestScrapeMediaAndManualTransferEnhanced(t *testing.T) {
	t.Parallel()

	srcDir := t.TempDir()
	dstDir := t.TempDir()
	movieFile := filepath.Join(srcDir, "Dune.2021.2160p.mkv")
	if err := os.WriteFile(movieFile, []byte("movie-content"), 0o600); err != nil {
		t.Fatalf("write movie: %v", err)
	}

	scraper := &fakeManualScraper{}
	refresher := &fakeMediaServerRefresher{}
	authSvc := auth.NewService(nil, "scrape-jwt", "scrape-res", 3600_000_000_000)
	admin := auth.User{ID: 1, Username: "admin", SuperUser: true}
	token, _ := authSvc.AccessToken(admin)

	router := NewRouter(Dependencies{
		Config:               config.Config{MaxRequestBody: 1 << 20, FrontendDir: t.TempDir()},
		Auth:                 authSvc,
		Users:                fakeUserLookup{user: admin},
		Scraper:              scraper,
		MediaServerRefresher: refresher,
	})

	t.Run("POST /media/scrape/local scrapes movie NFO and images", func(t *testing.T) {
		payload, _ := json.Marshal(map[string]any{"path": movieFile, "name": "Dune.2021.2160p.mkv"})
		req := httptest.NewRequest(http.MethodPost, "/api/v1/media/scrape/local?media_source=themoviedb&media_id=438631&type_name=电影", strings.NewReader(string(payload)))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)

		var resp Response
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || !resp.Success {
			t.Fatalf("scrape response = %s", rec.Body.String())
		}
		if len(scraper.movieNFOs) != 1 || scraper.movieNFOs[0] != 438631 || len(scraper.movieImgs) != 1 {
			t.Fatalf("scraper calls nfo=%v imgs=%v", scraper.movieNFOs, scraper.movieImgs)
		}
	})

	t.Run("POST /transfer/manual copy + scrape + refresh", func(t *testing.T) {
		targetFile := filepath.Join(dstDir, "Dune (2021)", "Dune (2021).mkv")
		payload, _ := json.Marshal(map[string]any{
			"source_path":         movieFile,
			"target_path":         targetFile,
			"storage":             "local",
			"transfer_type":       "copy",
			"scrape":              true,
			"refresh_mediaserver": true,
			"tmdbid":              438631,
			"media_type":          "电影",
		})
		req := httptest.NewRequest(http.MethodPost, "/api/v1/transfer/manual", strings.NewReader(string(payload)))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)

		var resp Response
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || !resp.Success {
			t.Fatalf("manual transfer response = %s", rec.Body.String())
		}
		if _, err := os.Stat(targetFile); err != nil {
			t.Fatalf("copied target missing: %v", err)
		}
		if _, err := os.Stat(movieFile); err != nil {
			t.Fatalf("source should remain after copy: %v", err)
		}
		if refresher.calls != 1 {
			t.Fatalf("refresher calls = %d, want 1", refresher.calls)
		}
		if len(scraper.movieNFOs) != 2 {
			t.Fatalf("expected second scrape after manual transfer, got %v", scraper.movieNFOs)
		}
	})
}

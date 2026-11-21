package metadata

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestFanartClientPicksPreferredLanguageAndLikes(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("api_key") != "fanart-key" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/movies/438631":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"movieposter": []map[string]any{
					{"url": "http://assets.fanart.tv/fanart/movies/438631/poster-en.jpg", "lang": "en", "likes": "10"},
					{"url": "http://assets.fanart.tv/fanart/movies/438631/poster-zh.jpg", "lang": "zh", "likes": "2"},
				},
				"hdmovielogo": []map[string]any{
					{"url": "https://assets.fanart.tv/fanart/movies/438631/logo-1.png", "lang": "en", "likes": "5"},
					{"url": "https://assets.fanart.tv/fanart/movies/438631/logo-2.png", "lang": "en", "likes": "15"},
				},
				"hdmovieclearart": []map[string]any{
					{"url": "https://assets.fanart.tv/fanart/movies/438631/clearart.png", "lang": "00", "likes": "4"},
				},
				"moviebanner": []map[string]any{
					{"url": "https://assets.fanart.tv/fanart/movies/438631/banner.jpg", "lang": "zh", "likes": "1"},
				},
				"moviedisc": []map[string]any{
					{"url": "https://assets.fanart.tv/fanart/movies/438631/disc.png", "lang": "en", "likes": "3"},
				},
			})
		case "/tv/81189":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"hdtvlogo": []map[string]any{
					{"url": "https://assets.fanart.tv/fanart/tv/81189/logo-zh.png", "lang": "zh", "likes": "3"},
				},
				"seasonposter": []map[string]any{
					{"url": "https://assets.fanart.tv/fanart/tv/81189/s1-en.jpg", "lang": "en", "likes": "2", "season": "1"},
					{"url": "https://assets.fanart.tv/fanart/tv/81189/s1-zh.jpg", "lang": "zh", "likes": "1", "season": "1"},
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	httpClient, err := NewHTTPClient(2, 5*time.Second, 4<<20)
	if err != nil {
		t.Fatalf("NewHTTPClient: %v", err)
	}
	fanart := NewFanart(httpClient, server.URL, "fanart-key", []string{"zh", "en"})
	ctx := context.Background()

	movieImgs, err := fanart.GetMovieImages(ctx, 438631)
	if err != nil {
		t.Fatalf("GetMovieImages: %v", err)
	}
	if movieImgs.PosterURL != "https://assets.fanart.tv/fanart/movies/438631/poster-zh.jpg" {
		t.Fatalf("expected zh poster with https upgrade, got %q", movieImgs.PosterURL)
	}
	if movieImgs.LogoURL != "https://assets.fanart.tv/fanart/movies/438631/logo-2.png" {
		t.Fatalf("expected highest likes en logo, got %q", movieImgs.LogoURL)
	}
	if movieImgs.ClearArtURL == "" || movieImgs.BannerURL == "" || movieImgs.DiscURL == "" {
		t.Fatalf("missing movie fanart fields: %+v", movieImgs)
	}

	tvImgs, err := fanart.GetTVImages(ctx, 81189)
	if err != nil {
		t.Fatalf("GetTVImages: %v", err)
	}
	if tvImgs.LogoURL != "https://assets.fanart.tv/fanart/tv/81189/logo-zh.png" {
		t.Fatalf("unexpected tv logo: %q", tvImgs.LogoURL)
	}
	if tvImgs.SeasonPosters[1] != "https://assets.fanart.tv/fanart/tv/81189/s1-zh.jpg" {
		t.Fatalf("unexpected season 1 poster: %+v", tvImgs.SeasonPosters)
	}
}

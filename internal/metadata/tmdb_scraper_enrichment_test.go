package metadata

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestTMDBScraperAdapterEnrichesWithTVDBAndFanart(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/tmdb/3/movie/438631":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": 438631, "title": "沙丘", "original_title": "Dune",
				"release_date": "2021-10-22", "overview": "电影简介",
				"poster_path": "/poster.jpg", "backdrop_path": "/backdrop.jpg", "imdb_id": "tt1160419",
			})
		case "/tmdb/3/tv/1396":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": 1396, "name": "绝命毒师", "original_name": "Breaking Bad",
				"first_air_date": "2008-01-20", "overview": "",
				"poster_path": "/tv-poster.jpg", "backdrop_path": "",
				"external_ids": map[string]any{"imdb_id": "tt0903747", "tvdb_id": 81189},
			})
		case "/tmdb/3/tv/1396/images":
			_ = json.NewEncoder(w).Encode(map[string]any{"posters": []any{}, "backdrops": []any{}})
		case "/tmdb/3/tv/1396/season/1":
			_ = json.NewEncoder(w).Encode(map[string]any{"poster_path": ""})
		case "/tmdb/3/tv/1396/season/1/episode/1":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": 62085, "name": "", "overview": "", "air_date": "", "still_path": "",
			})
		case "/tvdb/login":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"token": "jwt-ok"}})
		case "/tvdb/series/81189/extended":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{
					"id": 81189, "name": "Breaking Bad",
					"artworks": []map[string]any{{"image": "https://tvdb.example/bg.jpg", "type": 3, "score": 90}},
					"translations": map[string]any{
						"overviewTranslations": []map[string]any{{"overview": "来自TVDB的中文简介", "language": "zho"}},
					},
				},
			})
		case "/tvdb/series/81189/episodes/default/zho":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{
					"episodes": []map[string]any{{
						"id": 9001, "name": "TVDB试播集", "overview": "TVDB单集简介",
						"aired": "2008-01-20", "runtime": 58, "image": "https://tvdb.example/ep1.jpg",
						"seasonNumber": 1, "number": 1,
					}},
				},
			})
		case "/fanart/movies/438631":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"hdmovielogo":     []map[string]any{{"url": "https://fanart.example/movie-logo.png", "lang": "zh", "likes": "9"}},
				"hdmovieclearart": []map[string]any{{"url": "https://fanart.example/movie-clearart.png", "lang": "en", "likes": "4"}},
				"moviebanner":     []map[string]any{{"url": "https://fanart.example/movie-banner.jpg", "lang": "zh", "likes": "2"}},
				"moviedisc":       []map[string]any{{"url": "https://fanart.example/movie-disc.png", "lang": "en", "likes": "5"}},
			})
		case "/fanart/tv/81189":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"hdtvlogo":     []map[string]any{{"url": "https://fanart.example/tv-logo.png", "lang": "zh", "likes": "8"}},
				"seasonposter": []map[string]any{{"url": "https://fanart.example/s1.jpg", "lang": "zh", "likes": "5", "season": "1"}},
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
	tmdb := NewTMDB(httpClient, server.URL+"/tmdb", "tmdb-key")
	tvdb := NewTVDB(httpClient, server.URL+"/tvdb", "tvdb-key", "")
	fanart := NewFanart(httpClient, server.URL+"/fanart", "fanart-key", []string{"zh", "en"})

	adapter := NewTMDBScraperAdapter(tmdb)
	adapter.SetTVDB(tvdb)
	adapter.SetFanart(fanart)

	ctx := context.Background()
	movie, err := adapter.GetMovieDetails(ctx, 438631)
	if err != nil {
		t.Fatalf("GetMovieDetails: %v", err)
	}
	if movie.LogoURL != "https://fanart.example/movie-logo.png" ||
		movie.ClearArtURL != "https://fanart.example/movie-clearart.png" ||
		movie.BannerURL != "https://fanart.example/movie-banner.jpg" ||
		movie.DiscURL != "https://fanart.example/movie-disc.png" {
		t.Fatalf("unexpected movie fanart enrichment: %+v", movie)
	}

	show, err := adapter.GetTVShowDetails(ctx, 1396)
	if err != nil {
		t.Fatalf("GetTVShowDetails: %v", err)
	}
	if show.Plot != "来自TVDB的中文简介" || show.BackdropURL != "https://tvdb.example/bg.jpg" || show.LogoURL != "https://fanart.example/tv-logo.png" {
		t.Fatalf("unexpected show enrichment: %+v", show)
	}

	ep, err := adapter.GetEpisodeDetails(ctx, 1396, 1, 1)
	if err != nil {
		t.Fatalf("GetEpisodeDetails: %v", err)
	}
	if ep.Title != "TVDB试播集" || ep.Plot != "TVDB单集简介" || ep.PosterURL != "https://tvdb.example/ep1.jpg" {
		t.Fatalf("unexpected episode TVDB fallback: %+v", ep)
	}

	seasonPoster, err := adapter.GetSeasonPosterURL(ctx, 1396, 1)
	if err != nil || seasonPoster != "https://fanart.example/s1.jpg" {
		t.Fatalf("unexpected Fanart season poster fallback: %q err=%v", seasonPoster, err)
	}
}

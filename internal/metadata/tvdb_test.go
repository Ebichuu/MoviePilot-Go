package metadata

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestTVDBClientLoginSearchDetailAndEpisode(t *testing.T) {
	logins := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/login":
			logins++
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": "success",
				"data":   map[string]any{"token": "tvdb-jwt-1"},
			})
		case "/search":
			if r.Header.Get("Authorization") != "Bearer tvdb-jwt-1" {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": []map[string]any{
					{
						"tvdb_id":          "81189",
						"name":             "Breaking Bad",
						"type":             "series",
						"year":             "2008",
						"image_url":        "https://artworks.thetvdb.com/poster.jpg",
						"primary_language": "eng",
						"translations":     map[string]any{"zho": "绝命毒师"},
						"overviews":        map[string]any{"zho": "化学老师制毒"},
					},
				},
			})
		case "/series/81189/extended":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{
					"id":         81189,
					"name":       "Breaking Bad",
					"year":       "2008",
					"firstAired": "2008-01-20",
					"overview":   "English overview",
					"image":      "https://artworks.thetvdb.com/poster.jpg",
					"score":      9.5,
					"genres":     []map[string]any{{"name": "Drama"}},
					"artworks": []map[string]any{
						{"image": "https://artworks.thetvdb.com/bg.jpg", "type": 3, "score": 100},
					},
					"remoteIds": []map[string]any{
						{"id": "tt0903747", "sourceName": "IMDB"},
						{"id": "1396", "sourceName": "TheMovieDB.com"},
					},
					"translations": map[string]any{
						"nameTranslations":     []map[string]any{{"name": "绝命毒师", "language": "zho"}},
						"overviewTranslations": []map[string]any{{"overview": "中文简介", "language": "zho"}},
					},
				},
			})
		case "/series/81189/episodes/default/zho":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{
					"episodes": []map[string]any{
						{
							"id": 349232, "name": "试播集", "overview": "第一集简介",
							"aired": "2008-01-20", "runtime": 58, "image": "https://artworks.thetvdb.com/ep1.jpg",
							"seasonNumber": 1, "number": 1,
						},
					},
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
	tvdb := NewTVDB(httpClient, server.URL, "api-key", "pin-1")
	ctx := context.Background()

	results, err := tvdb.Search(ctx, "Breaking Bad")
	if err != nil || len(results) != 1 || results[0].Title != "绝命毒师" || results[0].ID != "81189" {
		t.Fatalf("Search: results=%+v err=%v", results, err)
	}

	mediaItem, err := tvdb.Detail(ctx, "tvdb:81189")
	if err != nil || mediaItem.Title != "绝命毒师" || mediaItem.Overview != "中文简介" || mediaItem.TMDBID != "1396" || mediaItem.BackdropURL == "" {
		t.Fatalf("Detail: media=%+v err=%v", mediaItem, err)
	}

	ep, err := tvdb.GetEpisode(ctx, 81189, 1, 1)
	if err != nil || ep.Title != "试播集" || ep.Runtime != 58 {
		t.Fatalf("GetEpisode: ep=%+v err=%v", ep, err)
	}

	if logins != 1 {
		t.Fatalf("expected JWT token to be cached across requests, logins=%d", logins)
	}
}

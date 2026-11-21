package plex

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPlexClientLibrariesItemsEpisodesAndRefresh(t *testing.T) {
	refreshed := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Plex-Token") != "plex-secret-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/library/sections":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"MediaContainer": map[string]any{
					"machineIdentifier": "machine-abc",
					"Directory": []map[string]any{
						{
							"key": "1", "title": "Movies", "type": "movie",
							"Location": []map[string]any{{"path": "/media/movies"}},
						},
						{
							"key": "2", "title": "TV Shows", "type": "show",
							"Location": []map[string]any{{"path": "/media/tv"}},
						},
					},
				},
			})
		case "/library/sections/1/all":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"MediaContainer": map[string]any{
					"totalSize": 1,
					"Metadata": []map[string]any{
						{
							"ratingKey": "101", "type": "movie", "title": "Dune", "originalTitle": "Dune", "year": 2021,
							"Guid":  []map[string]any{{"id": "tmdb://438631"}, {"id": "imdb://tt1160419"}},
							"Media": []map[string]any{{"Part": []map[string]any{{"file": "/media/movies/Dune (2021)/Dune.mkv"}}}},
						},
					},
				},
			})
		case "/library/metadata/200/allLeaves":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"MediaContainer": map[string]any{
					"totalSize": 1,
					"Metadata": []map[string]any{
						{
							"ratingKey": "201", "type": "episode", "title": "Pilot", "parentIndex": 1, "index": 1,
							"Media": []map[string]any{{"Part": []map[string]any{{"file": "/media/tv/Show/S01E01.mkv"}}}},
						},
					},
				},
			})
		case "/library/metadata/101":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"MediaContainer": map[string]any{
					"Metadata": []map[string]any{
						{"ratingKey": "101", "type": "movie", "title": "Dune", "year": 2021},
					},
				},
			})
		case "/library/metadata/999":
			http.NotFound(w, r)
		case "/library/sections/all/refresh":
			refreshed = true
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client, err := NewClient(Config{
		BaseURL:  server.URL,
		PlayHost: "https://plex.example.test",
		Token:    "plex-secret-token",
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer client.CloseIdleConnections()

	ctx := context.Background()
	libs, err := client.ListLibraries(ctx)
	if err != nil || len(libs) != 2 || libs[0].Title != "Movies" || libs[1].Type != "show" {
		t.Fatalf("ListLibraries: libs=%+v err=%v", libs, err)
	}

	page, err := client.ListSectionItems(ctx, "1", 0, 50)
	if err != nil || len(page.Items) != 1 || page.Items[0].ProviderIDs["Tmdb"] != "438631" || page.Items[0].Path != "/media/movies/Dune (2021)/Dune.mkv" {
		t.Fatalf("ListSectionItems: page=%+v err=%v", page, err)
	}

	eps, err := client.ListEpisodes(ctx, "200", 0, 50)
	if err != nil || len(eps.Items) != 1 || *eps.Items[0].ParentIndex != 1 || *eps.Items[0].Index != 1 {
		t.Fatalf("ListEpisodes: eps=%+v err=%v", eps, err)
	}

	item, found, err := client.GetItem(ctx, "101")
	if err != nil || !found || item.Title != "Dune" {
		t.Fatalf("GetItem 101: item=%+v found=%v err=%v", item, found, err)
	}
	_, found, err = client.GetItem(ctx, "999")
	if err != nil || found {
		t.Fatalf("GetItem 999: found=%v err=%v", found, err)
	}

	if err := client.RefreshAll(ctx); err != nil || !refreshed {
		t.Fatalf("RefreshAll: refreshed=%v err=%v", refreshed, err)
	}

	playURL, err := client.PlayURL("101")
	if err != nil || !strings.Contains(playURL, "https://plex.example.test/web/index.html#!/server/machine-abc/details?key=%2Flibrary%2Fmetadata%2F101") {
		t.Fatalf("PlayURL = %q, err = %v", playURL, err)
	}
}

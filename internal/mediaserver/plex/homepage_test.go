package plex

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPlexHomepageQueriesAndURLs(t *testing.T) {
	var seenResumeDirs, seenLatestDirs string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Plex-Token") != "tok-abc" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/library/sections":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"MediaContainer": map[string]any{
					"machineIdentifier": "mach-99",
					"Directory": []map[string]any{
						{
							"key": "1", "title": "电影", "type": "movie",
							"composite": "/library/sections/1/composite/1700000000",
							"Location":  []map[string]any{{"path": "/media/movies"}},
						},
					},
				},
			})
		case "/hubs/continueWatching/items":
			seenResumeDirs = r.URL.Query().Get("contentDirectoryID")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"MediaContainer": map[string]any{
					"Metadata": []map[string]any{
						{
							"key": "/library/metadata/101", "ratingKey": "101", "type": "movie",
							"title": "Dune", "year": 2021, "art": "/library/metadata/101/art/1",
							"viewOffset": 1800000, "duration": 3600000,
						},
						{
							"key": "/library/metadata/202", "ratingKey": "202", "type": "episode",
							"title": "Winter Is Coming", "grandparentTitle": "Game of Thrones",
							"parentIndex": 1, "index": 1, "art": "/library/metadata/200/art/2",
							"viewOffset": 900000, "duration": 3600000,
						},
					},
				},
			})
		case "/hubs/promoted":
			seenLatestDirs = r.URL.Query().Get("contentDirectoryID")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"MediaContainer": map[string]any{
					"Hub": []map[string]any{
						{
							"Metadata": []map[string]any{
								{
									"key": "/library/metadata/301", "ratingKey": "301", "type": "movie",
									"title": "Older Movie", "year": 2020, "thumb": "/library/metadata/301/thumb/1",
									"addedAt": 1700000100,
								},
							},
						},
						{
							"Metadata": []map[string]any{
								{
									"key": "/library/metadata/302", "ratingKey": "302", "type": "season",
									"parentTitle": "Shogun", "index": 1, "year": 2024,
									"thumb": "/library/metadata/302/thumb/1", "addedAt": 1700000500,
								},
							},
						},
					},
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client, err := NewClient(Config{
		BaseURL:  server.URL,
		PlayHost: "https://plex.example.test",
		Token:    "tok-abc",
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer client.CloseIdleConnections()

	libs, err := client.ListLibraries(context.Background())
	if err != nil || len(libs) != 1 || libs[0].Composite != "/library/sections/1/composite/1700000000" {
		t.Fatalf("ListLibraries: libs=%+v err=%v", libs, err)
	}

	libURL := client.LibraryURL("1")
	wantLibURL := "https://plex.example.test/web/index.html#!/media/mach-99/com.plexapp.plugins.library?source=1"
	if libURL != wantLibURL {
		t.Fatalf("LibraryURL = %q, want %q", libURL, wantLibURL)
	}

	imgURL := client.ImageURL(libs[0].Composite)
	if !strings.HasPrefix(imgURL, server.URL+"/library/sections/1/composite/1700000000?X-Plex-Token=tok-abc") {
		t.Fatalf("unexpected ImageURL: %q", imgURL)
	}

	resume, err := client.ListContinueWatching(context.Background(), []string{"1", "2"}, 12)
	if err != nil {
		t.Fatalf("ListContinueWatching: %v", err)
	}
	if seenResumeDirs != "1,2" || len(resume) != 2 || resume[0].Title != "Dune" || resume[1].GrandparentTitle != "Game of Thrones" {
		t.Fatalf("unexpected resume: dirs=%q items=%+v", seenResumeDirs, resume)
	}

	latest, err := client.ListRecentlyAdded(context.Background(), []string{"1"}, 20)
	if err != nil {
		t.Fatalf("ListRecentlyAdded: %v", err)
	}
	if seenLatestDirs != "1" || len(latest) != 2 {
		t.Fatalf("unexpected latest: dirs=%q items=%+v", seenLatestDirs, latest)
	}
	// Sorted by addedAt descending: Shogun (1700000500) before Older Movie (1700000100)
	if latest[0].ParentTitle != "Shogun" || latest[1].Title != "Older Movie" {
		t.Fatalf("expected descending addedAt order, got %+v", latest)
	}
}

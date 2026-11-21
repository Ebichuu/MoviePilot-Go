package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/moviepilot-go/moviepilot-go/internal/mediaconfig"
	"github.com/moviepilot-go/moviepilot-go/internal/mediaindex"
)

func TestMediaServerRuntimeSupportsPlex(t *testing.T) {
	refreshed := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Plex-Token") != "plex-token-123" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/library/sections":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"MediaContainer": map[string]any{
					"machineIdentifier": "plex-machine-01",
					"Directory": []map[string]any{
						{
							"key": "1", "title": "Movies", "type": "movie",
							"composite": "/library/sections/1/composite/1700000000",
							"Location":  []map[string]any{{"path": "/media/movies"}},
						},
					},
				},
			})
		case "/hubs/promoted":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"MediaContainer": map[string]any{
					"Hub": []map[string]any{
						{
							"Metadata": []map[string]any{
								{
									"key": "/library/metadata/555", "ratingKey": "555", "type": "movie",
									"title": "Inception", "year": 2010, "thumb": "/library/metadata/555/thumb/1",
									"addedAt": 1700000500,
								},
								{
									"key": "/library/metadata/600", "ratingKey": "600", "type": "episode",
									"title": "Pilot", "grandparentTitle": "Severance", "parentIndex": 1, "index": 1,
									"year": 2022, "parentThumb": "/library/metadata/599/thumb/1", "addedAt": 1700000900,
								},
							},
						},
					},
				},
			})
		case "/hubs/continueWatching/items":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"MediaContainer": map[string]any{
					"Metadata": []map[string]any{
						{
							"key": "/library/metadata/600", "ratingKey": "600", "type": "episode",
							"title": "Pilot", "grandparentTitle": "Severance", "parentIndex": 1, "index": 1,
							"art": "/library/metadata/598/art/1", "viewOffset": 1500000, "duration": 3000000,
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
							"ratingKey": "555", "type": "movie", "title": "Inception", "year": 2010,
							"Guid":  []map[string]any{{"id": "tmdb://27205"}},
							"Media": []map[string]any{{"Part": []map[string]any{{"file": "/media/movies/Inception (2010)/Inception.mkv"}}}},
						},
					},
				},
			})
		case "/library/sections/all/refresh":
			refreshed = true
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	snapshot, _, err := mediaconfig.Normalize([]mediaconfig.Server{{
		Name:    "HomePlex",
		Type:    "plex",
		Enabled: true,
		Config: mediaconfig.Config{
			Host:     server.URL,
			PlayHost: "https://plex.example.test",
			Token:    "plex-token-123",
		},
	}})
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if snapshot.RequiredID == "" {
		t.Fatal("expected enabled Plex server to be selected as RequiredID")
	}

	runtime, err := buildMediaServerRuntime(snapshot, 5*time.Second, nil)
	if err != nil {
		t.Fatalf("buildMediaServerRuntime: %v", err)
	}
	defer runtime.CloseIdleConnections()

	clients := runtime.ListMediaServerClients(context.Background())
	if len(clients) != 1 || clients[0].Name != "HomePlex" || clients[0].Type != "plex" {
		t.Fatalf("unexpected clients: %+v", clients)
	}

	libs, err := runtime.ListMediaServerLibraries(context.Background(), "HomePlex", false)
	if err != nil || len(libs) != 1 || libs[0].Name != "Movies" || libs[0].Type != "plex" {
		t.Fatalf("ListMediaServerLibraries: libs=%+v err=%v", libs, err)
	}
	if !strings.Contains(libs[0].Link, "plex-machine-01/com.plexapp.plugins.library?source=1") {
		t.Fatalf("unexpected Plex library link: %+v", libs[0])
	}
	if !strings.Contains(libs[0].Image, "/library/sections/1/composite/1700000000") {
		t.Fatalf("unexpected Plex library image: %+v", libs[0])
	}

	latest, err := runtime.ListMediaServerLatest(context.Background(), "HomePlex", 10)
	if err != nil || len(latest) != 2 {
		t.Fatalf("ListMediaServerLatest: items=%+v err=%v", latest, err)
	}
	if latest[0].Title != "Severance 第1季 第1集" || latest[0].Type != "电视剧" || latest[0].ServerType != "plex" {
		t.Fatalf("unexpected first latest item: %+v", latest[0])
	}
	if latest[1].Title != "Inception" || latest[1].Subtitle != "2010" || latest[1].Type != "电影" {
		t.Fatalf("unexpected second latest item: %+v", latest[1])
	}

	playing, err := runtime.ListMediaServerPlaying(context.Background(), "HomePlex", 10)
	if err != nil || len(playing) != 1 {
		t.Fatalf("ListMediaServerPlaying: items=%+v err=%v", playing, err)
	}
	if playing[0].Title != "Severance" || playing[0].Subtitle != "S1:E1 - Pilot" || playing[0].Percent == nil || *playing[0].Percent != 50 {
		t.Fatalf("unexpected playing item: %+v", playing[0])
	}

	playback, found, err := runtime.PlayMediaServerItem(context.Background(), "555")
	if err != nil || !found || playback.ServerType != "plex" || !strings.Contains(playback.URL, "plex-machine-01") {
		t.Fatalf("PlayMediaServerItem: playback=%+v found=%v err=%v", playback, found, err)
	}

	// Verify mediaindex Resolve + FindItems + RefreshAll work for Plex
	ref := mediaindex.ServerRef{
		ID:          snapshot.Servers[0].ID,
		Name:        snapshot.Servers[0].Name,
		Type:        "plex",
		Fingerprint: mediaServerEndpointFingerprint(snapshot.Servers[0]),
	}
	idxServer, ok := runtime.Resolve(ref)
	if !ok {
		t.Fatal("expected Resolve to succeed for Plex server")
	}
	foundItems, err := idxServer.FindItems(context.Background(), mediaindex.Query{
		Path:       "/media/movies/Inception (2010)/Inception.mkv",
		ProviderID: "tmdb.27205",
	})
	if err != nil || len(foundItems) != 1 || foundItems[0].ID != "555" {
		t.Fatalf("FindItems: items=%+v err=%v", foundItems, err)
	}
	if err := runtime.RefreshAllMediaServers(context.Background()); err != nil || !refreshed {
		t.Fatalf("RefreshAllMediaServers: refreshed=%v err=%v", refreshed, err)
	}
}

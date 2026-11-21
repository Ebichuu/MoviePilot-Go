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
							"Location": []map[string]any{{"path": "/media/movies"}},
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

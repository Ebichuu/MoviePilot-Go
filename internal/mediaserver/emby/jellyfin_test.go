package emby

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestJellyfinClientHeadersPlayURLAndAPI(t *testing.T) {
	t.Parallel()

	var gotAuth, gotToken string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotToken = r.Header.Get("X-Emby-Token")
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/System/Info":
			_, _ = w.Write([]byte(`{"Id":"jf-server-1","ServerName":"MyJellyfin","Version":"10.10.3","ProductName":"Jellyfin Server"}`))
		case "/Library/VirtualFolders":
			_, _ = w.Write([]byte(`[{"Name":"Movies","CollectionType":"movies","ItemId":"lib-1","Locations":["/media/movies"]}]`))
		case "/Library/Refresh":
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	authHeader := `MediaBrowser Token="jf-api-key", Client="MoviePilot-Go", Device="MoviePilot-Go", DeviceId="dev-1", Version="0.1"`
	client, err := NewClient(Config{
		ServerType:    "jellyfin",
		BaseURL:       server.URL,
		PlayHost:      "https://jf.example.com",
		Token:         "jf-api-key",
		Authorization: authHeader,
		Timeout:       5 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	defer client.CloseIdleConnections()

	info, err := client.SystemInfo(context.Background())
	if err != nil {
		t.Fatalf("SystemInfo() error = %v", err)
	}
	if info.ServerName != "MyJellyfin" || info.ID != "jf-server-1" {
		t.Fatalf("SystemInfo() = %+v", info)
	}
	if !strings.HasPrefix(gotAuth, `MediaBrowser Token="jf-api-key"`) || gotToken != "jf-api-key" {
		t.Fatalf("headers auth=%q token=%q", gotAuth, gotToken)
	}

	libs, err := client.ListLibraries(context.Background())
	if err != nil || len(libs) != 1 || libs[0].Name != "Movies" {
		t.Fatalf("ListLibraries() = %+v, err=%v", libs, err)
	}

	if err := client.RefreshAll(context.Background()); err != nil {
		t.Fatalf("RefreshAll() error = %v", err)
	}

	playURL, err := client.PlayURL("item-42")
	if err != nil {
		t.Fatalf("PlayURL() error = %v", err)
	}
	wantURL := "https://jf.example.com/web/index.html#!/details?id=item-42"
	if playURL != wantURL {
		t.Fatalf("PlayURL() = %q, want %q", playURL, wantURL)
	}
}

package app

import (
	"context"
	"testing"
	"time"

	"github.com/moviepilot-go/moviepilot-go/internal/mediaconfig"
)

func TestMediaServerRuntimeSupportsJellyfin(t *testing.T) {
	t.Parallel()

	snapshot, _, err := mediaconfig.Normalize([]mediaconfig.Server{{
		Name:    "HomeJellyfin",
		Type:    "jellyfin",
		Enabled: true,
		Config: mediaconfig.Config{
			Host:     "http://127.0.0.1:8096",
			PlayHost: "https://jellyfin.example.test",
			APIKey:   "jf-secret-key",
		},
	}})
	if err != nil {
		t.Fatalf("mediaconfig.Normalize() error = %v", err)
	}
	if snapshot.RequiredID == "" {
		t.Fatal("expected enabled Jellyfin server to be selected as RequiredID")
	}

	runtime, err := buildMediaServerRuntime(snapshot, 5*time.Second, nil)
	if err != nil {
		t.Fatalf("buildMediaServerRuntime() error = %v", err)
	}
	defer runtime.CloseIdleConnections()

	clients := runtime.ListMediaServerClients(context.Background())
	if len(clients) != 1 || clients[0].Name != "HomeJellyfin" || clients[0].Type != "jellyfin" {
		t.Fatalf("ListMediaServerClients() = %+v", clients)
	}

	playback, ok, err := runtime.PlayMediaServerItem(context.Background(), "abc123")
	if err != nil || !ok {
		t.Fatalf("PlayMediaServerItem() ok=%v err=%v", ok, err)
	}
	if playback.ServerType != "jellyfin" || playback.URL != "https://jellyfin.example.test/web/index.html#!/details?id=abc123" {
		t.Fatalf("PlayMediaServerItem() = %+v", playback)
	}
}

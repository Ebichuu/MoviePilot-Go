package emby

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestNewClientRejectsUnsafeConfiguration(t *testing.T) {
	tests := []struct {
		name   string
		config Config
	}{
		{name: "empty URL", config: Config{Token: "token"}},
		{name: "unsupported scheme", config: Config{BaseURL: "ftp://example.test", Token: "token"}},
		{name: "userinfo", config: Config{BaseURL: "http://user:pass@example.test", Token: "token"}},
		{name: "query", config: Config{BaseURL: "http://example.test?secret=value", Token: "token"}},
		{name: "fragment", config: Config{BaseURL: "http://example.test/#secret", Token: "token"}},
		{name: "path traversal", config: Config{BaseURL: "http://example.test/emby/../admin", Token: "token"}},
		{name: "empty token", config: Config{BaseURL: "http://example.test"}},
		{name: "token newline", config: Config{BaseURL: "http://example.test", Token: "token\r\nleak"}},
		{
			name:   "authorization newline",
			config: Config{BaseURL: "http://example.test", Token: "token", Authorization: "Bearer ok\nleak"},
		},
		{
			name:   "play host query",
			config: Config{BaseURL: "http://example.test", PlayHost: "https://play.test/base?secret=value", Token: "token"},
		},
		{name: "negative timeout", config: Config{BaseURL: "http://example.test", Token: "token", Timeout: -time.Second}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client, err := NewClient(tt.config)
			if client != nil || err == nil {
				t.Fatalf("NewClient() = (%v, %v), want error", client, err)
			}
			var embyErr *Error
			if !errors.As(err, &embyErr) || embyErr.Code != ErrorCodeInvalidConfig {
				t.Fatalf("NewClient() error = %v, want invalid_config", err)
			}
			for _, secret := range []string{"user:pass", "secret=value", "token\r\nleak", "Bearer ok"} {
				if strings.Contains(err.Error(), secret) {
					t.Fatalf("error leaked %q: %q", secret, err)
				}
			}
		})
	}
}

func TestBaseURLPathsAndAuthenticationHeaders(t *testing.T) {
	tests := []struct {
		name     string
		basePath string
		wantPath string
	}{
		{name: "root", wantPath: "/System/Info"},
		{name: "emby subpath", basePath: "/emby/", wantPath: "/emby/System/Info"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != tt.wantPath {
					t.Errorf("request = %s %s, want GET %s", r.Method, r.URL.Path, tt.wantPath)
				}
				if got := r.Header.Get("X-Emby-Token"); got != "fixture-token" {
					t.Errorf("X-Emby-Token = %q", got)
				}
				if got := r.Header.Get("Authorization"); got != "MediaBrowser Client=MoviePilot-Go" {
					t.Errorf("Authorization = %q", got)
				}
				if r.URL.Query().Has("api_key") {
					t.Errorf("request contains forbidden api_key: %s", r.URL.RawQuery)
				}
				_, _ = w.Write([]byte(`{"Id":"server-id"}`))
			}))
			t.Cleanup(server.Close)

			client := newTestClient(t, server.URL+tt.basePath)
			info, err := client.SystemInfo(t.Context())
			if err != nil || info.ID != "server-id" {
				t.Fatalf("SystemInfo() = (%#v, %v)", info, err)
			}
		})
	}
}

func TestPlayURLPreservesBasePathAndEscapesItemID(t *testing.T) {
	tests := []struct {
		name     string
		baseURL  string
		playHost string
		itemID   string
		want     string
	}{
		{
			name:     "dedicated play host",
			baseURL:  "https://api.example.test/emby",
			playHost: "https://play.example.test/base/",
			itemID:   "movie/1?x=1&y=#",
			want:     "https://play.example.test/base/web/index.html#!/item?id=movie%2F1%3Fx%3D1%26y%3D%23",
		},
		{
			name:    "base URL fallback",
			baseURL: "https://emby.example.test/emby/",
			itemID:  "episode 2",
			want:    "https://emby.example.test/emby/web/index.html#!/item?id=episode+2",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client, err := NewClient(Config{BaseURL: tt.baseURL, PlayHost: tt.playHost, Token: "token"})
			if err != nil {
				t.Fatal(err)
			}
			got, err := client.PlayURL(tt.itemID)
			if err != nil || got != tt.want {
				t.Fatalf("PlayURL() = (%q, %v), want %q", got, err, tt.want)
			}
		})
	}
}

func TestPlayURLRejectsInvalidItemID(t *testing.T) {
	client, err := NewClient(Config{BaseURL: "https://emby.example.test", Token: "token"})
	if err != nil {
		t.Fatal(err)
	}
	for _, itemID := range []string{"", " \t ", "item\nheader"} {
		if _, err := client.PlayURL(itemID); err == nil {
			t.Fatalf("PlayURL(%q) error = nil", itemID)
		}
	}
}

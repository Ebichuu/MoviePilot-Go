package indexer

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestYemaAdapterRejectsOutOfRangeIDAndMissingSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{}})
	}))
	defer server.Close()
	adapter, err := NewYemaAdapter(newIndexerHTTPClient(t, 1), 1, "Yema", server.URL, map[string]any{"apikey": "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.Search(context.Background(), Query{}); err == nil {
		t.Fatal("YemaPT response without success=true was accepted")
	}
	if _, err := adapter.ResolveDownload(context.Background(), "99999999999999999999"); err == nil {
		t.Fatal("out-of-range YemaPT torrent ID was not rejected")
	}
}

func TestSunnyPTDownloadURLAllowsOnlyConfiguredOrigins(t *testing.T) {
	adapter, err := NewSunnyPTAdapter(newIndexerHTTPClient(t, 1), 1, "SunnyPT", "https://sunnypt.example", map[string]any{
		"api_url": "https://api.sunnypt.example/api/v1/mp", "apikey": "fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{
		"https://sunnypt.example/download/1",
		"https://api.sunnypt.example/download/1",
		"/api/v1/mp/download/1",
	} {
		if _, err := adapter.resolveDownloadURL(raw); err != nil {
			t.Errorf("configured SunnyPT origin was rejected: %s: %v", raw, err)
		}
	}
	for _, raw := range []string{
		"https://attacker.example/download/1",
		"http://sunnypt.example/download/1",
		"ftp://sunnypt.example/download/1",
	} {
		if _, err := adapter.resolveDownloadURL(raw); err == nil {
			t.Errorf("untrusted SunnyPT origin was accepted: %s", raw)
		}
	}
}

func TestCategory404MapsToMovie(t *testing.T) {
	if got := hddolbyCategory(404); got != "电影" {
		t.Fatalf("HDDolby category 404 mapped to %q", got)
	}
	if got := haidanCategory("404"); got != "电影" {
		t.Fatalf("Haidan category 404 mapped to %q", got)
	}
}

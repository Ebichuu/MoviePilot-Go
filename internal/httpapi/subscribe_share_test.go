package httpapi

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/moviepilot-go/moviepilot-go/internal/database"
	"github.com/moviepilot-go/moviepilot-go/internal/media"
)

func TestSubscribeShareV2Shape(t *testing.T) {
	season := 2
	created := time.Date(2026, 11, 18, 3, 4, 5, 0, time.UTC)
	share := database.SubscribeShare{
		ID: 7, ShareKey: "abc123", Username: "admin", Title: "沙丘",
		MediaType: "电影", Year: "2021", Season: &season,
		MediaSource: "themoviedb", MediaID: "438631",
		Sites: []int{1, 2}, Filter: "4K", Description: "测试", Poster: "p.jpg",
		FollowCount: 3, CreatedAt: created,
	}
	payload := subscribeShareV2(share)
	for _, key := range []string{
		"id", "share_key", "username", "title", "media_type", "year", "season",
		"media_source", "media_id", "tmdbid", "sites", "filter", "description",
		"poster", "follow_count", "created_at",
	} {
		if _, ok := payload[key]; !ok {
			t.Fatalf("share payload missing %q", key)
		}
	}
	if payload["season"] != season {
		t.Fatalf("season = %#v, want %d", payload["season"], season)
	}
	if payload["tmdbid"] != "438631" {
		t.Fatalf("tmdbid = %#v, want 438631", payload["tmdbid"])
	}
	if sites, ok := payload["sites"].([]int); !ok || len(sites) != 2 {
		t.Fatalf("sites = %#v", payload["sites"])
	}
	if payload["created_at"] != "2026-11-18T03:04:05Z" {
		t.Fatalf("created_at = %#v", payload["created_at"])
	}
}

func TestSubscribeShareV2NilSeason(t *testing.T) {
	payload := subscribeShareV2(database.SubscribeShare{ID: 1, Title: "x"})
	if payload["season"] != nil {
		t.Fatalf("season = %#v, want nil", payload["season"])
	}
}

func TestReadShareIDFromBodyAndQuery(t *testing.T) {
	body := httptest.NewRequest("POST", "/subscribe/follow", strings.NewReader(`{"share_id": 42}`))
	if got := readShareID(body); got != 42 {
		t.Fatalf("body share_id = %d, want 42", got)
	}
	query := httptest.NewRequest("GET", "/subscribe/refresh?share_id=9", nil)
	if got := readShareID(query); got != 9 {
		t.Fatalf("query share_id = %d, want 9", got)
	}
	bad := httptest.NewRequest("POST", "/subscribe/follow", strings.NewReader(`{}`))
	if got := readShareID(bad); got != 0 {
		t.Fatalf("missing share_id = %d, want 0", got)
	}
}

func TestTMDBMediaTypeMapping(t *testing.T) {
	if got := tmdbMediaType(string(media.TypeTV)); got != "tv" {
		t.Fatalf("tv mapping = %q", got)
	}
	if got := tmdbMediaType(string(media.TypeMovie)); got != "movie" {
		t.Fatalf("movie mapping = %q", got)
	}
	if got := tmdbMediaType(""); got != "movie" {
		t.Fatalf("empty mapping = %q", got)
	}
}

func TestNewShareKeyShape(t *testing.T) {
	first, second := newShareKey(), newShareKey()
	if len(first) != 16 || len(second) != 16 {
		t.Fatalf("share key length = %d/%d, want 16", len(first), len(second))
	}
	if first == second {
		t.Fatalf("share keys重复: %q", first)
	}
	if strings.Trim(first, "0123456789abcdef") != "" {
		t.Fatalf("share key 非十六进制: %q", first)
	}
}

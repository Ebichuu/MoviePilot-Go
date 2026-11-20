package indexer

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestRSSDomainUsesNoCacheKey(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		raw  string
		want bool
	}{
		{name: "m-team host", raw: "https://m-team.io", want: true},
		{name: "m-team subdomain", raw: "https://rss.m-team.cc:443", want: true},
		{name: "other host", raw: "https://example.test", want: false},
		{name: "nil", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var site *url.URL
			if tt.raw != "" {
				var err error
				site, err = url.Parse(tt.raw)
				if err != nil {
					t.Fatal(err)
				}
			}
			if got := rssDomainUsesNoCacheKey(site); got != tt.want {
				t.Fatalf("rssDomainUsesNoCacheKey(%q) = %t, want %t", tt.raw, got, tt.want)
			}
		})
	}
}

func TestRSSItemKeyIdentityPrecedence(t *testing.T) {
	t.Parallel()

	published := time.Date(2026, time.August, 10, 12, 30, 0, 0, time.FixedZone("CST", 8*60*60))
	tests := []struct {
		name          string
		guid          string
		detailURL     string
		downloadURL   string
		title         string
		publishedAt   time.Time
		sizeBytes     int64
		wantNonEmpty  bool
		identityMatch string
	}{
		{
			name:          "GUID 优先于链接",
			guid:          " guid-1 ",
			detailURL:     "https://example.test/detail-a#first",
			downloadURL:   "https://example.test/download-a",
			title:         "First",
			publishedAt:   published,
			sizeBytes:     1,
			wantNonEmpty:  true,
			identityMatch: "guid",
		},
		{
			name:          "详情链接作为第二优先级",
			detailURL:     "HTTPS://EXAMPLE.TEST/detail-a#fragment",
			downloadURL:   "https://example.test/download-a",
			title:         "First",
			publishedAt:   published,
			sizeBytes:     1,
			wantNonEmpty:  true,
			identityMatch: "detail",
		},
		{
			name:          "下载链接作为第三优先级",
			downloadURL:   "https://example.test/download-a#fragment",
			title:         "First",
			publishedAt:   published,
			sizeBytes:     1,
			wantNonEmpty:  true,
			identityMatch: "download",
		},
		{
			name:         "标题时间大小元组兜底",
			title:        "  First   Movie ",
			publishedAt:  published,
			sizeBytes:    1024,
			wantNonEmpty: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := RSSItemKey(7, tt.guid, tt.detailURL, tt.downloadURL, tt.title, tt.publishedAt, tt.sizeBytes)
			if (got != "") != tt.wantNonEmpty {
				t.Fatalf("RSSItemKey() = %q, want non-empty=%v", got, tt.wantNonEmpty)
			}
			if tt.identityMatch == "" {
				return
			}

			var changed string
			switch tt.identityMatch {
			case "guid":
				changed = RSSItemKey(7, tt.guid, "https://example.test/other", "https://example.test/other-download", "Other", time.Time{}, 99)
			case "detail":
				changed = RSSItemKey(7, "", "https://example.test/detail-a#different", "https://example.test/other-download", "Other", time.Time{}, 99)
			case "download":
				changed = RSSItemKey(7, "", "", tt.downloadURL, "Other", time.Time{}, 99)
			}
			if got != changed {
				t.Fatalf("RSSItemKey identity precedence changed key: got=%q changed=%q", got, changed)
			}
		})
	}
}

func TestRSSItemKeyIsSiteScopedAndStable(t *testing.T) {
	t.Parallel()

	published := time.Date(2026, time.August, 10, 4, 30, 0, 0, time.UTC)
	base := RSSItemKey(11, "", "", "", "  Example   Movie ", published, 2048)
	if base == "" {
		t.Fatal("tuple identity should produce a key")
	}
	if got := RSSItemKey(11, "", "", "", "Example Movie", published.In(time.FixedZone("CST", 8*60*60)), 2048); got != base {
		t.Fatalf("equivalent title/time formatting changed key: got=%q want=%q", got, base)
	}
	if got := RSSItemKey(12, "", "", "", "Example Movie", published, 2048); got == base {
		t.Fatal("RSS item keys must be scoped to site ID")
	}
	if got := RSSItemKey(0, "guid", "", "", "", time.Time{}, 0); got != "" {
		t.Fatalf("invalid site ID produced key %q", got)
	}
}

func TestDeclarativeRSSFetchConditionalRequestAnd304(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if requests == 1 {
			if got := r.Header.Get("If-None-Match"); got != "fixture-etag" {
				t.Errorf("If-None-Match = %q, want fixture-etag", got)
			}
			if got := r.Header.Get("If-Modified-Since"); got != "fixture-date" {
				t.Errorf("If-Modified-Since = %q, want fixture-date", got)
			}
			w.Header().Set("ETag", "fixture-etag-v2")
			w.Header().Set("Last-Modified", "fixture-date-v2")
			w.Header().Set("Content-Type", "application/rss+xml")
			_, _ = w.Write([]byte(`<?xml version="1.0"?><rss version="2.0"><channel><title>Fixture</title><item><title>Example Movie</title><guid>fixture-guid</guid><link>/detail/1</link><pubDate>Mon, 10 Aug 2026 12:30:00 GMT</pubDate><enclosure url="/download/1" length="1024" type="application/x-bittorrent"/></item></channel></rss>`))
			return
		}
		if got := r.Header.Get("If-None-Match"); got != "fixture-etag-v2" {
			t.Errorf("second If-None-Match = %q, want fixture-etag-v2", got)
		}
		if got := r.Header.Get("If-Modified-Since"); got != "fixture-date-v2" {
			t.Errorf("second If-Modified-Since = %q, want fixture-date-v2", got)
		}
		w.WriteHeader(http.StatusNotModified)
	}))
	defer server.Close()

	adapter, err := NewDeclarativeAdapter(newIndexerHTTPClient(t, 1), Definition{
		ID: 21, Name: "fixture-rss-conditional", BaseURL: server.URL, SearchPath: "/search", RSSPath: "/rss", Format: "rss",
	})
	if err != nil {
		t.Fatal(err)
	}
	first, err := adapter.FetchRSS(context.Background(), RSSRequest{ETag: "fixture-etag", LastModified: "fixture-date", Limit: 10})
	if err != nil {
		t.Fatalf("first FetchRSS() error = %v", err)
	}
	if first.NotModified || len(first.Items) != 1 || first.ETag != "fixture-etag-v2" || first.LastModified != "fixture-date-v2" {
		t.Fatalf("first FetchRSS() = %#v, want parsed item and validators", first)
	}
	if first.Items[0].ItemKey == "" || first.Items[0].Resource.ItemKey != first.Items[0].ItemKey {
		t.Fatalf("RSS item key not propagated: %#v", first.Items[0])
	}

	second, err := adapter.FetchRSS(context.Background(), RSSRequest{ETag: first.ETag, LastModified: first.LastModified, Limit: 10})
	if err != nil {
		t.Fatalf("304 FetchRSS() error = %v", err)
	}
	if !second.NotModified || len(second.Items) != 0 {
		t.Fatalf("304 FetchRSS() = %#v, want NotModified with no items", second)
	}
}

func TestDeclarativeRSSFetch304WithoutBodyIsSafe(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotModified)
	}))
	defer server.Close()

	adapter, err := NewDeclarativeAdapter(newIndexerHTTPClient(t, 1), Definition{
		ID: 22, Name: "fixture-rss-empty-304", BaseURL: server.URL, RSSPath: "/rss", Format: "rss",
	})
	if err != nil {
		t.Fatal(err)
	}
	response, err := adapter.FetchRSS(context.Background(), RSSRequest{Limit: 10})
	if err != nil {
		t.Fatalf("304 without body returned error: %v", err)
	}
	if !response.NotModified || response.Items != nil {
		t.Fatalf("304 without body = %#v, want NotModified and nil items", response)
	}
}

func TestDeclarativeRSSFetchEnforcesResponseBodyLimit(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", 513)))
	}))
	defer server.Close()

	client, err := NewHTTPClient(1, time.Second, 512)
	if err != nil {
		t.Fatal(err)
	}
	client.allowPrivate = true
	adapter, err := NewDeclarativeAdapter(client, Definition{
		ID: 23, Name: "fixture-rss-limit", BaseURL: server.URL, RSSPath: "/rss", Format: "rss",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.FetchRSS(context.Background(), RSSRequest{Limit: 10}); err == nil || !strings.Contains(err.Error(), "512 字节限制") {
		t.Fatalf("oversized RSS response was accepted or wrong error: %v", err)
	}
}

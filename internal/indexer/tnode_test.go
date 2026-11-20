package indexer

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTNodeAdapterSearchCachesCSRFAndResolvesDownload(t *testing.T) {
	var csrfRequests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Cookie") != "session=fixture" {
			t.Fatalf("TNode Cookie 缺失")
		}
		switch r.URL.Path {
		case "/":
			csrfRequests++
			_, _ = w.Write([]byte(`<html><meta name="x-csrf-token" content="fixture-token"></html>`))
		case "/api/torrent/advancedSearch":
			if r.Header.Get("X-CSRF-TOKEN") != "fixture-token" {
				t.Fatalf("TNode CSRF 请求头错误")
			}
			var request map[string]any
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			if request["type"] != "imdbid" || request["keyword"] != "tt1234567" || request["page"] != float64(1) {
				t.Fatalf("TNode 搜索类型错误: %#v", request)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"torrents": []any{map[string]any{
				"id": 88, "title": "Fixture 2026", "subtitle": "description", "upload_time": 1786363200000,
				"size": 4096, "seeding": 8, "leeching": 2, "complete": 6, "downloadRate": 0, "uploadRate": 2,
			}}}})
		case "/api/torrent/download/88":
			_, _ = w.Write([]byte("d4:infod4:name5:tnodeee"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	adapter, err := NewTNodeAdapter(newIndexerHTTPClient(t, 2), 9, "TNode", server.URL, map[string]any{
		"cookie": "session=fixture", "ua": "fixture-agent",
	})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		resources, err := adapter.Search(context.Background(), Query{Keyword: "tt1234567", Page: 0, Limit: 100})
		if err != nil || len(resources) != 1 {
			t.Fatalf("TNode 搜索失败: resources=%#v err=%v", resources, err)
		}
		resource := resources[0]
		if resource.DownloadID != "88" || resource.Peers != 2 || resource.DownloadURL != "/api/v1/resource/download/9/88" {
			t.Fatalf("TNode 结果映射错误: %#v", resource)
		}
	}
	if csrfRequests != 1 {
		t.Fatalf("TNode CSRF token 未缓存: requests=%d", csrfRequests)
	}
	download, err := adapter.ResolveDownload(context.Background(), "88")
	if err != nil || string(download.Body) != "d4:infod4:name5:tnodeee" {
		t.Fatalf("TNode 下载失败: download=%#v err=%v", download, err)
	}
}

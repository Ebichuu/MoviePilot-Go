package indexer

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRousiSearchAndResolveDownload(t *testing.T) {
	var serverURL string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fixture-passkey" {
			t.Fatalf("RousiPro Bearer 鉴权缺失")
		}
		switch r.URL.Path {
		case "/api/v1/torrents":
			if r.URL.Query().Get("keyword") != "Fixture" || r.URL.Query().Get("page") != "2" {
				t.Fatalf("RousiPro 搜索参数错误: %s", r.URL.RawQuery)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{"torrents": []any{map[string]any{
				"id": 63, "uuid": "fixture-uuid", "title": "Fixture Movie 2026", "subtitle": "description",
				"created_at": "2026-08-10T12:00:00Z", "size": 4096, "seeders": 8, "leechers": 1, "downloads": 5,
				"category":  map[string]any{"slug": "movie"},
				"promotion": map[string]any{"is_active": true, "down_multiplier": 0.5, "up_multiplier": 2, "until": "2026-08-11T12:00:00Z"},
			}}}})
		case "/api/v1/torrents/63":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"download_url": serverURL + "/download/63"}})
		case "/download/63":
			_, _ = w.Write([]byte("d4:infod4:name5:rousiee"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	serverURL = server.URL

	adapter, err := NewRousiAdapter(newIndexerHTTPClient(t, 2), 14, "RousiPro", server.URL, map[string]any{
		"apikey": "fixture-passkey",
		"token":  "fixture-message-token",
	})
	if err != nil {
		t.Fatal(err)
	}
	resources, err := adapter.Search(context.Background(), Query{Keyword: "Fixture", Page: 1, Limit: 25})
	if err != nil || len(resources) != 1 {
		t.Fatalf("RousiPro 搜索失败: resources=%#v err=%v", resources, err)
	}
	resource := resources[0]
	if resource.TorrentID != "63" || resource.Category != "电影" || resource.Peers != 1 || resource.Grabs != 5 || *resource.DownloadVolumeFactor != 0.5 {
		t.Fatalf("RousiPro 字段映射错误: %#v", resource)
	}
	download, err := adapter.ResolveDownload(context.Background(), resource.DownloadID)
	if err != nil || string(download.Body) != "d4:infod4:name5:rousiee" {
		t.Fatalf("RousiPro 二阶段下载失败: download=%#v err=%v", download, err)
	}
}

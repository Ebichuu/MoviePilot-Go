package indexer

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSunnyPTSearchAndResolveDownload(t *testing.T) {
	var serverURL string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/mp/download/55" && r.Header.Get("X-API-Key") != "fixture-key" {
			t.Fatalf("SunnyPT API Key 缺失")
		}
		switch r.URL.Path {
		case "/api/v1/mp/torrents":
			if r.URL.Query().Get("keyword") != "Fixture" || r.URL.Query().Get("page") != "2" || r.URL.Query().Get("page_size") != "50" {
				t.Fatalf("SunnyPT 搜索参数错误: %s", r.URL.RawQuery)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{"items": []any{map[string]any{
				"id": 55, "title": "Fixture S01E02", "subtitle": "description", "created_at": "2026-08-10T12:00:00Z",
				"size": 4096, "seeders": 9, "leechers": 2, "completed": 6, "details_url": serverURL + "/torrent/55",
				"imdb_id": "tt1234567", "tags": []any{"官种", "中字"}, "hit_and_run": true, "media_type": "tv",
				"promotion": map[string]any{"is_active": true, "down_multiplier": 0, "up_multiplier": 2, "until": "2026-08-11T12:00:00Z"},
			}}}})
		case "/api/v1/mp/torrents/55/download-token":
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{"download_url": serverURL + "/api/v1/mp/download/55"}})
		case "/api/v1/mp/download/55":
			if r.Header.Get("X-API-Key") != "" {
				t.Fatal("SunnyPT API key leaked to torrent download request")
			}
			_, _ = w.Write([]byte("d4:infod4:name7:sunny-ptee"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	serverURL = server.URL

	adapter, err := NewSunnyPTAdapter(newIndexerHTTPClient(t, 2), 13, "SunnyPT", server.URL, map[string]any{
		"api_url": server.URL + "/api/v1/mp", "apikey": "fixture-key",
	})
	if err != nil {
		t.Fatal(err)
	}
	resources, err := adapter.Search(context.Background(), Query{Keyword: "Fixture", Page: 1, Limit: 50})
	if err != nil || len(resources) != 1 {
		t.Fatalf("SunnyPT 搜索失败: resources=%#v err=%v", resources, err)
	}
	resource := resources[0]
	if resource.TorrentID != "55" || resource.Category != "电视剧" || resource.Peers != 2 || resource.Grabs != 6 || !resource.HitAndRun || len(resource.Labels) != 2 {
		t.Fatalf("SunnyPT 字段映射错误: %#v", resource)
	}
	if *resource.DownloadVolumeFactor != 0 || *resource.UploadVolumeFactor != 2 {
		t.Fatalf("SunnyPT 促销映射错误: %#v", resource)
	}
	download, err := adapter.ResolveDownload(context.Background(), resource.DownloadID)
	if err != nil || string(download.Body) != "d4:infod4:name7:sunny-ptee" {
		t.Fatalf("SunnyPT 二阶段下载失败: download=%#v err=%v", download, err)
	}
}

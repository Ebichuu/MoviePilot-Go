package indexer

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHDDolbySearchAndResolveDownload(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "fixture-key" || r.Header.Get("Cookie") != "session=fixture" {
			t.Fatalf("HDDolby 鉴权头缺失")
		}
		switch r.URL.Path {
		case "/api/v1/torrent/search":
			var request map[string]any
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			if request["page_number"] != float64(1) || request["page_size"] != float64(30) {
				t.Fatalf("HDDolby 分页参数错误: %#v", request)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{
				"id": 99, "downhash": "fixture-hash", "name": "Fixture Movie 2026", "small_descr": "description",
				"added": "2026-08-10 12:00:00", "size": 8192, "seeders": 12, "leechers": 3, "times_completed": 8,
				"promotion_time_type": 5, "promotion_until": "2026-08-11 12:00:00", "tags": "gf;db;hdr10", "category": 401,
			}}})
		case "/download.php":
			if r.URL.Query().Get("id") != "99" || r.URL.Query().Get("downhash") != "fixture-hash" {
				t.Fatalf("HDDolby 下载参数错误: %s", r.URL.RawQuery)
			}
			_, _ = w.Write([]byte("d4:infod4:name7:hddolbyee"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	adapter, err := NewHDDolbyAdapter(newIndexerHTTPClient(t, 2), 16, "HDDolby", server.URL, map[string]any{
		"api_url": server.URL, "apikey": "fixture-key", "cookie": "session=fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	resources, err := adapter.Search(context.Background(), Query{Keyword: "Fixture", Page: 1, Limit: 30})
	if err != nil || len(resources) != 1 {
		t.Fatalf("HDDolby 搜索失败: resources=%#v err=%v", resources, err)
	}
	resource := resources[0]
	if resource.TorrentID != "99" || resource.TorrentID == resource.DownloadID || resource.Category != "电影" ||
		len(resource.Labels) != 3 || resource.Peers != 3 || *resource.DownloadVolumeFactor != 0.5 {
		t.Fatalf("HDDolby 字段映射错误: %#v", resource)
	}
	download, err := adapter.ResolveDownload(context.Background(), resource.DownloadID)
	if err != nil || string(download.Body) != "d4:infod4:name7:hddolbyee" {
		t.Fatalf("HDDolby 下载失败: download=%#v err=%v", download, err)
	}
}

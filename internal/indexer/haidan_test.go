package indexer

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHaidanObjectSearchAndResolveDownload(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Cookie") != "session=fixture" {
			t.Fatalf("Haidan Cookie 缺失")
		}
		switch r.URL.Path {
		case "/torrents.php":
			if r.URL.Query().Get("search_area") != "4" || r.URL.Query().Get("search") != "tt1234567" {
				t.Fatalf("Haidan 搜索参数错误: %s", r.URL.RawQuery)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{"77": map[string]any{
				"name": "Fixture Movie 2026", "small_descr": "description", "url": "/download.php?id=77", "group_id": 7,
				"added": 1786363200, "size": 4096, "seeders": 6, "leechers": 2, "times_completed": 5,
				"sp_state": "4", "category": "401",
			}}})
		case "/download.php":
			_, _ = w.Write([]byte("d4:infod4:name6:haidanee"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	adapter, err := NewHaidanAdapter(newIndexerHTTPClient(t, 2), 15, "Haidan", server.URL, map[string]any{"cookie": "session=fixture"})
	if err != nil {
		t.Fatal(err)
	}
	resources, err := adapter.Search(context.Background(), Query{Keyword: "tt1234567", Limit: 20})
	if err != nil || len(resources) != 1 {
		t.Fatalf("Haidan 搜索失败: resources=%#v err=%v", resources, err)
	}
	resource := resources[0]
	if resource.TorrentID != "77" || resource.TorrentID == resource.DownloadID || resource.Category != "电影" ||
		resource.Peers != 2 || resource.Grabs != 5 || *resource.DownloadVolumeFactor != 0 || *resource.UploadVolumeFactor != 2 {
		t.Fatalf("Haidan 字段映射错误: %#v", resource)
	}
	download, err := adapter.ResolveDownload(context.Background(), resource.DownloadID)
	if err != nil || string(download.Body) != "d4:infod4:name6:haidanee" {
		t.Fatalf("Haidan 下载失败: download=%#v err=%v", download, err)
	}
}

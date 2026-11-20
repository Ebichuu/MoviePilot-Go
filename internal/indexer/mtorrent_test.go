package indexer

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestMTorrentSearchAndResolveDownload(t *testing.T) {
	var serverURL string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "fixture-key" {
			t.Fatalf("mTorrent API Key 缺失")
		}
		switch r.URL.Path {
		case "/api/torrent/search":
			var request map[string]any
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			if request["keyword"] != "https://www.imdb.com/title/tt1234567" || request["pageNumber"] != float64(2) {
				t.Fatalf("mTorrent 搜索参数错误: %#v", request)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"data": []any{map[string]any{
				"id": 42, "name": "Fixture S01E01", "smallDescr": "description", "category": "403",
				"createdDate": 1786363200000, "size": "8192", "imdb": "https://imdb.com/title/tt1234567/",
				"labelsNew": []any{"DIY", "中字"}, "status": map[string]any{
					"seeders": 10, "leechers": 2, "timesCompleted": 7, "discount": "_2X_FREE", "discountEndTime": "2026-08-11 12:00:00",
				},
			}}}})
		case "/api/torrent/genDlToken":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": serverURL + "/download/42"})
		case "/download/42":
			_, _ = w.Write([]byte("d4:infod4:name5:mteamee"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	serverURL = server.URL

	adapter, err := NewMTorrentAdapter(newIndexerHTTPClient(t, 2), 12, "M-Team", server.URL, map[string]any{
		"api_url": server.URL, "apikey": "fixture-key",
	})
	if err != nil {
		t.Fatal(err)
	}
	resources, err := adapter.Search(context.Background(), Query{Keyword: "tt1234567", Page: 1, Limit: 50})
	if err != nil || len(resources) != 1 {
		t.Fatalf("mTorrent 搜索失败: resources=%#v err=%v", resources, err)
	}
	resource := resources[0]
	if resource.TorrentID != "42" || resource.Category != "电视剧" || resource.IMDBID != "tt1234567" || len(resource.Labels) != 2 || resource.Peers != 2 {
		t.Fatalf("mTorrent 字段映射错误: %#v", resource)
	}
	if *resource.DownloadVolumeFactor != 0 || *resource.UploadVolumeFactor != 2 {
		t.Fatalf("mTorrent 促销映射错误: %#v", resource)
	}
	download, err := adapter.ResolveDownload(context.Background(), resource.DownloadID)
	if err != nil || string(download.Body) != "d4:infod4:name5:mteamee" {
		t.Fatalf("mTorrent 二阶段下载失败: download=%#v err=%v", download, err)
	}
}

package indexer

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestYemaAdapterSearchAndResolveDownload(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "fixture-auth-key" {
			t.Fatalf("YemaPT 鉴权头缺失")
		}
		switch r.URL.Path {
		case "/openApi/torrent/fetchOpenTorrentList.json":
			var request map[string]any
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			page := request["pageParam"].(map[string]any)
			if request["keyword"] != "Example" || page["current"] != float64(2) || page["pageSize"] != float64(40) {
				t.Fatalf("YemaPT 请求参数错误: %#v", request)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": []any{map[string]any{
				"id": 42, "showName": "Example S01E01", "shortDesc": "fixture", "categoryId": 5,
				"tagList": []any{"6", "10"}, "listingTime": "2026-08-10 12:00:00", "fileSize": 2048,
				"seedNum": 12, "leechNum": 3, "completedNum": 9, "downloadPromotion": "free",
				"uploadPromotion": "double_upload", "hrPunishEnable": true,
			}}})
		case "/openApi/torrent/generateDownloadKey.json":
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": "fixture-token"})
		case "/api/torrent/download1":
			if r.URL.Query().Get("token") != "fixture-token" {
				t.Fatalf("YemaPT 下载 token 错误")
			}
			_, _ = w.Write([]byte("d4:infod4:name7:fixtureee"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := newIndexerHTTPClient(t, 2)
	adapter, err := NewYemaAdapter(client, 7, "YemaPT", server.URL, map[string]any{"apikey": "fixture-auth-key"})
	if err != nil {
		t.Fatal(err)
	}
	resources, err := adapter.Search(context.Background(), Query{Keyword: "Example", Page: 1, Limit: 100})
	if err != nil || len(resources) != 1 {
		t.Fatalf("YemaPT 搜索失败: resources=%#v err=%v", resources, err)
	}
	resource := resources[0]
	if resource.SiteID != 7 || resource.TorrentID != "42" || resource.DownloadID != "42" || resource.Peers != 3 || resource.Grabs != 9 {
		t.Fatalf("YemaPT 基础字段映射错误: %#v", resource)
	}
	if resource.Category != "电视剧" || len(resource.Labels) != 2 || !resource.HitAndRun || *resource.DownloadVolumeFactor != 0 || *resource.UploadVolumeFactor != 2 {
		t.Fatalf("YemaPT 分类、标签或促销映射错误: %#v", resource)
	}
	download, err := adapter.ResolveDownload(context.Background(), resource.DownloadID)
	if err != nil || string(download.Body) != "d4:infod4:name7:fixtureee" || download.Filename != "42.torrent" {
		t.Fatalf("YemaPT 二阶段下载失败: download=%#v err=%v", download, err)
	}
}

func TestYemaAdapterRejectsMissingCredentialsAndInvalidDownloadID(t *testing.T) {
	client := newIndexerHTTPClient(t, 1)
	if _, err := NewYemaAdapter(client, 1, "Yema", "https://example.test", nil); err == nil {
		t.Fatal("缺少 YemaPT API Key 时应拒绝装配")
	}
	adapter, err := NewYemaAdapter(client, 1, "Yema", "https://example.test", map[string]any{"apikey": "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.ResolveDownload(context.Background(), "../../secret"); err == nil {
		t.Fatal("非法 YemaPT 种子 ID 未被拒绝")
	}
}

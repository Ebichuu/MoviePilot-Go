package indexer

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTorrentLeechSearchBranchesAndResolveDownload(t *testing.T) {
	var requestedPaths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Cookie") != "session=fixture" {
			t.Fatalf("TorrentLeech Cookie 缺失")
		}
		requestedPaths = append(requestedPaths, r.URL.EscapedPath())
		if strings.HasPrefix(r.URL.Path, "/download/") {
			_, _ = w.Write([]byte("d4:infod4:name2:tlee"))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"torrentList": []any{map[string]any{
			"name": "Fixture Movie 2026", "fid": 71, "filename": "fixture movie.torrent", "addedTimestamp": 1786363200,
			"size": 8192, "seeders": 15, "leechers": 4, "completed": 10, "download_multiplier": 0.5, "imdbID": "tt7654321",
		}}})
	}))
	defer server.Close()

	adapter, err := NewTorrentLeechAdapter(newIndexerHTTPClient(t, 2), 11, "TorrentLeech", server.URL, map[string]any{"cookie": "session=fixture"})
	if err != nil {
		t.Fatal(err)
	}
	resources, err := adapter.Search(context.Background(), Query{Keyword: "Fixture Movie", Page: 2, Limit: 20})
	if err != nil || len(resources) != 1 {
		t.Fatalf("TorrentLeech 关键词搜索失败: resources=%#v err=%v", resources, err)
	}
	resource := resources[0]
	if resource.TorrentID != "71" || resource.TorrentID == resource.DownloadID || resource.Peers != 4 ||
		resource.Grabs != 10 || resource.IMDBID != "tt7654321" || *resource.DownloadVolumeFactor != 0.5 {
		t.Fatalf("TorrentLeech 字段映射错误: %#v", resource)
	}
	if _, err := adapter.Search(context.Background(), Query{Page: 2, Limit: 20}); err != nil {
		t.Fatal(err)
	}
	if len(requestedPaths) < 2 || requestedPaths[0] != "/torrents/browse/list/query/Fixture%20Movie" || requestedPaths[1] != "/torrents/browse/list/page/3" {
		t.Fatalf("TorrentLeech 搜索 URL 分支错误: %#v", requestedPaths)
	}
	download, err := adapter.ResolveDownload(context.Background(), resource.DownloadID)
	if err != nil || string(download.Body) != "d4:infod4:name2:tlee" || download.Filename != "fixture movie.torrent" {
		t.Fatalf("TorrentLeech 下载失败: download=%#v err=%v", download, err)
	}
	if !strings.HasSuffix(requestedPaths[len(requestedPaths)-1], "/download/71/fixture%20movie.torrent") {
		t.Fatalf("TorrentLeech 下载 URL 错误: %#v", requestedPaths)
	}
}

func TestTorrentLeechReturnsEmptyForChineseKeyword(t *testing.T) {
	adapter, err := NewTorrentLeechAdapter(newIndexerHTTPClient(t, 1), 1, "TorrentLeech", "https://example.test", map[string]any{"cookie": "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	resources, err := adapter.Search(context.Background(), Query{Keyword: "中文"})
	if err != nil || len(resources) != 0 {
		t.Fatalf("TorrentLeech 中文关键词应返回空结果: resources=%#v err=%v", resources, err)
	}
}

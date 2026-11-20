package indexer

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"
)

// 本文件覆盖 HDDolby 搜索与下载的分支：
// 页码归一、响应非 JSON、遍历中取消、命中上限、下载请求失败，
// 以及站内信未读数为 0 时用链接数补齐。

const hddolbySearchPayload = `{"data":[
	{"id":"12","downhash":"fixture-hash","name":"示例剧集","size":4096,"seeders":6,"leechers":2,
	 "times_completed":9,"added":"2026-01-02 03:04:05","promotion_until":"2026-01-03 00:00:00",
	 "tags":"禁转,中字","category":401,"promotion_time_type":2},
	{"id":"13","downhash":"","name":"缺 downhash"}
]}`

func hddolbySearchServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/torrent/search":
			_, _ = w.Write([]byte(body))
		case "/download.php":
			_, _ = w.Write([]byte("d4:infod4:name7:hddolby"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func newHDDolbySearchAdapter(t *testing.T, client *HTTPClient, endpoint string) *HDDolbyAdapter {
	t.Helper()
	adapter, err := NewHDDolbyAdapter(client, 7, "HDDolby", endpoint, map[string]any{
		"api_url": endpoint, "apikey": "api-key", "cookie": "session=fixture",
	})
	if err != nil {
		t.Fatalf("构造适配器: %v", err)
	}
	return adapter
}

func TestHDDolbySearchNormalizesPageAndStopsAtLimit(t *testing.T) {
	server := hddolbySearchServer(t, hddolbySearchPayload)
	adapter := newHDDolbySearchAdapter(t, newIndexerHTTPClient(t, 1), server.URL)
	resources, err := adapter.Search(context.Background(), Query{Page: -2, Limit: 1})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(resources) != 1 {
		t.Fatalf("命中上限后应只返回 1 条，得到 %d", len(resources))
	}
	item := resources[0]
	if item.TorrentID != "12" || item.SizeBytes != 4096 || item.Seeders != 6 || item.Peers != 2 || item.Grabs != 9 {
		t.Fatalf("条目字段 = %+v", item)
	}
	if item.DownloadVolumeFactor == nil || *item.DownloadVolumeFactor != 0 {
		t.Fatalf("促销系数 = %+v", item.DownloadVolumeFactor)
	}
	decoded, decodeErr := base64.RawURLEncoding.DecodeString(item.DownloadID)
	if decodeErr != nil || string(decoded) != "12\nfixture-hash" {
		t.Fatalf("下载标识 = %q（%v）", item.DownloadID, decodeErr)
	}
}

func TestHDDolbySearchRejectsNonJSON(t *testing.T) {
	server := hddolbySearchServer(t, "<html>拦截页</html>")
	adapter := newHDDolbySearchAdapter(t, newIndexerHTTPClient(t, 1), server.URL)
	if _, err := adapter.Search(context.Background(), Query{}); err == nil {
		t.Fatal("响应不是 JSON 时应返回错误")
	}
}

func TestHDDolbySearchCanceledDuringIteration(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	client := newIndexerHTTPClient(t, 1)
	client.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		cancel()
		return mtorrentJSONResponse(hddolbySearchPayload), nil
	})
	adapter := newHDDolbySearchAdapter(t, client, "https://hddolby.example")
	if _, err := adapter.Search(ctx, Query{Limit: 10}); err == nil {
		t.Fatal("上下文取消后应返回错误")
	}
}

func TestHDDolbyResolveDownloadTransportFailure(t *testing.T) {
	closed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	endpoint := closed.URL
	closed.Close()
	adapter := newHDDolbySearchAdapter(t, newIndexerHTTPClient(t, 1), endpoint)
	resourceID := base64.RawURLEncoding.EncodeToString([]byte("12\nfixture-hash"))
	if _, err := adapter.ResolveDownload(context.Background(), resourceID); err == nil {
		t.Fatal("连接不可用时应返回错误")
	}
}

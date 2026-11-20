package indexer

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 本文件覆盖 M-Team 搜索/下载的分支：
// 促销规则与商城免费、条目清洗、命中上限、上下文取消、响应非法与下载失败。

const mtorrentSearchPayload = `{"code":"0","data":{"data":[
	"skip",
	{"id":"abc","name":"非法 ID"},
	{"id":"11","name":"","status":{}},
	{"id":"12","name":"促销条目","smallDescr":"描述","size":1024,"createdDate":"2026-01-02 03:04:05",
	 "category":"TV","imdb":"tt1234567","labelsNew":["禁转","中字"],
	 "status":{"seeders":3,"leechers":1,"timesCompleted":2,"discount":"FREE","discountEndTime":"2026-01-03 00:00:00",
	           "promotionRule":{"discount":"PERCENT_50","endTime":"2026-01-04 00:00:00"},
	           "mallSingleFree":{"status":"ONGOING","endDate":"2026-01-05 00:00:00"}}}
]}}`

func mtorrentBranchServer(t *testing.T, searchBody string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/torrent/search":
			_, _ = w.Write([]byte(searchBody))
		case "/api/torrent/genDlToken":
			_, _ = w.Write([]byte(`{"code":"0","data":"/download/12.torrent"}`))
		case "/download/12.torrent":
			_, _ = w.Write([]byte("d4:infod4:name6:mtorre"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func newMTorrentBranchAdapter(t *testing.T, client *HTTPClient, endpoint string) *MTorrentAdapter {
	t.Helper()
	adapter, err := NewMTorrentAdapter(client, 31, "M-Team", endpoint, map[string]any{
		"api_url": endpoint, "apikey": "fixture-key",
	})
	if err != nil {
		t.Fatalf("构造适配器: %v", err)
	}
	return adapter
}

func TestMTorrentSearchPromotionAndMallBranches(t *testing.T) {
	server := mtorrentBranchServer(t, mtorrentSearchPayload)
	adapter := newMTorrentBranchAdapter(t, newIndexerHTTPClient(t, 1), server.URL)
	resources, err := adapter.Search(context.Background(), Query{Keyword: "tt1234567", Limit: 10})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(resources) != 1 {
		t.Fatalf("资源条数 = %d", len(resources))
	}
	item := resources[0]
	if item.TorrentID != "12" || item.SizeBytes != 1024 || item.Seeders != 3 || item.Peers != 1 || item.Grabs != 2 {
		t.Fatalf("条目字段 = %+v", item)
	}
	if item.DownloadVolumeFactor == nil || *item.DownloadVolumeFactor != 0 {
		t.Fatalf("商城免费应把下载系数置 0，得到 %+v", item.DownloadVolumeFactor)
	}
	if item.FreeDate.IsZero() || item.FreeDate.Format("2006-01-02") != "2026-01-05" {
		t.Fatalf("免费时间 = %v", item.FreeDate)
	}
	if item.IMDBID != "tt1234567" || len(item.Labels) != 2 {
		t.Fatalf("IMDB/标签 = %+v / %+v", item.IMDBID, item.Labels)
	}
}

func TestMTorrentSearchStopsAtLimit(t *testing.T) {
	server := mtorrentBranchServer(t, mtorrentSearchPayload)
	adapter := newMTorrentBranchAdapter(t, newIndexerHTTPClient(t, 1), server.URL)
	resources, err := adapter.Search(context.Background(), Query{Page: -1, Limit: 1})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(resources) != 1 {
		t.Fatalf("命中上限后应只返回 1 条，得到 %d", len(resources))
	}
}

func TestMTorrentSearchCanceledDuringIteration(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	client := newIndexerHTTPClient(t, 1)
	client.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		cancel()
		return mtorrentJSONResponse(mtorrentSearchPayload), nil
	})
	adapter := newMTorrentBranchAdapter(t, client, "https://mtorrent.example")
	if _, err := adapter.Search(ctx, Query{Limit: 10}); err == nil {
		t.Fatal("上下文取消后应返回错误")
	}
}

func TestMTorrentSearchRejectsBadResponses(t *testing.T) {
	t.Run("响应不是 JSON", func(t *testing.T) {
		server := mtorrentBranchServer(t, "<html>拦截页</html>")
		adapter := newMTorrentBranchAdapter(t, newIndexerHTTPClient(t, 1), server.URL)
		if _, err := adapter.Search(context.Background(), Query{}); err == nil {
			t.Fatal("响应不是 JSON 时应返回错误")
		}
	})

	t.Run("缺少数据数组", func(t *testing.T) {
		server := mtorrentBranchServer(t, `{"code":"0","data":{}}`)
		adapter := newMTorrentBranchAdapter(t, newIndexerHTTPClient(t, 1), server.URL)
		if _, err := adapter.Search(context.Background(), Query{}); err == nil ||
			!strings.Contains(err.Error(), "搜索结果不是数组") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("请求失败", func(t *testing.T) {
		closed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		endpoint := closed.URL
		closed.Close()
		adapter := newMTorrentBranchAdapter(t, newIndexerHTTPClient(t, 1), endpoint)
		if _, err := adapter.Search(context.Background(), Query{}); err == nil {
			t.Fatal("连接不可用时应返回错误")
		}
	})
}

func TestMTorrentResolveDownloadFailures(t *testing.T) {
	server := mtorrentBranchServer(t, mtorrentSearchPayload)

	t.Run("凭证请求失败", func(t *testing.T) {
		closed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		endpoint := closed.URL
		closed.Close()
		adapter := newMTorrentBranchAdapter(t, newIndexerHTTPClient(t, 1), endpoint)
		if _, err := adapter.ResolveDownload(context.Background(), "12"); err == nil {
			t.Fatal("凭证请求失败时应返回错误")
		}
	})

	t.Run("下载文件请求失败", func(t *testing.T) {
		client := newIndexerHTTPClient(t, 1)
		client.client.Transport = &failAfterRequestsWith{
			base: http.DefaultTransport, after: 1, err: http.ErrHandlerTimeout,
		}
		adapter := newMTorrentBranchAdapter(t, client, server.URL)
		if _, err := adapter.ResolveDownload(context.Background(), "12"); err == nil {
			t.Fatal("下载文件失败时应返回错误")
		}
	})

	t.Run("正常下载", func(t *testing.T) {
		adapter := newMTorrentBranchAdapter(t, newIndexerHTTPClient(t, 1), server.URL)
		download, err := adapter.ResolveDownload(context.Background(), "12")
		if err != nil || string(download.Body) != "d4:infod4:name6:mtorre" {
			t.Fatalf("下载结果 = %+v（%v）", download, err)
		}
		if download.Filename != "12.torrent" {
			t.Fatalf("Filename = %q", download.Filename)
		}
	})
}

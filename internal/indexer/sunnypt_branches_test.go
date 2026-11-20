package indexer

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

// TestNewSunnyPTAdapterValidation 覆盖 SunnyPT 构造期的配置校验。
func TestNewSunnyPTAdapterValidation(t *testing.T) {
	client := newIndexerHTTPClient(t, 1)
	if _, err := NewSunnyPTAdapter(nil, 1, "x", "https://index.example.com", nil); err == nil ||
		!strings.Contains(err.Error(), "站点 HTTP 客户端不能为空") {
		t.Fatalf("空客户端 = %v", err)
	}
	if _, err := NewSunnyPTAdapter(client, 1, "x", "ftp://index.example.com", nil); err == nil {
		t.Fatal("非法站点地址应报错")
	}
	if _, err := NewSunnyPTAdapter(client, 1, "x", "https://index.example.com", map[string]any{
		"api_url": "ftp://api.example.com", "apikey": "k",
	}); err == nil || !strings.Contains(err.Error(), "SunnyPT API 地址无效") {
		t.Fatalf("非法 API 地址 = %v", err)
	}
	if _, err := NewSunnyPTAdapter(client, 1, "x", "https://index.example.com", nil); err == nil ||
		!strings.Contains(err.Error(), "未配置 API Key") {
		t.Fatalf("缺 Key = %v", err)
	}
	if _, err := NewSunnyPTAdapter(client, 1, "x", "https://index.example.com", map[string]any{
		"apikey": strings.Repeat("k", 4097),
	}); err == nil || !strings.Contains(err.Error(), "API Key 无效") {
		t.Fatalf("超长 Key = %v", err)
	}
	if _, err := NewSunnyPTAdapter(client, 1, "x", "https://index.example.com", map[string]any{
		"apikey": "k\r\nX-Injected: 1",
	}); err == nil || !strings.Contains(err.Error(), "API Key 无效") {
		t.Fatalf("换行 Key = %v", err)
	}
	if _, err := NewSunnyPTAdapter(client, 1, "x", "https://index.example.com", map[string]any{
		"apikey": "k", "ua": strings.Repeat("u", 1025),
	}); err == nil || !strings.Contains(err.Error(), "User-Agent 无效") {
		t.Fatalf("超长 UA = %v", err)
	}
	if _, err := NewSunnyPTAdapter(client, 1, "x", "https://index.example.com", map[string]any{
		"apikey": "k", "ua": "bad\r\nua",
	}); err == nil || !strings.Contains(err.Error(), "User-Agent 无效") {
		t.Fatalf("换行 UA = %v", err)
	}

	// 缺省 API 地址走官方域名，名字回落默认值。
	adapter, err := NewSunnyPTAdapter(client, 11, "  ", "https://index.example.com", map[string]any{
		"apikey": "key",
	})
	if err != nil {
		t.Fatalf("合法配置: %v", err)
	}
	if adapter.name != "SunnyPT" || adapter.api.Host != "api.sunnypt.top" {
		t.Fatalf("默认配置 = name:%q api:%v", adapter.name, adapter.api)
	}
	if adapter.headers.Get("X-API-Key") != "key" {
		t.Fatalf("API Key 请求头 = %q", adapter.headers.Get("X-API-Key"))
	}
	if adapter.Name() != "SunnyPT" || adapter.SiteID() != 11 || adapter.ResolverKind() != "sunnypt" {
		t.Fatal("适配器元信息错误")
	}
}

// TestSunnyPTSearchBranches 覆盖查询参数、失败码与条目清洗。
func TestSunnyPTSearchBranches(t *testing.T) {
	var query string
	endpoint := probeServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "torrents/42/download-token") {
			_, _ = w.Write([]byte(`{"code":0,"data":{"download_url":"/download/42"}}`))
			return
		}
		query = r.URL.RawQuery
		_, _ = w.Write([]byte(`{"code":0,"data":{"items":[
			"skip",
			{"id":"","title":"缺 ID"},
			{"id":"1","title":""},
			{"id":"2","title":"保留","size":512,"seeders":2,"leechers":1,"completed":4,
				"promotion":{"is_active":true,"down_multiplier":0.5,"up_multiplier":2,"until":"2026-12-31 00:00:00"},
				"details_url":"/torrents/2","media_type":"TV","hit_and_run":true,
				"tags":["中字", 3],"imdb_id":"tt99","created_at":"2026-05-06 07:08:09","subtitle":"副标题"},
			{"id":"3","title":"无详情","media_type":"movie","details_url":"https://evil.example.com/x"}
		]}}`))
	})
	adapter, err := NewSunnyPTAdapter(newIndexerHTTPClient(t, 1), 11, "SunnyPT", endpoint, map[string]any{
		"api_url": endpoint, "apikey": "k",
	})
	if err != nil {
		t.Fatalf("构造适配器: %v", err)
	}
	resources, err := adapter.Search(context.Background(), Query{Keyword: "某剧", Page: 1, Limit: 5})
	if err != nil {
		t.Fatalf("搜索失败: %v", err)
	}
	for _, fragment := range []string{"page=2", "page_size=5", "sort=created_at", "order=desc", "keyword="} {
		if !strings.Contains(query, fragment) {
			t.Fatalf("查询串缺少 %q: %s", fragment, query)
		}
	}
	if len(resources) != 2 {
		t.Fatalf("资源条数 = %d", len(resources))
	}
	first := resources[0]
	if first.Title != "保留" || first.SizeBytes != 512 || first.Seeders != 2 || first.Peers != 1 ||
		first.Grabs != 4 || first.IMDBID != "tt99" || !first.HitAndRun || first.Description != "副标题" {
		t.Fatalf("首条字段 = %+v", first)
	}
	if first.Category != "电视剧" || len(first.Labels) != 2 || first.Labels[1] != "3" {
		t.Fatalf("分类/标签 = %+v", first)
	}
	if first.DownloadVolumeFactor == nil || *first.DownloadVolumeFactor != 0.5 ||
		first.UploadVolumeFactor == nil || *first.UploadVolumeFactor != 2 {
		t.Fatalf("促销系数 = %+v", first)
	}
	if !strings.Contains(first.DetailURL, "/torrents/2") || first.FreeDate.IsZero() {
		t.Fatalf("详情与免费时间 = %+v", first)
	}
	// 跨站 details_url 被丢弃，链接为空。
	if resources[1].DetailURL != "" || resources[1].Category != "电影" {
		t.Fatalf("次条字段 = %+v", resources[1])
	}

	// 空关键字不进查询串。
	_, _ = adapter.Search(context.Background(), Query{Keyword: "  "})
	if strings.Contains(query, "keyword") {
		t.Fatalf("空关键字不应进查询串: %s", query)
	}

	for _, body := range []string{`{"code":7}`, `{"code":0,"data":{}}`} {
		brokenEndpoint := probeServer(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(body))
		})
		broken, err := NewSunnyPTAdapter(newIndexerHTTPClient(t, 1), 11, "SunnyPT", brokenEndpoint, map[string]any{
			"api_url": brokenEndpoint, "apikey": "k",
		})
		if err != nil {
			t.Fatalf("构造适配器: %v", err)
		}
		if _, searchErr := broken.Search(context.Background(), Query{Keyword: "某剧"}); searchErr == nil {
			t.Fatalf("%s 应报错", body)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := adapter.Search(ctx, Query{Keyword: "某剧"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("取消上下文 = %v", err)
	}
}

// TestSunnyPTResolveDownloadBranches 覆盖下载 token 与下载地址可信性。
func TestSunnyPTResolveDownloadBranches(t *testing.T) {
	var downloadHeaders http.Header
	endpoint := probeServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			body, _ := io.ReadAll(r.Body)
			if string(body) != "{}" {
				t.Fatalf("token 请求体 = %s", body)
			}
			_, _ = w.Write([]byte(`{"code":0,"data":{"download_url":"/download/42"}}`))
			return
		}
		downloadHeaders = r.Header.Clone()
		_, _ = w.Write([]byte("d4:infod4:name5:sunnyee"))
	})
	adapter, err := NewSunnyPTAdapter(newIndexerHTTPClient(t, 1), 11, "SunnyPT", endpoint, map[string]any{
		"api_url": endpoint, "apikey": "k",
	})
	if err != nil {
		t.Fatalf("构造适配器: %v", err)
	}
	if _, err := adapter.ResolveDownload(context.Background(), "abc"); err == nil ||
		!strings.Contains(err.Error(), "种子 ID 无效") {
		t.Fatalf("非法 ID = %v", err)
	}
	download, err := adapter.ResolveDownload(context.Background(), "42")
	if err != nil || string(download.Body) != "d4:infod4:name5:sunnyee" ||
		download.Filename != "42.torrent" || download.ContentType != "application/x-bittorrent" {
		t.Fatalf("下载结果 = %+v（%v）", download, err)
	}
	if downloadHeaders.Get("X-API-Key") != "" {
		t.Fatalf("下载请求不应带 API Key: %+v", downloadHeaders)
	}

	for _, body := range []string{`{"code":9}`, `{"code":0,"data":{}}`, `not json`} {
		brokenEndpoint := probeServer(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(body))
		})
		broken, err := NewSunnyPTAdapter(newIndexerHTTPClient(t, 1), 11, "SunnyPT", brokenEndpoint, map[string]any{
			"api_url": brokenEndpoint, "apikey": "k",
		})
		if err != nil {
			t.Fatalf("构造适配器: %v", err)
		}
		if _, downloadErr := broken.ResolveDownload(context.Background(), "42"); downloadErr == nil {
			t.Fatalf("%s 应报错", body)
		}
	}

	untrusted := probeServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"data":{"download_url":"https://evil.example.com/x.torrent"}}`))
	})
	broken, err := NewSunnyPTAdapter(newIndexerHTTPClient(t, 1), 11, "SunnyPT", untrusted, map[string]any{
		"api_url": untrusted, "apikey": "k",
	})
	if err != nil {
		t.Fatalf("构造适配器: %v", err)
	}
	if _, err := broken.ResolveDownload(context.Background(), "42"); err == nil ||
		!strings.Contains(err.Error(), "下载地址不受信任") {
		t.Fatalf("跨站下载地址 = %v", err)
	}
}

// TestSunnyPTResolveDownloadURLBranches 覆盖下载地址的可信来源判定。
func TestSunnyPTResolveDownloadURLBranches(t *testing.T) {
	endpoint := probeServer(t, func(w http.ResponseWriter, r *http.Request) {})
	adapter, err := NewSunnyPTAdapter(newIndexerHTTPClient(t, 1), 11, "SunnyPT", endpoint, map[string]any{
		"api_url": endpoint, "apikey": "k",
	})
	if err != nil {
		t.Fatalf("构造适配器: %v", err)
	}
	if _, err := adapter.resolveDownloadURL("ftp://index.example.com/x"); err == nil {
		t.Fatal("非 HTTP 协议应被拒绝")
	}
	if _, err := adapter.resolveDownloadURL("https://user:pass@index.example.com/x"); err == nil {
		t.Fatal("内嵌用户信息应被拒绝")
	}
	if _, err := adapter.resolveDownloadURL("https://evil.example.com/x"); err == nil {
		t.Fatal("其它来源应被拒绝")
	}
	resolved, err := adapter.resolveDownloadURL("/download/7")
	if err != nil || !strings.HasSuffix(resolved, "/download/7") {
		t.Fatalf("相对下载地址 = %q（%v）", resolved, err)
	}
}

// TestSunnyPTCategoryBranches 覆盖分类映射。
func TestSunnyPTCategoryBranches(t *testing.T) {
	cases := map[string]string{"movie": "电影", "MOVIE": "电影", "TV": "电视剧", "anime": "未知", "": "未知"}
	for value, want := range cases {
		if got := sunnyPTCategory(value); got != want {
			t.Fatalf("sunnyPTCategory(%q) = %q，期望 %q", value, got, want)
		}
	}
}

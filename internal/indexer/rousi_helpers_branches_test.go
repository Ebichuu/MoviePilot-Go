package indexer

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
)

// TestNewRousiAdapterValidation 覆盖 RousiPro 构造期的配置校验。
func TestNewRousiAdapterValidation(t *testing.T) {
	client := newIndexerHTTPClient(t, 1)
	if _, err := NewRousiAdapter(nil, 1, "x", "https://index.example.com", nil); err == nil ||
		!strings.Contains(err.Error(), "站点 HTTP 客户端不能为空") {
		t.Fatalf("空客户端 = %v", err)
	}
	if _, err := NewRousiAdapter(client, 1, "x", "ftp://index.example.com", nil); err == nil {
		t.Fatal("非法站点地址应报错")
	}
	if _, err := NewRousiAdapter(client, 1, "x", "https://index.example.com", nil); err == nil ||
		!strings.Contains(err.Error(), "未配置 Passkey") {
		t.Fatalf("缺 Passkey = %v", err)
	}
	if _, err := NewRousiAdapter(client, 1, "x", "https://index.example.com", map[string]any{
		"apikey": strings.Repeat("k", 4097),
	}); err == nil || !strings.Contains(err.Error(), "Passkey 无效") {
		t.Fatalf("超长 Passkey = %v", err)
	}
	if _, err := NewRousiAdapter(client, 1, "x", "https://index.example.com", map[string]any{
		"passkey": "k\r\nX-Injected: 1",
	}); err == nil || !strings.Contains(err.Error(), "Passkey 无效") {
		t.Fatalf("换行 Passkey = %v", err)
	}
	if _, err := NewRousiAdapter(client, 1, "x", "https://index.example.com", map[string]any{
		"passkey": "k", "token": strings.Repeat("t", 4097),
	}); err == nil || !strings.Contains(err.Error(), "Token 无效") {
		t.Fatalf("超长 Token = %v", err)
	}
	if _, err := NewRousiAdapter(client, 1, "x", "https://index.example.com", map[string]any{
		"passkey": "k", "token": "t\r\nX-Injected: 1",
	}); err == nil || !strings.Contains(err.Error(), "Token 无效") {
		t.Fatalf("换行 Token = %v", err)
	}
	if _, err := NewRousiAdapter(client, 1, "x", "https://index.example.com", map[string]any{
		"passkey": "k", "token": "t", "ua": strings.Repeat("u", 1025),
	}); err == nil || !strings.Contains(err.Error(), "User-Agent 无效") {
		t.Fatalf("超长 UA = %v", err)
	}

	// 无 token：消息请求头为空，名字回落默认值。
	adapter, err := NewRousiAdapter(client, 3, "  ", "https://index.example.com", map[string]any{
		"passkey": "key",
	})
	if err != nil {
		t.Fatalf("合法配置: %v", err)
	}
	if adapter.name != "RousiPro" || adapter.messageHeaders != nil {
		t.Fatalf("默认配置 = %+v", adapter)
	}
	if adapter.headers.Get("Authorization") != "Bearer key" {
		t.Fatalf("ApiKey 请求头 = %q", adapter.headers.Get("Authorization"))
	}
	if adapter.Name() != "RousiPro" || adapter.SiteID() != 3 || adapter.ResolverKind() != "rousi" {
		t.Fatal("适配器元信息错误")
	}

	// 有 token：自动补 Bearer 前缀并带上 UA。
	withToken, err := NewRousiAdapter(client, 3, "Rousi", "https://index.example.com", map[string]any{
		"passkey": "key", "token": "abc", "ua": "fixture/2",
	})
	if err != nil {
		t.Fatalf("带 token: %v", err)
	}
	if withToken.messageHeaders.Get("Authorization") != "Bearer abc" ||
		withToken.messageHeaders.Get("User-Agent") != "fixture/2" {
		t.Fatalf("消息请求头 = %+v", withToken.messageHeaders)
	}
	explicit, err := NewRousiAdapter(client, 3, "Rousi", "https://index.example.com", map[string]any{
		"passkey": "key", "token": "Bearer xyz",
	})
	if err != nil || explicit.messageHeaders.Get("Authorization") != "Bearer xyz" {
		t.Fatalf("显式 Bearer = %+v（%v）", explicit.messageHeaders, err)
	}
}

// TestRousiSearchBranches 覆盖搜索的业务码、结果形状与条目清洗。
func TestRousiSearchBranches(t *testing.T) {
	var receivedQuery string
	endpoint := probeServer(t, func(w http.ResponseWriter, r *http.Request) {
		receivedQuery = r.URL.RawQuery
		_, _ = w.Write([]byte(`{"code":0,"data":{"torrents":[
			"skip",
			{"id":"","title":"缺 ID"},
			{"id":"1","title":""},
			{"id":"2","title":"保留","size":100,"seeders":3,"leechers":1,"downloads":9,
				"promotion":{"is_active":true,"down_multiplier":0.5,"up_multiplier":2,"until":"2026-12-31 00:00:00"},
				"category":{"slug":"movie"},"uuid":"abc"},
			{"id":"3","title":"分类回落","category":{"name":"TV"},"uuid":"` + strings.Repeat("u", 129) + `"}
		]}}`))
	})
	adapter, err := NewRousiAdapter(newIndexerHTTPClient(t, 1), 5, "Rousi", endpoint, map[string]any{
		"passkey": "k",
	})
	if err != nil {
		t.Fatalf("构造适配器: %v", err)
	}
	resources, err := adapter.Search(context.Background(), Query{Keyword: "某剧", Page: 1, Limit: 10})
	if err != nil {
		t.Fatalf("搜索失败: %v", err)
	}
	if !strings.Contains(receivedQuery, "keyword=") || !strings.Contains(receivedQuery, "page=2") {
		t.Fatalf("查询参数 = %q", receivedQuery)
	}
	if len(resources) != 2 {
		t.Fatalf("资源条数 = %d", len(resources))
	}
	first := resources[0]
	if first.Title != "保留" || first.Category != "电影" || first.SizeBytes != 100 ||
		first.Seeders != 3 || first.Peers != 1 || first.Grabs != 9 {
		t.Fatalf("首条字段 = %+v", first)
	}
	if first.DownloadVolumeFactor == nil || *first.DownloadVolumeFactor != 0.5 ||
		first.UploadVolumeFactor == nil || *first.UploadVolumeFactor != 2 {
		t.Fatalf("促销系数 = %+v", first)
	}
	if !strings.Contains(first.DetailURL, "/torrent/abc") {
		t.Fatalf("详情地址 = %q", first.DetailURL)
	}
	if resources[1].Category != "电视剧" || resources[1].DetailURL != "" {
		t.Fatalf("分类回落与超长 uuid = %+v", resources[1])
	}

	// 业务码非零、结果非数组、空关键字。
	for _, body := range []string{`{"code":7}`, `{"code":0,"data":{}}`} {
		brokenEndpoint := probeServer(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(body))
		})
		broken, err := NewRousiAdapter(newIndexerHTTPClient(t, 1), 5, "Rousi", brokenEndpoint, map[string]any{
			"passkey": "k",
		})
		if err != nil {
			t.Fatalf("构造适配器: %v", err)
		}
		if _, searchErr := broken.Search(context.Background(), Query{Keyword: ""}); searchErr == nil {
			t.Fatalf("%s 应报错", body)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := adapter.Search(ctx, Query{Keyword: "某剧"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("取消上下文 = %v", err)
	}
}

// TestRousiResolveDownloadBranches 覆盖下载解析的 ID 与地址校验。
func TestRousiResolveDownloadBranches(t *testing.T) {
	empty := probeServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"data":{"download_url":""}}`))
	})
	adapter, err := NewRousiAdapter(newIndexerHTTPClient(t, 1), 5, "Rousi", empty, map[string]any{
		"passkey": "k",
	})
	if err != nil {
		t.Fatalf("构造适配器: %v", err)
	}
	if _, err := adapter.ResolveDownload(context.Background(), "abc"); err == nil ||
		!strings.Contains(err.Error(), "种子 ID 无效") {
		t.Fatalf("非法 ID = %v", err)
	}
	if _, err := adapter.ResolveDownload(context.Background(), "42"); err == nil ||
		!strings.Contains(err.Error(), "下载地址无效") {
		t.Fatalf("空下载地址 = %v", err)
	}

	untrusted := probeServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"data":{"download_url":"https://evil.example.com/x.torrent"}}`))
	})
	adapter, err = NewRousiAdapter(newIndexerHTTPClient(t, 1), 5, "Rousi", untrusted, map[string]any{
		"passkey": "k",
	})
	if err != nil {
		t.Fatalf("构造适配器: %v", err)
	}
	if _, err := adapter.ResolveDownload(context.Background(), "42"); err == nil ||
		!strings.Contains(err.Error(), "下载地址不受信任") {
		t.Fatalf("跨主机 = %v", err)
	}

	success := probeServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/api/v1/torrents/") {
			_, _ = w.Write([]byte(`{"code":0,"data":{"download_url":"/download/42"}}`))
			return
		}
		_, _ = w.Write([]byte("d4:infod4:name4:rousiee"))
	})
	adapter, err = NewRousiAdapter(newIndexerHTTPClient(t, 1), 5, "Rousi", success, map[string]any{
		"passkey": "k",
	})
	if err != nil {
		t.Fatalf("构造适配器: %v", err)
	}
	download, err := adapter.ResolveDownload(context.Background(), "42")
	if err != nil || string(download.Body) != "d4:infod4:name4:rousiee" ||
		download.Filename != "42.torrent" || download.ContentType != "application/x-bittorrent" {
		t.Fatalf("下载结果 = %+v（%v）", download, err)
	}
}

// TestRousiCategoryBranches 覆盖分类映射。
func TestRousiCategoryBranches(t *testing.T) {
	cases := map[string]string{"movie": "电影", "MOVIE": "电影", " tv ": "电视剧", "other": "未知", "": "未知"}
	for value, want := range cases {
		if got := rousiCategory(value); got != want {
			t.Fatalf("rousiCategory(%q) = %q，期望 %q", value, got, want)
		}
	}
}

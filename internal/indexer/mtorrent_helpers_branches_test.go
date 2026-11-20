package indexer

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

// TestNewMTorrentAdapterValidation 覆盖 mTorrent 构造期的四类配置错误。
func TestNewMTorrentAdapterValidation(t *testing.T) {
	client := newIndexerHTTPClient(t, 1)
	if _, err := NewMTorrentAdapter(nil, 1, "x", "https://index.example.com", nil); err == nil ||
		!strings.Contains(err.Error(), "站点 HTTP 客户端不能为空") {
		t.Fatalf("空客户端 = %v", err)
	}
	if _, err := NewMTorrentAdapter(client, 1, "x", "ftp://index.example.com", nil); err == nil {
		t.Fatal("非法站点地址应报错")
	}
	if _, err := NewMTorrentAdapter(client, 1, "x", "https://index.example.com", map[string]any{
		"api_url": "ftp://api.example.com", "apikey": "k",
	}); err == nil || !strings.Contains(err.Error(), "mTorrent API 地址无效") {
		t.Fatalf("非法 API 地址 = %v", err)
	}
	if _, err := NewMTorrentAdapter(client, 1, "x", "https://index.example.com", nil); err == nil ||
		!strings.Contains(err.Error(), "未配置 API Key") {
		t.Fatalf("缺 Key = %v", err)
	}
	if _, err := NewMTorrentAdapter(client, 1, "x", "https://index.example.com", map[string]any{
		"apikey": strings.Repeat("k", 4097),
	}); err == nil || !strings.Contains(err.Error(), "API Key 无效") {
		t.Fatalf("超长 Key = %v", err)
	}
	if _, err := NewMTorrentAdapter(client, 1, "x", "https://index.example.com", map[string]any{
		"apikey": "k\r\nX-Injected: 1",
	}); err == nil || !strings.Contains(err.Error(), "API Key 无效") {
		t.Fatalf("换行 Key = %v", err)
	}
	if _, err := NewMTorrentAdapter(client, 1, "x", "https://index.example.com", map[string]any{
		"apikey": "k", "ua": strings.Repeat("u", 1025),
	}); err == nil || !strings.Contains(err.Error(), "User-Agent 无效") {
		t.Fatalf("超长 UA = %v", err)
	}
	if _, err := NewMTorrentAdapter(client, 1, "x", "https://index.example.com", map[string]any{
		"apikey": "k", "ua": "bad\r\nua",
	}); err == nil || !strings.Contains(err.Error(), "User-Agent 无效") {
		t.Fatalf("换行 UA = %v", err)
	}

	// 名字为空回落默认值，API 地址由站点域名推导。
	adapter, err := NewMTorrentAdapter(client, 7, "   ", "https://www.index.example.com", map[string]any{
		"apikey": "key", "ua": "fixture/1",
	})
	if err != nil {
		t.Fatalf("合法配置: %v", err)
	}
	if adapter.name != "mTorrent" {
		t.Fatalf("默认名称 = %q", adapter.name)
	}
	if adapter.api == nil || adapter.api.Host != "api.index.example.com" {
		t.Fatalf("推导 API 地址 = %+v", adapter.api)
	}
	if adapter.headers.Get("x-api-key") != "key" || adapter.headers.Get("User-Agent") != "fixture/1" {
		t.Fatalf("请求头 = %+v", adapter.headers)
	}
	if adapter.Name() != "mTorrent" || adapter.SiteID() != 7 || adapter.ResolverKind() != "mtorrent" {
		t.Fatal("适配器元信息错误")
	}
}

// TestMTorrentSearchBranches 覆盖搜索的关键字改写与结果形状校验。
func TestMTorrentSearchBranches(t *testing.T) {
	var received map[string]any
	endpoint := probeServer(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		received = map[string]any{"raw": string(body)}
		if strings.Contains(string(body), "imdb.com") {
			_, _ = w.Write([]byte(`{"code":0,"data":{"data":[
				{"id":1,"name":"某剧","category":"403","size":"10","status":{"seeders":1}}]}}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":0,"data":{}}`))
	})
	adapter, err := NewMTorrentAdapter(newIndexerHTTPClient(t, 1), 12, "M-Team", endpoint, map[string]any{
		"api_url": endpoint, "apikey": "k",
	})
	if err != nil {
		t.Fatalf("构造适配器: %v", err)
	}

	resources, err := adapter.Search(context.Background(), Query{Keyword: "tt123", Page: -1, Limit: 999})
	if err != nil || len(resources) != 1 {
		t.Fatalf("IMDB 搜索 = %d（%v）", len(resources), err)
	}
	if !strings.Contains(received["raw"].(string), "imdb.com") {
		t.Fatalf("关键字未改写成 IMDB 链接: %s", received["raw"])
	}
	if resources[0].Category != "电视剧" || resources[0].DownloadURL == "" {
		t.Fatalf("资源字段 = %+v", resources[0])
	}

	if _, err := adapter.Search(context.Background(), Query{Keyword: "某剧"}); err == nil ||
		!strings.Contains(err.Error(), "搜索结果不是数组") {
		t.Fatalf("结果非数组 = %v", err)
	}
}

// TestMTorrentResolveDownloadBranches 覆盖下载解析的 ID 与地址校验。
func TestMTorrentResolveDownloadBranches(t *testing.T) {
	endpoint := probeServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"data":""}`))
	})
	adapter, err := NewMTorrentAdapter(newIndexerHTTPClient(t, 1), 12, "M-Team", endpoint, map[string]any{
		"api_url": endpoint, "apikey": "k",
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
		_, _ = w.Write([]byte(`{"code":0,"data":"https://evil.example.com/x.torrent"}`))
	})
	adapter, err = NewMTorrentAdapter(newIndexerHTTPClient(t, 1), 12, "M-Team", untrusted, map[string]any{
		"api_url": untrusted, "apikey": "k",
	})
	if err != nil {
		t.Fatalf("构造适配器: %v", err)
	}
	if _, err := adapter.ResolveDownload(context.Background(), "42"); err == nil ||
		!strings.Contains(err.Error(), "下载地址不受信任") {
		t.Fatalf("跨主机下载地址 = %v", err)
	}

	broken := probeServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`not json`))
	})
	adapter, err = NewMTorrentAdapter(newIndexerHTTPClient(t, 1), 12, "M-Team", broken, map[string]any{
		"api_url": broken, "apikey": "k",
	})
	if err != nil {
		t.Fatalf("构造适配器: %v", err)
	}
	if _, err := adapter.ResolveDownload(context.Background(), "42"); err == nil {
		t.Fatal("非法响应应报错")
	}
}

// TestMTorrentFactorsBranches 覆盖促销系数映射。
func TestMTorrentFactorsBranches(t *testing.T) {
	cases := []struct {
		discount string
		down     float64
		up       float64
	}{
		{discount: "FREE", down: 0, up: 1},
		{discount: "_2X_FREE", down: 0, up: 2},
		{discount: "PERCENT_50", down: 0.5, up: 1},
		{discount: "_2X_PERCENT_50", down: 0.5, up: 2},
		{discount: "PERCENT_70", down: 0.3, up: 1},
		{discount: "_2X", down: 1, up: 2},
		{discount: "NORMAL", down: 1, up: 1},
		{discount: "", down: 1, up: 1},
	}
	for _, tc := range cases {
		down, up := mTorrentFactors(tc.discount)
		if down != tc.down || up != tc.up {
			t.Fatalf("mTorrentFactors(%q) = (%v,%v)，期望 (%v,%v)", tc.discount, down, up, tc.down, tc.up)
		}
	}
	// 大小写不敏感。
	if down, up := mTorrentFactors("free"); down != 0 || up != 1 {
		t.Fatalf("小写促销 = (%v,%v)", down, up)
	}
}

// TestMTorrentCategoryBranches 覆盖分类映射。
func TestMTorrentCategoryBranches(t *testing.T) {
	for _, code := range []string{"401", "419", "420", "421", "439", "405", "404"} {
		if got := mTorrentCategory(code); got != "电影" {
			t.Fatalf("%s = %q", code, got)
		}
	}
	for _, code := range []string{"403", "402", "435", "438"} {
		if got := mTorrentCategory(code); got != "电视剧" {
			t.Fatalf("%s = %q", code, got)
		}
	}
	if got := mTorrentCategory("999"); got != "未知" {
		t.Fatalf("未知分类 = %q", got)
	}
}

// TestStringListBranches 覆盖字符串列表清洗。
func TestStringListBranches(t *testing.T) {
	if list := stringList("nope", 4); list != nil {
		t.Fatalf("非数组 = %+v", list)
	}
	if list := stringList(nil, 4); list != nil {
		t.Fatalf("nil = %+v", list)
	}
	list := stringList([]any{" a ", "", "b", 3, "   "}, 2)
	if len(list) != 2 || list[0] != "a" || list[1] != "b" {
		t.Fatalf("列表 = %+v", list)
	}
	list = stringList([]any{"a", "b", "c"}, 5)
	if len(list) != 3 {
		t.Fatalf("上限未截断 = %+v", list)
	}
}

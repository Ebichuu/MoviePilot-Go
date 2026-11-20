package indexer

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
	"testing"
)

// TestNewHDDolbyAdapterValidation 覆盖 HDDBits 构造期的鉴权校验。
func TestNewHDDolbyAdapterValidation(t *testing.T) {
	client := newIndexerHTTPClient(t, 1)
	cases := []struct {
		name    string
		base    string
		config  map[string]any
		message string
	}{
		{name: "非法站点地址", base: "ftp://index.example.com", message: ""},
		{name: "非法 API 地址", base: "https://index.example.com",
			config: map[string]any{"api_url": "ftp://api.example.com", "apikey": "k", "cookie": "c"}, message: "HDDolby API 地址无效"},
		{name: "缺 Key 与 Cookie", base: "https://index.example.com", message: "必须配置 API Key 和 Cookie"},
		{name: "缺 Cookie", base: "https://index.example.com",
			config: map[string]any{"apikey": "k"}, message: "必须配置 API Key 和 Cookie"},
		{name: "超长 Key", base: "https://index.example.com",
			config: map[string]any{"apikey": strings.Repeat("k", 4097), "cookie": "c"}, message: "鉴权配置无效"},
		{name: "超长 Cookie", base: "https://index.example.com",
			config: map[string]any{"apikey": "k", "cookie": strings.Repeat("c", 64<<10+1)}, message: "鉴权配置无效"},
		{name: "含换行", base: "https://index.example.com",
			config: map[string]any{"apikey": "k", "cookie": "c\r\nX: 1"}, message: "鉴权配置无效"},
	}
	if _, err := NewHDDolbyAdapter(nil, 1, "x", "https://index.example.com", nil); err == nil ||
		!strings.Contains(err.Error(), "站点 HTTP 客户端不能为空") {
		t.Fatalf("空客户端 = %v", err)
	}
	for _, tc := range cases {
		_, err := NewHDDolbyAdapter(client, 1, "x", tc.base, tc.config)
		if err == nil {
			t.Fatalf("%s 应报错", tc.name)
		}
		if tc.message != "" && !strings.Contains(err.Error(), tc.message) {
			t.Fatalf("%s = %v", tc.name, err)
		}
	}

	adapter, err := NewHDDolbyAdapter(client, 4, "  ", "https://www.index.example.com", map[string]any{
		"apikey": "key", "cookie": "session=abc",
	})
	if err != nil {
		t.Fatalf("合法配置: %v", err)
	}
	if adapter.name != "HDDolby" || adapter.api.Host != "api.index.example.com" {
		t.Fatalf("默认配置 = name:%q api:%v", adapter.name, adapter.api)
	}
	if adapter.headers.Get("x-api-key") != "key" || adapter.headers.Get("Cookie") != "session=abc" ||
		adapter.headers.Get("Referer") == "" {
		t.Fatalf("请求头 = %+v", adapter.headers)
	}
	if adapter.Name() != "HDDolby" || adapter.SiteID() != 4 || adapter.ResolverKind() != "hddolby" {
		t.Fatal("适配器元信息错误")
	}
}

// TestHDDolbySearchBranches 覆盖搜索的失败码、脏条目与字段映射。
func TestHDDolbySearchBranches(t *testing.T) {
	var payload string
	endpoint := probeServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "detail") || strings.Contains(r.URL.Path, "download.php") {
			_, _ = w.Write([]byte("d4:infod4:name3:hde"))
			return
		}
		body := make([]byte, 0, 512)
		buffer := make([]byte, 512)
		for {
			n, err := r.Body.Read(buffer)
			body = append(body, buffer[:n]...)
			if err != nil {
				break
			}
		}
		payload = string(body)
		_, _ = w.Write([]byte(`{"data":[
			"skip",
			{"id":"","name":"缺 ID","downhash":"h"},
			{"id":"1","name":"","downhash":"h"},
			{"id":"2","name":"缺 hash"},
			{"id":"3","name":"超长 hash","downhash":"` + strings.Repeat("h", 513) + `"},
			{"id":"4","name":"保留","downhash":"xyz","size":1024,"seeders":3,"leechers":1,
				"times_completed":2,"promotion_time_type":2,"promotion_until":"2026-06-01 00:00:00",
				"tags":"gf;HDR10;unknown","category":401,"added":"2026-05-01 00:00:00","small_descr":"描述"}
		]}`))
	})
	adapter, err := NewHDDolbyAdapter(newIndexerHTTPClient(t, 1), 4, "HDD", endpoint, map[string]any{
		"api_url": endpoint, "apikey": "k", "cookie": "c",
	})
	if err != nil {
		t.Fatalf("构造适配器: %v", err)
	}
	resources, err := adapter.Search(context.Background(), Query{Keyword: "某剧", Page: 1, Limit: 10})
	if err != nil {
		t.Fatalf("搜索失败: %v", err)
	}
	for _, fragment := range []string{`"page_number":1`, `"page_size":10`, "某剧"} {
		if !strings.Contains(payload, fragment) {
			t.Fatalf("请求体缺少 %q: %s", fragment, payload)
		}
	}
	if len(resources) != 1 {
		t.Fatalf("资源条数 = %d", len(resources))
	}
	resource := resources[0]
	if resource.Title != "保留" || resource.SizeBytes != 1024 || resource.Seeders != 3 ||
		resource.Peers != 1 || resource.Grabs != 2 || resource.Description != "描述" {
		t.Fatalf("资源字段 = %+v", resource)
	}
	if resource.Category != "电影" || len(resource.Labels) != 2 || resource.Labels[1] != "HDR10" {
		t.Fatalf("分类/标签 = %+v", resource)
	}
	if resource.DownloadVolumeFactor == nil || *resource.DownloadVolumeFactor != 0 ||
		resource.UploadVolumeFactor == nil || *resource.UploadVolumeFactor != 1 {
		t.Fatalf("促销系数 = %+v", resource)
	}
	if !strings.Contains(resource.DetailURL, "/details.php?id=4") {
		t.Fatalf("详情地址 = %q", resource.DetailURL)
	}
	if resource.FreeDate.IsZero() {
		t.Fatalf("促销结束时间未解析: %+v", resource.FreeDate)
	}

	for _, body := range []string{`{"error":"boom"}`, `{"data":{}}`} {
		brokenEndpoint := probeServer(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(body))
		})
		broken, err := NewHDDolbyAdapter(newIndexerHTTPClient(t, 1), 4, "HDD", brokenEndpoint, map[string]any{
			"api_url": brokenEndpoint, "apikey": "k", "cookie": "c",
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

// TestHDDolbyResolveDownloadBranches 覆盖下载标识解码与下载。
func TestHDDolbyResolveDownloadBranches(t *testing.T) {
	var downloadQuery string
	endpoint := probeServer(t, func(w http.ResponseWriter, r *http.Request) {
		downloadQuery = r.URL.RawQuery
		_, _ = w.Write([]byte("d4:infod4:name3:hde"))
	})
	adapter, err := NewHDDolbyAdapter(newIndexerHTTPClient(t, 1), 4, "HDD", endpoint, map[string]any{
		"api_url": endpoint, "apikey": "k", "cookie": "c",
	})
	if err != nil {
		t.Fatalf("构造适配器: %v", err)
	}
	download, err := adapter.ResolveDownload(context.Background(), base64.RawURLEncoding.EncodeToString([]byte("4\nxyz")))
	if err != nil || string(download.Body) != "d4:infod4:name3:hde" ||
		download.Filename != "4.torrent" || download.ContentType != "application/x-bittorrent" {
		t.Fatalf("下载结果 = %+v（%v）", download, err)
	}
	if !strings.Contains(downloadQuery, "id=4") {
		t.Fatalf("下载查询串 = %q", downloadQuery)
	}

	invalid := []string{"", "!!!", "NAo", base64.RawURLEncoding.EncodeToString([]byte("abc\nxyz")), base64.RawURLEncoding.EncodeToString([]byte("4\n" + strings.Repeat("h", 513)))}
	for _, value := range invalid {
		if _, err := adapter.ResolveDownload(context.Background(), value); err == nil {
			t.Fatalf("%q 应判定为非法标识", value)
		}
	}
}

// TestHDDolbyMappingBranches 覆盖促销、分类与标签映射。
func TestHDDolbyMappingBranches(t *testing.T) {
	factors := map[int][2]float64{2: {0, 1}, 3: {1, 2}, 4: {1, 2}, 5: {0.5, 1}, 6: {1, 2}, 7: {0.3, 1}, 99: {1, 1}}
	for value, want := range factors {
		down, up := hddolbyFactors(value)
		if down != want[0] || up != want[1] {
			t.Fatalf("hddolbyFactors(%d) = (%v,%v)", value, down, up)
		}
	}
	categories := map[int]string{401: "电影", 404: "电影", 405: "电影", 402: "电视剧", 403: "电视剧", 999: "未知"}
	for value, want := range categories {
		if got := hddolbyCategory(value); got != want {
			t.Fatalf("hddolbyCategory(%d) = %q", value, got)
		}
	}
	if labels := hddolbyLabels(""); len(labels) != 0 {
		t.Fatalf("空标签 = %+v", labels)
	}
	labels := hddolbyLabels("GF, hdr10 ; db; unknown")
	if len(labels) != 3 || labels[0] != "官方" || labels[1] != "HDR10" || labels[2] != "Dolby Vision" {
		t.Fatalf("标签映射 = %+v", labels)
	}
}

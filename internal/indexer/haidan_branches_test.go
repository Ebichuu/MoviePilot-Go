package indexer

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// 本文件覆盖 Haidan 适配器的构造闸门、搜索响应的各类畸形输入，
// 以及下载解析的校验分支。

func TestHaidanAdapterConstructorGuards(t *testing.T) {
	client := newIndexerHTTPClient(t, 1)

	if _, err := NewHaidanAdapter(nil, 1, "Haidan", "https://tracker.example", map[string]any{"cookie": "c"}); err == nil {
		t.Fatal("nil 客户端应被拒绝")
	}
	if _, err := NewHaidanAdapter(client, 1, "Haidan", "ftp://tracker.example", map[string]any{"cookie": "c"}); err == nil {
		t.Fatal("非 HTTP 站点地址应被拒绝")
	}
	if _, err := NewHaidanAdapter(client, 1, "Haidan", "https://tracker.example", nil); err == nil {
		t.Fatal("缺少 Cookie 应被拒绝")
	}
	for name, cookie := range map[string]string{
		"换行": "session=a\r\nsession=b",
		"超长": strings.Repeat("a", 64<<10+1),
	} {
		t.Run("Cookie"+name, func(t *testing.T) {
			if _, err := NewHaidanAdapter(client, 1, "Haidan", "https://tracker.example", map[string]any{"cookie": cookie}); err == nil {
				t.Fatal("非法 Cookie 应被拒绝")
			}
		})
	}
	for name, ua := range map[string]string{
		"换行": "agent\r\nX-Inject: 1",
		"超长": strings.Repeat("u", 1025),
	} {
		t.Run("UA"+name, func(t *testing.T) {
			_, err := NewHaidanAdapter(client, 1, "Haidan", "https://tracker.example", map[string]any{"cookie": "c", "ua": ua})
			if err == nil {
				t.Fatal("非法 User-Agent 应被拒绝")
			}
		})
	}

	adapter, err := NewHaidanAdapter(client, 21, "   ", "https://tracker.example", map[string]any{"cookie": "c"})
	if err != nil {
		t.Fatalf("合法配置构造失败: %v", err)
	}
	if adapter.Name() != "Haidan" {
		t.Fatalf("空名称应回退为 Haidan，得到 %q", adapter.Name())
	}
}

func TestHaidanSearchRejectsMalformedResponses(t *testing.T) {
	tests := []struct {
		name string
		body string
		code int
	}{
		{name: "非 JSON", body: "not-json", code: http.StatusOK},
		{name: "接口返回失败", body: `{"code":1,"message":"denied"}`, code: http.StatusOK},
		{name: "data 不是对象", body: `{"code":0,"data":[]}`, code: http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.code)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer server.Close()
			adapter, err := NewHaidanAdapter(newIndexerHTTPClient(t, 1), 22, "Haidan", server.URL, map[string]any{"cookie": "c"})
			if err != nil {
				t.Fatalf("构造适配器: %v", err)
			}
			if _, err := adapter.Search(context.Background(), Query{Keyword: "x", Limit: 5}); err == nil {
				t.Fatal("畸形响应应返回错误")
			}
		})
	}

	t.Run("HTTP 错误状态", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "boom", http.StatusInternalServerError)
		}))
		defer server.Close()
		adapter, err := NewHaidanAdapter(newIndexerHTTPClient(t, 1), 23, "Haidan", server.URL, map[string]any{"cookie": "c"})
		if err != nil {
			t.Fatalf("构造适配器: %v", err)
		}
		if _, err := adapter.Search(context.Background(), Query{Keyword: "x", Limit: 5}); err == nil {
			t.Fatal("HTTP 500 应返回错误")
		}
	})

	t.Run("上下文已取消", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"code":0,"data":{"11":{"name":"t","url":"/d?id=11"}}}`))
		}))
		defer server.Close()
		adapter, err := NewHaidanAdapter(newIndexerHTTPClient(t, 1), 24, "Haidan", server.URL, map[string]any{"cookie": "c"})
		if err != nil {
			t.Fatalf("构造适配器: %v", err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := adapter.Search(ctx, Query{Keyword: "x", Limit: 5}); err == nil {
			t.Fatal("已取消上下文应返回错误")
		}
	})
}

// cancelOnReadTransport 在响应体读完后立刻取消上下文，用于稳定命中
// 「请求成功、遍历结果时上下文已取消」的分支。
type cancelOnReadTransport struct {
	base   http.RoundTripper
	cancel context.CancelFunc
}

func (t *cancelOnReadTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	resp.Body = &cancelOnReadBody{inner: resp.Body, cancel: t.cancel}
	return resp, nil
}

type cancelOnReadBody struct {
	inner  io.ReadCloser
	cancel context.CancelFunc
}

func (b *cancelOnReadBody) Read(p []byte) (int, error) {
	n, err := b.inner.Read(p)
	if err == io.EOF {
		b.cancel()
	}
	return n, err
}

func (b *cancelOnReadBody) Close() error { return b.inner.Close() }

func TestHaidanSearchLoopSkipsBadItems(t *testing.T) {
	var hits int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": 0,
			"data": map[string]any{
				"11":  "不是对象",
				"12":  map[string]any{"name": "", "url": "/download.php?id=12"},
				"13":  map[string]any{"name": "有标题但没地址"},
				"14":  map[string]any{"name": "地址过长", "url": "/" + strings.Repeat("a", 600)},
				"15":  map[string]any{"name": "不可信协议", "url": "ftp://evil.example/x"},
				"16":  map[string]any{"name": "跨主机", "url": "https://other.example/x"},
				"17":  map[string]any{"name": "正常", "url": "/download.php?id=17", "group_id": 3},
				"abc": map[string]any{"name": "非数字 ID", "url": "/download.php?id=abc"},
			},
		})
	}))
	defer server.Close()
	adapter, err := NewHaidanAdapter(newIndexerHTTPClient(t, 1), 25, "Haidan", server.URL, map[string]any{"cookie": "c"})
	if err != nil {
		t.Fatalf("构造适配器: %v", err)
	}
	resources, err := adapter.Search(context.Background(), Query{Keyword: "x", Limit: 100})
	if err != nil {
		t.Fatalf("搜索失败: %v", err)
	}
	if len(resources) != 1 || resources[0].TorrentID != "17" {
		t.Fatalf("应只保留唯一合法条目，得到 %#v", resources)
	}
	if hits != 1 {
		t.Fatalf("应只请求一次，实际 %d", hits)
	}

	truncateServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": 0,
			"data": map[string]any{
				"31": map[string]any{"name": "第一条", "url": "/download.php?id=31"},
				"32": map[string]any{"name": "第二条", "url": "/download.php?id=32"},
			},
		})
	}))
	defer truncateServer.Close()
	truncateAdapter, err := NewHaidanAdapter(newIndexerHTTPClient(t, 1), 28, "Haidan", truncateServer.URL, map[string]any{"cookie": "c"})
	if err != nil {
		t.Fatalf("构造截断适配器: %v", err)
	}
	limited, err := truncateAdapter.Search(context.Background(), Query{Keyword: "x", Limit: 1})
	if err != nil {
		t.Fatalf("限制条数搜索失败: %v", err)
	}
	if len(limited) != 1 || limited[0].TorrentID != "31" {
		t.Fatalf("limit=1 应只返回排序后的第一条，得到 %#v", limited)
	}
}

func TestHaidanSearchCancelsDuringIteration(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"data":{"11":{"name":"t","url":"/download.php?id=11"}}}`))
	}))
	defer server.Close()
	client := newIndexerHTTPClient(t, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client.client.Transport = &cancelOnReadTransport{base: http.DefaultTransport, cancel: cancel}
	adapter, err := NewHaidanAdapter(client, 26, "Haidan", server.URL, map[string]any{"cookie": "c"})
	if err != nil {
		t.Fatalf("构造适配器: %v", err)
	}
	if _, err := adapter.Search(ctx, Query{Keyword: "x", Limit: 5}); err == nil {
		t.Fatal("遍历阶段上下文已取消应返回错误")
	}
}

func TestHaidanResolveDownloadGuards(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/broken.php" {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte("d4:infod4:name4:teste"))
	}))
	defer server.Close()
	adapter, err := NewHaidanAdapter(newIndexerHTTPClient(t, 1), 27, "Haidan", server.URL, map[string]any{"cookie": "c"})
	if err != nil {
		t.Fatalf("构造适配器: %v", err)
	}

	tests := []struct {
		name       string
		resourceID string
	}{
		{name: "空标识", resourceID: ""},
		{name: "标识过长", resourceID: strings.Repeat("a", 1025)},
		{name: "非法 base64", resourceID: "%%%"},
		{name: "跨主机地址", resourceID: base64.RawURLEncoding.EncodeToString([]byte("https://other.example/x"))},
		{name: "下载请求失败", resourceID: base64.RawURLEncoding.EncodeToString([]byte("/broken.php"))},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := adapter.ResolveDownload(context.Background(), tt.resourceID); err == nil {
				t.Fatal("非法下载标识应返回错误")
			}
		})
	}

	valid := base64.RawURLEncoding.EncodeToString([]byte("/download.php?id=1"))
	download, err := adapter.ResolveDownload(context.Background(), valid)
	if err != nil {
		t.Fatalf("合法下载失败: %v", err)
	}
	if download.Filename != "haidan-27.torrent" || download.ContentType != "application/x-bittorrent" {
		t.Fatalf("下载元数据错误: %#v", download)
	}
}

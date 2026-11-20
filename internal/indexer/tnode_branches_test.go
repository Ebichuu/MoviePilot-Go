package indexer

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 本文件覆盖 TNode 适配器的构造校验与失败分支：
// 空客户端、默认站点名、非法站点地址、搜索/下载传输失败、非数字下载 ID。

func TestNewTNodeAdapterValidatesConfig(t *testing.T) {
	client := newIndexerHTTPClient(t, 1)

	if _, err := NewTNodeAdapter(nil, 9, "TNode", "https://tnode.example", map[string]any{"cookie": "c"}); err == nil ||
		!strings.Contains(err.Error(), "站点 HTTP 客户端不能为空") {
		t.Fatalf("空客户端 error = %v", err)
	}
	if _, err := NewTNodeAdapter(client, 9, "TNode", "://bad", map[string]any{"cookie": "c"}); err == nil {
		t.Fatal("非法站点地址应返回错误")
	}

	adapter, err := NewTNodeAdapter(client, 9, "   ", "https://tnode.example", map[string]any{"cookie": "c"})
	if err != nil {
		t.Fatalf("构造适配器: %v", err)
	}
	if adapter.Name() != "TNode" {
		t.Fatalf("空站点名应回退为 TNode，得到 %q", adapter.Name())
	}
}

func TestTNodeSearchFailsWhenSiteUnavailable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	endpoint := server.URL
	server.Close()
	adapter, err := NewTNodeAdapter(newIndexerHTTPClient(t, 1), 9, "TNode", endpoint, map[string]any{"cookie": "c"})
	if err != nil {
		t.Fatalf("构造适配器: %v", err)
	}
	if _, err := adapter.Search(context.Background(), Query{}); err == nil {
		t.Fatal("连接不可用时应返回错误")
	}
	if _, err := adapter.ResolveDownload(context.Background(), "88"); err == nil {
		t.Fatal("连接不可用时应返回错误")
	}
}

func TestTNodeSearchClearsNonNumericDownloadID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			_, _ = w.Write([]byte(`<html><meta name="x-csrf-token" content="fixture-token"></html>`))
		case "/api/torrent/advancedSearch":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"torrents": []any{
				map[string]any{"id": "abc", "title": "非数字 ID", "size": 1024},
				map[string]any{"id": 89, "title": "正常条目", "size": 2048, "seeding": 4},
			}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	adapter, err := NewTNodeAdapter(newIndexerHTTPClient(t, 1), 9, "TNode", server.URL, map[string]any{
		"cookie": "session=fixture",
	})
	if err != nil {
		t.Fatalf("构造适配器: %v", err)
	}
	resources, err := adapter.Search(context.Background(), Query{Limit: 10})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(resources) != 2 {
		t.Fatalf("资源条数 = %d", len(resources))
	}
	if resources[0].DownloadURL != "" {
		t.Fatalf("非数字 ID 应清空下载地址，得到 %q", resources[0].DownloadURL)
	}
	if !strings.Contains(resources[1].DownloadURL, "/api/v1/resource/download/9/89") {
		t.Fatalf("数字 ID 下载地址 = %q", resources[1].DownloadURL)
	}
}

func TestTNodeResolveDownloadRejectsInvalidID(t *testing.T) {
	adapter, err := NewTNodeAdapter(newIndexerHTTPClient(t, 1), 9, "TNode", "https://tnode.example", map[string]any{
		"cookie": "session=fixture",
	})
	if err != nil {
		t.Fatalf("构造适配器: %v", err)
	}
	if _, err := adapter.ResolveDownload(context.Background(), "abc"); err == nil ||
		!strings.Contains(err.Error(), "种子 ID 无效") {
		t.Fatalf("error = %v", err)
	}
}

const tnodeProfileBody = `{"status":200,"data":{"id":77,"username":"fixture-user","upload":100,"download":50}}`

// tnodeUserDataServer 提供 CSRF、用户信息与做种列表三段响应。
func tnodeUserDataServer(t *testing.T, seeding string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			_, _ = w.Write([]byte(`<html><meta name="x-csrf-token" content="fixture-token"></html>`))
		case "/api/user/getMainInfo":
			_, _ = w.Write([]byte(tnodeProfileBody))
		case "/api/user/listTorrentActivity":
			_, _ = w.Write([]byte(seeding))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func TestTNodeUserDataFailures(t *testing.T) {
	ctx := context.Background()

	t.Run("CSRF 获取失败", func(t *testing.T) {
		closed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		endpoint := closed.URL
		closed.Close()
		adapter, err := NewTNodeAdapter(newIndexerHTTPClient(t, 1), 9, "TNode", endpoint, map[string]any{"cookie": "c"})
		if err != nil {
			t.Fatalf("构造适配器: %v", err)
		}
		if _, err := adapter.FetchSiteUserData(ctx); err == nil {
			t.Fatal("CSRF 获取失败时应返回错误")
		}
	})

	t.Run("用户信息请求失败", func(t *testing.T) {
		server := tnodeUserDataServer(t, `{"status":200,"data":{"torrents":[]}}`)
		client := newIndexerHTTPClient(t, 1)
		client.client.Transport = &failAfterRequestsWith{
			base: http.DefaultTransport, after: 1, err: http.ErrHandlerTimeout,
		}
		adapter, err := NewTNodeAdapter(client, 9, "TNode", server.URL, map[string]any{"cookie": "c"})
		if err != nil {
			t.Fatalf("构造适配器: %v", err)
		}
		if _, err := adapter.FetchSiteUserData(ctx); err == nil {
			t.Fatal("用户信息请求失败时应返回错误")
		}
	})

	t.Run("做种列表请求失败", func(t *testing.T) {
		server := tnodeUserDataServer(t, `{"status":200,"data":{"torrents":[]}}`)
		client := newIndexerHTTPClient(t, 1)
		client.client.Transport = &failAfterRequestsWith{
			base: http.DefaultTransport, after: 2, err: http.ErrHandlerTimeout,
		}
		adapter, err := NewTNodeAdapter(client, 9, "TNode", server.URL, map[string]any{"cookie": "c"})
		if err != nil {
			t.Fatalf("构造适配器: %v", err)
		}
		if _, err := adapter.FetchSiteUserData(ctx); err == nil {
			t.Fatal("做种列表请求失败时应返回错误")
		}
	})

	t.Run("做种数据不是对象", func(t *testing.T) {
		server := tnodeUserDataServer(t, `{"status":200,"data":[]}`)
		adapter, err := NewTNodeAdapter(newIndexerHTTPClient(t, 1), 9, "TNode", server.URL, map[string]any{"cookie": "c"})
		if err != nil {
			t.Fatalf("构造适配器: %v", err)
		}
		data, err := adapter.FetchSiteUserData(ctx)
		if err != nil {
			t.Fatalf("FetchSiteUserData: %v", err)
		}
		if data.Seeding != 0 {
			t.Fatalf("不应统计做种，得到 %d", data.Seeding)
		}
		if data.Username == nil || *data.Username != "fixture-user" {
			t.Fatalf("用户名 = %v", data.Username)
		}
	})

	t.Run("做种列表不是数组", func(t *testing.T) {
		server := tnodeUserDataServer(t, `{"status":200,"data":{"torrents":{}}}`)
		adapter, err := NewTNodeAdapter(newIndexerHTTPClient(t, 1), 9, "TNode", server.URL, map[string]any{"cookie": "c"})
		if err != nil {
			t.Fatalf("构造适配器: %v", err)
		}
		if _, err := adapter.FetchSiteUserData(ctx); err != nil {
			t.Fatalf("FetchSiteUserData: %v", err)
		}
	})

	t.Run("做种遍历期间取消", func(t *testing.T) {
		canceled, cancel := context.WithCancel(context.Background())
		client := newIndexerHTTPClient(t, 1)
		var seedingCalls int
		client.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if r.URL.Path == "/api/user/listTorrentActivity" {
				seedingCalls++
				cancel()
				return mtorrentJSONResponse(`{"status":200,"data":{"torrents":[{"size":1024,"seeding":3}]}}`), nil
			}
			return http.DefaultTransport.RoundTrip(r)
		})
		server := tnodeUserDataServer(t, `{"status":200,"data":{"torrents":[]}}`)
		adapter, err := NewTNodeAdapter(client, 9, "TNode", server.URL, map[string]any{"cookie": "c"})
		if err != nil {
			t.Fatalf("构造适配器: %v", err)
		}
		if _, err := adapter.FetchSiteUserData(canceled); err == nil {
			t.Fatalf("上下文取消后应返回错误（做种请求次数 %d）", seedingCalls)
		}
	})
}

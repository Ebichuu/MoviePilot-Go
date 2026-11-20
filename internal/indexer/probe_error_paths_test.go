package indexer

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// stagedCancelContext 让 Err/Done 的返回值按调用次数推进，从而稳定命中
// Coordinator.TestSite 中「已经通过前置检查、随后才发现取消」的分支。
type stagedCancelContext struct {
	dones     []chan struct{}
	doneCalls atomic.Int32
	errCalls  atomic.Int32
	cancelAt  int32
}

func newStagedCancelContext(cancelAt int32, dones ...chan struct{}) *stagedCancelContext {
	if len(dones) == 0 {
		dones = []chan struct{}{make(chan struct{})}
	}
	return &stagedCancelContext{dones: dones, cancelAt: cancelAt}
}

func (c *stagedCancelContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (c *stagedCancelContext) Value(any) any               { return nil }
func (c *stagedCancelContext) Done() <-chan struct{} {
	index := int(c.doneCalls.Add(1)) - 1
	if index >= len(c.dones) {
		index = len(c.dones) - 1
	}
	return c.dones[index]
}
func (c *stagedCancelContext) Err() error {
	if c.errCalls.Add(1) >= c.cancelAt {
		return context.Canceled
	}
	return nil
}

func closedSignal() chan struct{} {
	signal := make(chan struct{})
	close(signal)
	return signal
}

// TestCoordinatorTestSiteGuards 覆盖未初始化与空适配器的守卫。
func TestCoordinatorTestSiteGuards(t *testing.T) {
	var nilCoordinator *Coordinator
	if result := nilCoordinator.TestSite(context.Background(), &probeTestAdapter{id: 1}); result.Message != "站点测试服务未初始化" {
		t.Fatalf("nil 协调器 = %#v", result)
	}
	if result := newProbeCoordinator(t, 1).TestSite(context.Background(), nil); result.Message != "站点测试服务未初始化" {
		t.Fatalf("nil 适配器 = %#v", result)
	}
}

// TestCoordinatorTestSiteCanceledWhileWaiting 覆盖在探测槽与站点闸门上的取消。
func TestCoordinatorTestSiteCanceledWhileWaiting(t *testing.T) {
	t.Run("探测槽已满", func(t *testing.T) {
		coordinator := newProbeCoordinator(t, 1)
		coordinator.probeSlots <- struct{}{}
		defer func() { <-coordinator.probeSlots }()

		ctx := newStagedCancelContext(2, closedSignal())
		result := coordinator.TestSite(ctx, &probeTestAdapter{id: 901, name: "slot-full"})
		if result.Success || result.Message != "连接测试已取消" || result.Recordable {
			t.Fatalf("探测槽等待取消 = %#v", result)
		}
	})

	t.Run("取得探测槽后取消", func(t *testing.T) {
		coordinator := newProbeCoordinator(t, 1)
		ctx := newStagedCancelContext(2)
		result := coordinator.TestSite(ctx, &probeTestAdapter{id: 902, name: "after-slot"})
		if result.Success || result.Message != "连接测试已取消" || result.Recordable {
			t.Fatalf("取得探测槽后取消 = %#v", result)
		}
	})

	t.Run("取得闸门后取消", func(t *testing.T) {
		coordinator := newProbeCoordinator(t, 1)
		ctx := newStagedCancelContext(3)
		result := coordinator.TestSite(ctx, &probeTestAdapter{id: 916, name: "after-gate"})
		if result.Success || result.Message != "连接测试已取消" || result.Recordable {
			t.Fatalf("取得闸门后取消 = %#v", result)
		}
	})

	t.Run("站点闸门被占用", func(t *testing.T) {
		coordinator := newProbeCoordinator(t, 1)
		adapter := &probeTestAdapter{id: 903, name: "gate-full"}
		gate := coordinator.siteGate(adapterGateKey(adapter))
		gate <- struct{}{}
		defer func() { <-gate }()

		ctx := newStagedCancelContext(3, make(chan struct{}), closedSignal())
		result := coordinator.TestSite(ctx, adapter)
		if result.Success || result.Message != "连接测试已取消" || result.Recordable {
			t.Fatalf("站点闸门等待取消 = %#v", result)
		}
	})
}

// TestSiteTestResultHandlesNotStartedCancel 覆盖「请求未发出即取消」的归一化。
func TestSiteTestResultHandlesNotStartedCancel(t *testing.T) {
	adapter := &probeTestAdapter{id: 904, name: "not-started", test: func(context.Context) error {
		return fmt.Errorf("%w: %w", errHTTPRequestNotStarted, context.Canceled)
	}}
	result := newProbeCoordinator(t, 1).TestSite(context.Background(), adapter)
	if result.Success || result.Recordable || result.Message != "连接测试已取消" {
		t.Fatalf("未开始即取消 = %#v", result)
	}
}

// TestTNodeTestSiteCSRFGuards 覆盖 TNode 探测的 CSRF 抓取失败与缺失。
func TestTNodeTestSiteCSRFGuards(t *testing.T) {
	t.Run("CSRF 抓取失败", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer server.Close()
		adapter, err := NewTNodeAdapter(newIndexerHTTPClient(t, 1), 905, "TNode", server.URL, map[string]any{"cookie": "fixture"})
		if err != nil {
			t.Fatal(err)
		}
		var statusError *HTTPStatusError
		if err := adapter.TestSite(context.Background()); !errors.As(err, &statusError) ||
			statusError.StatusCode != http.StatusInternalServerError {
			t.Fatalf("CSRF 抓取失败 = %v", err)
		}
	})

	t.Run("缺少 token", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`<html>no token</html>`))
		}))
		defer server.Close()
		adapter, err := NewTNodeAdapter(newIndexerHTTPClient(t, 1), 906, "TNode", server.URL, map[string]any{"cookie": "fixture"})
		if err != nil {
			t.Fatal(err)
		}
		// 清空 CSRF 地址模拟历史配置缺少 token 来源：探测必须明确拒绝而不是带着空头请求。
		adapter.search.def.CSRFPath = ""
		if err := adapter.TestSite(context.Background()); err == nil ||
			!strings.Contains(err.Error(), "站点 CSRF Token 缺失") {
			t.Fatalf("缺少 CSRF token = %v", err)
		}
	})
}

// TestTNodeTestSiteRefreshesExpiredCSRF 覆盖探测遇到 401 时刷新一次 CSRF 并重试。
func TestTNodeTestSiteRefreshesExpiredCSRF(t *testing.T) {
	var csrfHits, infoHits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			count := csrfHits.Add(1)
			_, _ = fmt.Fprintf(w, `<meta name="x-csrf-token" content="tok-%d">`, count)
		case "/api/user/getInfo":
			infoHits.Add(1)
			if r.Header.Get("X-CSRF-TOKEN") == "tok-1" {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			writeProbeJSON(t, w, map[string]any{"data": map[string]any{"username": "fixture"}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	adapter, err := NewTNodeAdapter(newIndexerHTTPClient(t, 1), 907, "TNode", server.URL, map[string]any{"cookie": "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	if err := adapter.TestSite(context.Background()); err != nil {
		t.Fatalf("CSRF 刷新后探测失败: %v", err)
	}
	if csrfHits.Load() != 2 || infoHits.Load() != 2 {
		t.Fatalf("CSRF 抓取=%d getInfo=%d，期望各 2 次", csrfHits.Load(), infoHits.Load())
	}
}

// TestTNodeTestSiteResponseBranches 覆盖 TNode 探测的回落、解码与失效判定。
func TestTNodeTestSiteResponseBranches(t *testing.T) {
	cases := []struct {
		name        string
		getInfo     func(http.ResponseWriter)
		getMainInfo func(http.ResponseWriter)
		message     string
		statusCode  int
	}{
		{
			name: "主接口失败",
			getInfo: func(w http.ResponseWriter) {
				w.WriteHeader(http.StatusNotFound)
			},
			getMainInfo: func(w http.ResponseWriter) {
				w.WriteHeader(http.StatusInternalServerError)
			},
			statusCode: http.StatusInternalServerError,
		},
		{
			name: "响应格式无效",
			getInfo: func(w http.ResponseWriter) {
				_, _ = w.Write([]byte("not json"))
			},
			message: "站点响应格式无效",
		},
		{
			name: "Cookie 已失效",
			getInfo: func(w http.ResponseWriter) {
				_, _ = w.Write([]byte(`{"data":{}}`))
			},
			message: "Cookie 已失效",
		},
		{
			name: "主接口状态非 200",
			getInfo: func(w http.ResponseWriter) {
				w.WriteHeader(http.StatusNotFound)
			},
			getMainInfo: func(w http.ResponseWriter) {
				_, _ = w.Write([]byte(`{"status":500,"data":{"id":1}}`))
			},
			message: "Cookie 已失效",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/":
					_, _ = w.Write([]byte(`<meta name="x-csrf-token" content="tok">`))
				case "/api/user/getInfo":
					tc.getInfo(w)
				case "/api/user/getMainInfo":
					if tc.getMainInfo == nil {
						http.NotFound(w, r)
						return
					}
					tc.getMainInfo(w)
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()

			adapter, err := NewTNodeAdapter(newIndexerHTTPClient(t, 1), 908, "TNode", server.URL, map[string]any{"cookie": "fixture"})
			if err != nil {
				t.Fatal(err)
			}
			testErr := adapter.TestSite(context.Background())
			if tc.statusCode != 0 {
				var statusError *HTTPStatusError
				if !errors.As(testErr, &statusError) || statusError.StatusCode != tc.statusCode {
					t.Fatalf("错误 = %v，期望 HTTP %d", testErr, tc.statusCode)
				}
				return
			}
			if testErr == nil || testErr.Error() != tc.message {
				t.Fatalf("错误 = %v，期望 %q", testErr, tc.message)
			}
		})
	}
}

// TestTNodeTestSiteRefreshFailure 覆盖探测刷新 CSRF 时 token 页面自身失败。
func TestTNodeTestSiteRefreshFailure(t *testing.T) {
	var csrfHits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			if csrfHits.Add(1) == 1 {
				_, _ = w.Write([]byte(`<meta name="x-csrf-token" content="tok-1">`))
				return
			}
			w.WriteHeader(http.StatusInternalServerError)
		case "/api/user/getInfo":
			w.WriteHeader(http.StatusUnauthorized)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	adapter, err := NewTNodeAdapter(newIndexerHTTPClient(t, 1), 917, "TNode", server.URL, map[string]any{"cookie": "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	var statusError *HTTPStatusError
	if err := adapter.TestSite(context.Background()); !errors.As(err, &statusError) ||
		statusError.StatusCode != http.StatusInternalServerError {
		t.Fatalf("刷新 CSRF 失败 = %v", err)
	}
}

// TestBuiltInProbeTransportAndDecodeFailures 覆盖各站点探测的网络与解码失败。
func TestBuiltInProbeTransportAndDecodeFailures(t *testing.T) {
	closedURL := func(t *testing.T) string {
		t.Helper()
		server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		url := server.URL
		server.Close()
		return url
	}
	decodeURL := func(t *testing.T) string {
		t.Helper()
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("not json"))
		}))
		t.Cleanup(server.Close)
		return server.URL
	}

	t.Run("mTorrent 网络失败", func(t *testing.T) {
		adapter, err := NewMTorrentAdapter(newIndexerHTTPClient(t, 1), 909, "M-Team", closedURL(t), map[string]any{
			"api_url": closedURL(t), "apikey": "fixture",
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := adapter.TestSite(context.Background()); err == nil {
			t.Fatal("网络失败时应返回错误")
		}
	})

	t.Run("Yema 网络与解码失败", func(t *testing.T) {
		closed, err := NewYemaAdapter(newIndexerHTTPClient(t, 1), 910, "Yema", closedURL(t), map[string]any{"apikey": "fixture"})
		if err != nil {
			t.Fatal(err)
		}
		if err := closed.TestSite(context.Background()); err == nil {
			t.Fatal("网络失败时应返回错误")
		}
		broken, err := NewYemaAdapter(newIndexerHTTPClient(t, 1), 911, "Yema", decodeURL(t), map[string]any{"apikey": "fixture"})
		if err != nil {
			t.Fatal(err)
		}
		if err := broken.TestSite(context.Background()); err == nil ||
			!strings.Contains(err.Error(), "站点响应格式无效") {
			t.Fatalf("解码失败 = %v", err)
		}
	})

	t.Run("HDDolby 网络与解码失败", func(t *testing.T) {
		closed, err := NewHDDolbyAdapter(newIndexerHTTPClient(t, 1), 912, "HDDolby", closedURL(t), map[string]any{
			"api_url": closedURL(t), "apikey": "fixture", "cookie": "saved=1",
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := closed.TestSite(context.Background()); err == nil {
			t.Fatal("网络失败时应返回错误")
		}
		broken, err := NewHDDolbyAdapter(newIndexerHTTPClient(t, 1), 913, "HDDolby", decodeURL(t), map[string]any{
			"api_url": decodeURL(t), "apikey": "fixture", "cookie": "saved=1",
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := broken.TestSite(context.Background()); err == nil ||
			!strings.Contains(err.Error(), "站点响应格式无效") {
			t.Fatalf("解码失败 = %v", err)
		}
	})

	t.Run("Rousi 网络与解码失败", func(t *testing.T) {
		closed, err := NewRousiAdapter(newIndexerHTTPClient(t, 1), 914, "Rousi", closedURL(t), map[string]any{"apikey": "fixture"})
		if err != nil {
			t.Fatal(err)
		}
		if err := closed.TestSite(context.Background()); err == nil {
			t.Fatal("网络失败时应返回错误")
		}
		broken, err := NewRousiAdapter(newIndexerHTTPClient(t, 1), 915, "Rousi", decodeURL(t), map[string]any{"apikey": "fixture"})
		if err != nil {
			t.Fatal(err)
		}
		if err := broken.TestSite(context.Background()); err == nil ||
			!strings.Contains(err.Error(), "站点响应格式无效") {
			t.Fatalf("解码失败 = %v", err)
		}
	})
}

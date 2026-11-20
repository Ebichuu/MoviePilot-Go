package indexer

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const probeTestWaitTimeout = time.Second

type probeAdapterBuilder func(*testing.T, *HTTPClient, string) Adapter
type probeRequestHandler func(*testing.T, http.ResponseWriter, *http.Request)

func TestBuiltInSiteTestersUseExpectedEndpointsAndCredentials(t *testing.T) {
	tests := []struct {
		name         string
		requestCount int32
		build        probeAdapterBuilder
		handle       probeRequestHandler
	}{
		{
			name:         "TNode",
			requestCount: 2,
			build: func(t *testing.T, client *HTTPClient, baseURL string) Adapter {
				t.Helper()
				adapter, err := NewTNodeAdapter(client, 1, "TNode", baseURL, map[string]any{
					"cookie": "tnode-session", "ua": "probe-agent",
				})
				if err != nil {
					t.Fatal(err)
				}
				return adapter
			},
			handle: func(t *testing.T, w http.ResponseWriter, r *http.Request) {
				t.Helper()
				assertProbeRequest(t, r, http.MethodGet)
				if r.Header.Get("Cookie") != "tnode-session" || r.Header.Get("User-Agent") != "probe-agent" {
					t.Errorf("TNode 探测鉴权头错误: cookie=%q ua=%q", r.Header.Get("Cookie"), r.Header.Get("User-Agent"))
				}
				switch r.URL.Path {
				case "/":
					_, _ = io.WriteString(w, `<meta name="x-csrf-token" content="probe-csrf">`)
				case "/api/user/getInfo":
					if r.Header.Get("X-CSRF-TOKEN") != "probe-csrf" {
						t.Errorf("TNode 探测缺少 CSRF Token: %q", r.Header.Get("X-CSRF-TOKEN"))
					}
					writeProbeJSON(t, w, map[string]any{"data": map[string]any{"username": "fixture"}})
				default:
					t.Errorf("TNode 探测访问了错误端点: %s", r.URL.String())
					http.NotFound(w, r)
				}
			},
		},
		{
			name:         "TorrentLeech",
			requestCount: 1,
			build: func(t *testing.T, client *HTTPClient, baseURL string) Adapter {
				t.Helper()
				adapter, err := NewTorrentLeechAdapter(client, 2, "TorrentLeech", baseURL, map[string]any{
					"cookie": "torrentleech-session", "ua": "probe-agent",
				})
				if err != nil {
					t.Fatal(err)
				}
				return adapter
			},
			handle: func(t *testing.T, w http.ResponseWriter, r *http.Request) {
				t.Helper()
				assertProbeRequest(t, r, http.MethodGet)
				if r.URL.Path != "/" || r.Header.Get("Cookie") != "torrentleech-session" || r.Header.Get("User-Agent") != "probe-agent" {
					t.Errorf("TorrentLeech 探测请求错误: path=%s cookie=%q ua=%q", r.URL.Path, r.Header.Get("Cookie"), r.Header.Get("User-Agent"))
				}
				if !strings.Contains(r.Header.Get("Accept"), "text/html") {
					t.Errorf("TorrentLeech 探测 Accept 错误: %q", r.Header.Get("Accept"))
				}
				_, _ = io.WriteString(w, `<html><a href="/logout">Logout</a></html>`)
			},
		},
		{
			name:         "mTorrent",
			requestCount: 1,
			build: func(t *testing.T, client *HTTPClient, baseURL string) Adapter {
				t.Helper()
				adapter, err := NewMTorrentAdapter(client, 3, "M-Team", baseURL, map[string]any{
					"api_url": baseURL, "apikey": "mtorrent-api-key", "ua": "probe-agent",
				})
				if err != nil {
					t.Fatal(err)
				}
				return adapter
			},
			handle: func(t *testing.T, w http.ResponseWriter, r *http.Request) {
				t.Helper()
				assertProbeRequest(t, r, http.MethodPost)
				if r.URL.Path != "/api/member/profile" || r.Header.Get("x-api-key") != "mtorrent-api-key" || r.Header.Get("User-Agent") != "probe-agent" {
					t.Errorf("mTorrent 探测请求错误: path=%s key=%q ua=%q", r.URL.Path, r.Header.Get("x-api-key"), r.Header.Get("User-Agent"))
				}
				writeProbeJSON(t, w, map[string]any{"code": 0, "data": map[string]any{"id": 3}})
			},
		},
		{
			name:         "SunnyPT",
			requestCount: 1,
			build: func(t *testing.T, client *HTTPClient, baseURL string) Adapter {
				t.Helper()
				adapter, err := NewSunnyPTAdapter(client, 4, "SunnyPT", baseURL, map[string]any{
					"api_url": baseURL + "/api/v1/mp", "apikey": "sunnypt-api-key", "ua": "probe-agent",
				})
				if err != nil {
					t.Fatal(err)
				}
				return adapter
			},
			handle: func(t *testing.T, w http.ResponseWriter, r *http.Request) {
				t.Helper()
				assertProbeRequest(t, r, http.MethodGet)
				if r.URL.Path != "/api/v1/mp/profile" || r.Header.Get("X-API-Key") != "sunnypt-api-key" || r.Header.Get("User-Agent") != "probe-agent" {
					t.Errorf("SunnyPT 探测请求错误: path=%s key=%q ua=%q", r.URL.Path, r.Header.Get("X-API-Key"), r.Header.Get("User-Agent"))
				}
				writeProbeJSON(t, w, map[string]any{"code": 0, "data": map[string]any{"download_allowed": true}})
			},
		},
		{
			name:         "YemaPT",
			requestCount: 1,
			build: func(t *testing.T, client *HTTPClient, baseURL string) Adapter {
				t.Helper()
				adapter, err := NewYemaAdapter(client, 5, "YemaPT", baseURL, map[string]any{
					"apikey": "yema-auth-key", "ua": "probe-agent",
				})
				if err != nil {
					t.Fatal(err)
				}
				return adapter
			},
			handle: func(t *testing.T, w http.ResponseWriter, r *http.Request) {
				t.Helper()
				assertProbeRequest(t, r, http.MethodPost)
				if r.URL.Path != "/openApi/user/fetchBasicInfo.json" || r.Header.Get("Authorization") != "yema-auth-key" || r.Header.Get("User-Agent") != "probe-agent" {
					t.Errorf("YemaPT 探测请求错误: path=%s authorization=%q ua=%q", r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("User-Agent"))
				}
				var payload map[string]any
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil || len(payload) != 0 {
					t.Errorf("YemaPT 探测请求体不是空 JSON 对象: payload=%#v err=%v", payload, err)
				}
				writeProbeJSON(t, w, map[string]any{"success": true, "data": map[string]any{"id": 5}})
			},
		},
		{
			name:         "Haidan",
			requestCount: 1,
			build: func(t *testing.T, client *HTTPClient, baseURL string) Adapter {
				t.Helper()
				adapter, err := NewHaidanAdapter(client, 6, "Haidan", baseURL, map[string]any{
					"cookie": "haidan-session", "ua": "probe-agent",
				})
				if err != nil {
					t.Fatal(err)
				}
				return adapter
			},
			handle: func(t *testing.T, w http.ResponseWriter, r *http.Request) {
				t.Helper()
				assertProbeRequest(t, r, http.MethodGet)
				if r.URL.Path != "/" || r.Header.Get("Cookie") != "haidan-session" || r.Header.Get("User-Agent") != "probe-agent" {
					t.Errorf("Haidan 探测请求错误: path=%s cookie=%q ua=%q", r.URL.Path, r.Header.Get("Cookie"), r.Header.Get("User-Agent"))
				}
				if !strings.Contains(r.Header.Get("Accept"), "text/html") {
					t.Errorf("Haidan 探测 Accept 错误: %q", r.Header.Get("Accept"))
				}
				_, _ = io.WriteString(w, `<html><a href="logout.php">Logout</a></html>`)
			},
		},
		{
			name:         "HDDolby",
			requestCount: 1,
			build: func(t *testing.T, client *HTTPClient, baseURL string) Adapter {
				t.Helper()
				adapter, err := NewHDDolbyAdapter(client, 7, "HDDolby", baseURL, map[string]any{
					"api_url": baseURL, "apikey": "hddolby-api-key", "cookie": "hddolby-session",
				})
				if err != nil {
					t.Fatal(err)
				}
				return adapter
			},
			handle: func(t *testing.T, w http.ResponseWriter, r *http.Request) {
				t.Helper()
				assertProbeRequest(t, r, http.MethodGet)
				if r.URL.Path != "/api/v1/user/data" || r.Header.Get("x-api-key") != "hddolby-api-key" || r.Header.Get("Cookie") != "" {
					t.Errorf("HDDolby 探测请求错误: path=%s key=%q cookie=%q", r.URL.Path, r.Header.Get("x-api-key"), r.Header.Get("Cookie"))
				}
				writeProbeJSON(t, w, map[string]any{"status": 0, "data": map[string]any{"id": 7}})
			},
		},
		{
			name:         "RousiPro",
			requestCount: 1,
			build: func(t *testing.T, client *HTTPClient, baseURL string) Adapter {
				t.Helper()
				adapter, err := NewRousiAdapter(client, 8, "RousiPro", baseURL, map[string]any{"apikey": "rousi-passkey"})
				if err != nil {
					t.Fatal(err)
				}
				return adapter
			},
			handle: func(t *testing.T, w http.ResponseWriter, r *http.Request) {
				t.Helper()
				assertProbeRequest(t, r, http.MethodGet)
				if r.URL.Path != "/api/v1/profile" || r.Header.Get("Authorization") != "Bearer rousi-passkey" {
					t.Errorf("RousiPro 探测请求错误: path=%s authorization=%q", r.URL.Path, r.Header.Get("Authorization"))
				}
				if r.URL.Query().Get("include_fields[user]") != "seeding_leeching_data" {
					t.Errorf("RousiPro 探测查询参数错误: %s", r.URL.RawQuery)
				}
				writeProbeJSON(t, w, map[string]any{"code": 0, "data": map[string]any{"id": 8}})
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				test.handle(t, w, r)
			}))
			defer server.Close()

			client := newIndexerHTTPClient(t, 2)
			defer client.CloseIdleConnections()
			adapter := test.build(t, client, server.URL)
			tester, ok := adapter.(SiteTester)
			if !ok {
				t.Fatalf("%s 未实现 SiteTester", test.name)
			}
			if err := tester.TestSite(context.Background()); err != nil {
				t.Fatalf("%s 连通性探测失败: %v", test.name, err)
			}
			if got := requests.Load(); got != test.requestCount {
				t.Fatalf("%s 探测请求数=%d，期望=%d", test.name, got, test.requestCount)
			}
		})
	}
}

func TestTNodeProbeFallsBackToMainInfo(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		switch r.URL.Path {
		case "/":
			_, _ = io.WriteString(w, `<meta name="x-csrf-token" content="probe-csrf">`)
		case "/api/user/getInfo":
			http.NotFound(w, r)
		case "/api/user/getMainInfo":
			writeProbeJSON(t, w, map[string]any{"status": 200, "data": map[string]any{"username": "fixture"}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := newIndexerHTTPClient(t, 1)
	adapter, err := NewTNodeAdapter(client, 31, "TNode", server.URL, map[string]any{"cookie": "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	result := newProbeCoordinator(t, 1).TestSite(context.Background(), adapter)
	if !result.Success || result.Message != "连接成功" || requests.Load() != 3 {
		t.Fatalf("TNode fallback result=%#v requests=%d", result, requests.Load())
	}
}

func TestSiteTestResultSanitizesAuthenticationFailures(t *testing.T) {
	coordinator := newProbeCoordinator(t, 1)
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			apiKey := fmt.Sprintf("api-secret-%d", status)
			responseSecret := fmt.Sprintf("upstream-body-secret-%d", status)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("X-API-Key") != apiKey {
					t.Errorf("SunnyPT 探测 API Key 错误: %q", r.Header.Get("X-API-Key"))
				}
				w.WriteHeader(status)
				_, _ = io.WriteString(w, `{"error":"`+responseSecret+`"}`)
			}))
			defer server.Close()

			client := newIndexerHTTPClient(t, 1)
			defer client.CloseIdleConnections()
			adapter, err := NewSunnyPTAdapter(client, status, "SunnyPT", server.URL, map[string]any{
				"api_url": server.URL, "apikey": apiKey,
			})
			if err != nil {
				t.Fatal(err)
			}
			result := coordinator.TestSite(context.Background(), adapter)
			if result.Success || result.Message != "站点鉴权失败" {
				t.Fatalf("HTTP %d 探测结果未脱敏: %#v", status, result)
			}
			for _, secret := range []string{apiKey, responseSecret} {
				if strings.Contains(result.Message, secret) {
					t.Fatalf("HTTP %d 探测结果泄露敏感串 %q: %#v", status, secret, result)
				}
			}
		})
	}
}

func TestSiteTestResultRejectsDamagedJSONWithoutLeakingBody(t *testing.T) {
	const apiKey = "damaged-json-api-secret"
	const responseSecret = "damaged-upstream-secret"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/profile" || r.Header.Get("X-API-Key") != apiKey {
			t.Errorf("SunnyPT 损坏响应探测请求错误: path=%s key=%q", r.URL.Path, r.Header.Get("X-API-Key"))
		}
		_, _ = io.WriteString(w, `{"code":0,"data":{"secret":"`+responseSecret+`"}`)
	}))
	defer server.Close()

	client := newIndexerHTTPClient(t, 1)
	defer client.CloseIdleConnections()
	adapter, err := NewSunnyPTAdapter(client, 9, "SunnyPT", server.URL, map[string]any{
		"api_url": server.URL, "apikey": apiKey,
	})
	if err != nil {
		t.Fatal(err)
	}
	result := newProbeCoordinator(t, 1).TestSite(context.Background(), adapter)
	if result.Success || result.Message != "站点响应格式无效" {
		t.Fatalf("损坏 JSON 探测结果错误: %#v", result)
	}
	for _, secret := range []string{apiKey, responseSecret} {
		if strings.Contains(result.Message, secret) {
			t.Fatalf("损坏 JSON 探测结果泄露敏感串 %q: %#v", secret, result)
		}
	}
}

func TestSiteTestResultMapsTimeout(t *testing.T) {
	adapter := &probeTestAdapter{id: 10, name: "timeout", test: func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	started := time.Now()
	result := newProbeCoordinator(t, 1).TestSite(ctx, adapter)
	if result.Success || result.Message != "连接超时" {
		t.Fatalf("超时探测结果错误: %#v", result)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("超时上下文未及时终止探测: %s", elapsed)
	}
}

func TestCoordinatorMarksOnlySiteOutcomesRecordable(t *testing.T) {
	tests := []struct {
		name       string
		adapter    Adapter
		recordable bool
	}{
		{name: "成功", adapter: &probeTestAdapter{id: 101, name: "success"}, recordable: true},
		{name: "站点错误", adapter: &probeTestAdapter{id: 102, name: "failure", test: func(context.Context) error {
			return fmt.Errorf("upstream failed")
		}}, recordable: true},
		{name: "站点超时", adapter: &probeTestAdapter{id: 103, name: "timeout", test: func(context.Context) error {
			return context.DeadlineExceeded
		}}, recordable: true},
		{name: "客户端取消", adapter: &probeTestAdapter{id: 104, name: "cancelled", test: func(context.Context) error {
			return context.Canceled
		}}},
		{name: "适配器不支持探测", adapter: staticAdapter{name: "unsupported"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := newProbeCoordinator(t, 1).TestSite(context.Background(), tt.adapter)
			if result.Recordable != tt.recordable {
				t.Fatalf("Recordable=%v, want %v; result=%#v", result.Recordable, tt.recordable, result)
			}
		})
	}
}

func TestCoordinatorDoesNotRecordHTTPPermitTimeoutAsSiteFailure(t *testing.T) {
	client := newIndexerHTTPClient(t, 1)
	defer client.CloseIdleConnections()
	client.semaphore <- struct{}{}
	defer func() { <-client.semaphore }()

	adapter, err := NewTorrentLeechAdapter(client, 105, "capacity", "https://fixture.example", map[string]any{"cookie": "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	coordinator := newProbeCoordinator(t, 1)
	resultCh := make(chan SiteTestResult, 1)
	go func() {
		resultCh <- coordinator.TestSite(context.Background(), adapter)
	}()
	var result SiteTestResult
	select {
	case result = <-resultCh:
	case <-time.After(time.Second):
		t.Fatal("连接测试在 HTTP 并发槽已满时发生排队")
	}
	if result.Success || result.Recordable || result.Message != "站点测试繁忙" {
		t.Fatalf("HTTP 并发槽超时被错误归因到站点: %#v", result)
	}
}

func TestCoordinatorDoesNotInvokeProbeWithPreCanceledContext(t *testing.T) {
	var calls atomic.Int32
	adapter := &probeTestAdapter{id: 106, name: "pre-cancelled", test: func(context.Context) error {
		calls.Add(1)
		return nil
	}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result := newProbeCoordinator(t, 1).TestSite(ctx, adapter)
	if result.Success || result.Recordable || result.Message != "连接测试已取消" || calls.Load() != 0 {
		t.Fatalf("预取消上下文仍调用了站点适配器: calls=%d result=%#v", calls.Load(), result)
	}
}

func TestSiteTestResultDoesNotLeakUnknownErrors(t *testing.T) {
	const cookie = "session=unknown-error-secret"
	const apiKey = "unknown-api-key-secret"
	const upstream = "<html>upstream-private-body</html>"
	adapter := &probeTestAdapter{id: 11, name: "unknown", test: func(context.Context) error {
		return fmt.Errorf("cookie=%s apikey=%s upstream=%s", cookie, apiKey, upstream)
	}}
	result := newProbeCoordinator(t, 1).TestSite(context.Background(), adapter)
	if result.Success || result.Message != "站点连接失败" {
		t.Fatalf("未知错误未归一化: %#v", result)
	}
	for _, secret := range []string{cookie, apiKey, upstream} {
		if strings.Contains(result.Message, secret) {
			t.Fatalf("未知错误泄露敏感串 %q: %#v", secret, result)
		}
	}
}

func TestProbeLoginPageRejectsUnverifiedAndConflictingHTML(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "ambiguous gateway page", body: `<html><body>gateway error</body></html>`},
		{name: "logout marker on login form", body: `<html><a href="/logout">logout</a><form action="/login"><input type="password"></form></html>`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, test.body)
			}))
			defer server.Close()
			client := newIndexerHTTPClient(t, 1)
			defer client.CloseIdleConnections()
			adapter, err := NewTorrentLeechAdapter(client, 12, "TorrentLeech", server.URL, map[string]any{"cookie": "fixture"})
			if err != nil {
				t.Fatal(err)
			}
			if err := adapter.TestSite(context.Background()); err == nil {
				t.Fatal("无法确认登录状态的 HTML 被误判为连接成功")
			}
		})
	}
}

func TestProbeLoginPageRecognizesCloudflareForbiddenBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `<html><title>Just a moment...</title><div id="cf-chl-widget">challenge</div></html>`)
	}))
	defer server.Close()
	client := newIndexerHTTPClient(t, 1)
	defer client.CloseIdleConnections()
	adapter, err := NewTorrentLeechAdapter(client, 12, "TorrentLeech", server.URL, map[string]any{"cookie": "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	result := newProbeCoordinator(t, 1).TestSite(context.Background(), adapter)
	if result.Success || result.Message != "站点受 Cloudflare 挑战保护" {
		t.Fatalf("Cloudflare 403 body was not recognized: %#v", result)
	}
}

func TestProbeLoginPageRejectsErrorStatusWithPositiveLoginMarkers(t *testing.T) {
	tests := []struct {
		name        string
		status      int
		wantMessage string
	}{
		{name: "forbidden", status: http.StatusForbidden, wantMessage: "站点鉴权失败"},
		{name: "server error", status: http.StatusInternalServerError, wantMessage: "站点返回 HTTP 500"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = io.WriteString(w, `<html><a href="/logout">logout</a></html>`)
			}))
			defer server.Close()
			client := newIndexerHTTPClient(t, 1)
			defer client.CloseIdleConnections()
			adapter, err := NewTorrentLeechAdapter(client, tt.status, "TorrentLeech", server.URL, map[string]any{"cookie": "fixture"})
			if err != nil {
				t.Fatal(err)
			}
			result := newProbeCoordinator(t, 1).TestSite(context.Background(), adapter)
			if result.Success || !result.Recordable || result.Message != tt.wantMessage {
				t.Fatalf("HTTP %d 错误页被误判: %#v", tt.status, result)
			}
		})
	}
}

func TestCoordinatorRejectsProbeWhileSameSiteSearchIsRunning(t *testing.T) {
	coordinator := newProbeCoordinator(t, 1)
	searchEntered := make(chan struct{}, 1)
	releaseSearch, finishSearch := newProbeTestRelease(t)
	var probeCalls atomic.Int32
	adapter := &probeTestAdapter{
		id: 13, name: "same-site",
		search: func(ctx context.Context, _ Query) ([]Resource, error) {
			searchEntered <- struct{}{}
			select {
			case <-releaseSearch:
				return []Resource{}, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		},
		test: func(context.Context) error {
			probeCalls.Add(1)
			return nil
		},
	}
	searchDone := make(chan error, 1)
	go func() {
		_, err := coordinator.Search(t.Context(), []Adapter{adapter}, Query{}, nil)
		searchDone <- err
	}()
	select {
	case <-searchEntered:
	case <-time.After(time.Second):
		t.Fatal("搜索未进入同站并发闸门")
	}

	probeDone := make(chan SiteTestResult, 1)
	go func() {
		probeDone <- coordinator.TestSite(t.Context(), adapter)
	}()
	result := waitForProbeTestResult(t, probeDone, "持有搜索闸门时探测未及时返回")
	if result.Success || result.Message != "站点正在执行其他请求" {
		t.Fatalf("持有搜索闸门时探测未被背压: %#v", result)
	}
	if got := probeCalls.Load(); got != 0 {
		t.Fatalf("搜索期间同站探测越过闸门执行了 %d 次", got)
	}
	finishSearch()
	select {
	case err := <-searchDone:
		if err != nil {
			t.Fatalf("释放闸门后搜索失败: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("释放闸门后搜索未结束")
	}
}

func TestCoordinatorAllowsSameNameDifferentSiteIDsInParallel(t *testing.T) {
	coordinator := newProbeCoordinator(t, 2)
	entered := make(chan int, 2)
	release, finishProbes := newProbeTestRelease(t)
	var active atomic.Int32
	var maximum atomic.Int32
	newAdapter := func(id int) *probeTestAdapter {
		return &probeTestAdapter{id: id, name: "duplicate-name", test: func(ctx context.Context) error {
			current := active.Add(1)
			defer active.Add(-1)
			updateProbeMaximum(&maximum, current)
			entered <- id
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}}
	}
	results := make(chan SiteTestResult, 2)
	for _, adapter := range []*probeTestAdapter{newAdapter(21), newAdapter(22)} {
		go func() {
			results <- coordinator.TestSite(t.Context(), adapter)
		}()
	}
	seen := make(map[int]bool, 2)
	timer := time.NewTimer(probeTestWaitTimeout)
	defer timer.Stop()
	for len(seen) < 2 {
		select {
		case id := <-entered:
			seen[id] = true
		case <-timer.C:
			t.Fatalf("同名不同 SiteID 未并行进入探测: entered=%v", seen)
		}
	}
	finishProbes()
	for range 2 {
		result := waitForProbeTestResult(t, results, "并行探测未结束")
		if !result.Success || result.Message != "连接成功" {
			t.Fatalf("并行探测结果错误: %#v", result)
		}
	}
	if got := maximum.Load(); got != 2 {
		t.Fatalf("同名不同 SiteID 最大并发=%d，期望=2", got)
	}
}

func TestCoordinatorBoundsConcurrentSiteProbes(t *testing.T) {
	coordinator := newProbeCoordinator(t, 1)
	entered := make(chan struct{}, 1)
	release, finishFirstProbe := newProbeTestRelease(t)
	first := &probeTestAdapter{id: 41, name: "first", test: func(ctx context.Context) error {
		entered <- struct{}{}
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}}
	resultCh := make(chan SiteTestResult, 1)
	go func() { resultCh <- coordinator.TestSite(t.Context(), first) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("first probe did not start")
	}
	second := &probeTestAdapter{id: 42, name: "second"}
	secondResultCh := make(chan SiteTestResult, 1)
	go func() { secondResultCh <- coordinator.TestSite(t.Context(), second) }()
	result := waitForProbeTestResult(t, secondResultCh, "busy probe did not return promptly")
	if result.Success || result.Message != "站点测试繁忙" {
		t.Fatalf("second probe was not rejected: %#v", result)
	}
	finishFirstProbe()
	if result := waitForProbeTestResult(t, resultCh, "first probe did not finish after release"); !result.Success {
		t.Fatalf("first probe failed after release: %#v", result)
	}
}

func FuzzDecodeProbeJSON(f *testing.F) {
	for _, seed := range [][]byte{
		[]byte(`{"data":{"id":1}}`),
		[]byte(`{"data":[]}`),
		[]byte(`{"data":true}`),
		[]byte(`not-json`),
		{},
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		if len(body) > 64<<10 {
			return
		}
		document, err := decodeProbeJSON(body)
		if err != nil {
			if err.Error() != "站点响应为空" && err.Error() != "站点响应格式无效" {
				t.Fatalf("unsafe probe JSON error: %q", err)
			}
			return
		}
		if document == nil {
			t.Fatal("valid probe JSON decoded to nil document")
		}
	})
}

type probeTestAdapter struct {
	id     int
	name   string
	search func(context.Context, Query) ([]Resource, error)
	test   func(context.Context) error
}

func (a *probeTestAdapter) Name() string { return a.name }
func (a *probeTestAdapter) SiteID() int  { return a.id }

func (a *probeTestAdapter) Search(ctx context.Context, query Query) ([]Resource, error) {
	if a.search == nil {
		return []Resource{}, nil
	}
	return a.search(ctx, query)
}

func (a *probeTestAdapter) TestSite(ctx context.Context) error {
	if a.test == nil {
		return nil
	}
	return a.test(ctx)
}

func newProbeCoordinator(t *testing.T, workers int) *Coordinator {
	t.Helper()
	coordinator, err := NewCoordinator(workers)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(coordinator.Close)
	return coordinator
}

func newProbeTestRelease(t *testing.T) (<-chan struct{}, func()) {
	t.Helper()
	release := make(chan struct{})
	var once sync.Once
	finish := func() {
		once.Do(func() { close(release) })
	}
	t.Cleanup(finish)
	return release, finish
}

func waitForProbeTestResult(t *testing.T, results <-chan SiteTestResult, timeoutMessage string) SiteTestResult {
	t.Helper()
	timer := time.NewTimer(probeTestWaitTimeout)
	defer timer.Stop()
	select {
	case result := <-results:
		return result
	case <-timer.C:
		t.Fatal(timeoutMessage)
		return SiteTestResult{}
	}
}

func assertProbeRequest(t *testing.T, request *http.Request, method string) {
	t.Helper()
	if request.Method != method {
		t.Errorf("探测 HTTP 方法=%s，期望=%s", request.Method, method)
	}
}

func writeProbeJSON(t *testing.T, writer http.ResponseWriter, value any) {
	t.Helper()
	writer.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(writer).Encode(value); err != nil {
		t.Errorf("编码探测测试响应失败: %v", err)
	}
}

func updateProbeMaximum(maximum *atomic.Int32, current int32) {
	for {
		previous := maximum.Load()
		if current <= previous || maximum.CompareAndSwap(previous, current) {
			return
		}
	}
}

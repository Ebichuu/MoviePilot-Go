package indexer

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/PuerkitoBio/goquery"
)

func probeServer(t *testing.T, handler http.HandlerFunc) string {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server.URL
}

func mustParseProbeURL(t *testing.T, value string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(value)
	if err != nil {
		t.Fatalf("解析地址: %v", err)
	}
	return parsed
}

// TestProbeLoginPageBranches 覆盖登录页探测的各类响应。
func TestProbeLoginPageBranches(t *testing.T) {
	client := newIndexerHTTPClient(t, 1)
	cases := []struct {
		name       string
		status     int
		body       string
		message    string
		statusCode int
	}{
		{name: "已登录（用户面板）", status: http.StatusOK,
			body: `<html><body><div class="user-info-side">用户</div></body></html>`},
		{name: "已登录（退出链接）", status: http.StatusOK,
			body: `<html><body><a href="/logout.php">退出</a></body></html>`},
		{name: "已登录（data-url）", status: http.StatusOK,
			body: `<html><body><a data-url="/logout">退出</a></body></html>`},
		{name: "已登录（表单）", status: http.StatusOK,
			body: `<html><body><form action="/logout"></form></body></html>`},
		{name: "需要登录", status: http.StatusOK,
			body: `<html><body><input type="password" name="password"></body></html>`, message: "Cookie 已失效"},
		{name: "无法确认", status: http.StatusOK, body: `<html><body>普通页面</body></html>`,
			message: "无法确认站点登录状态"},
		{name: "Cloudflare", status: http.StatusOK, body: `<html>cf-chl-xxx</html>`,
			message: "站点受 Cloudflare 挑战保护"},
		{name: "空响应", status: http.StatusOK, body: "", message: "站点响应为空"},
		{name: "重定向", status: http.StatusFound, body: "", statusCode: http.StatusFound},
		{name: "403 命中登录页", status: http.StatusForbidden,
			body: `<html><body><input type="password"></body></html>`, message: "Cookie 已失效"},
		{name: "403 普通页面", status: http.StatusForbidden, body: `<html>forbidden</html>`,
			statusCode: http.StatusForbidden},
		{name: "500 普通页面", status: http.StatusInternalServerError, body: `<html>error</html>`,
			statusCode: http.StatusInternalServerError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			endpoint := probeServer(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			})
			err := probeLoginPage(context.Background(), client, mustParseProbeURL(t, endpoint), http.Header{})
			if tc.statusCode != 0 {
				var statusError *HTTPStatusError
				if !errors.As(err, &statusError) || statusError.StatusCode != tc.statusCode {
					t.Fatalf("错误 = %v，期望状态 %d", err, tc.statusCode)
				}
				return
			}
			if tc.message == "" {
				if err != nil {
					t.Fatalf("应成功，实际 %v", err)
				}
				return
			}
			if err == nil || err.Error() != tc.message {
				t.Fatalf("错误 = %v，期望 %q", err, tc.message)
			}
		})
	}
}

// TestMTorrentTestSiteBranches 覆盖 mTorrent 探测的成功与两种失败。
func TestMTorrentTestSiteBranches(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		message string
		ok      bool
	}{
		{name: "有效", body: `{"code":0,"data":{"id":1}}`, ok: true},
		{name: "Key 失效", body: `{"code":5,"data":{"id":1}}`, message: "API Key 已失效"},
		{name: "数据缺失", body: `{"code":0,"data":{}}`, message: "API Key 已失效"},
		{name: "响应格式无效", body: `not json`, message: "站点响应格式无效"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			endpoint := probeServer(t, func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(tc.body))
			})
			adapter, err := NewMTorrentAdapter(newIndexerHTTPClient(t, 1), 12, "M-Team", endpoint, map[string]any{
				"api_url": endpoint, "apikey": "fixture-key",
			})
			if err != nil {
				t.Fatalf("构造适配器: %v", err)
			}
			testErr := adapter.TestSite(context.Background())
			if tc.ok {
				if testErr != nil {
					t.Fatalf("应成功，实际 %v", testErr)
				}
				return
			}
			if testErr == nil || testErr.Error() != tc.message {
				t.Fatalf("错误 = %v，期望 %q", testErr, tc.message)
			}
		})
	}
}

// TestSunnyPTTestSiteBranches 覆盖 SunnyPT 探测的权限判定。
func TestSunnyPTTestSiteBranches(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		message string
	}{
		{name: "有效", body: `{"code":0,"data":{"download_allowed":true}}`},
		{name: "无下载权限", body: `{"code":0,"data":{"download_allowed":false}}`,
			message: "当前账号没有下载权限"},
		{name: "Key 失效", body: `{"code":1,"data":{"download_allowed":true}}`, message: "API Key 已失效"},
		{name: "数据为空", body: `{"code":0,"data":{}}`, message: "API Key 已失效"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			endpoint := probeServer(t, func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(tc.body))
			})
			adapter, err := NewSunnyPTAdapter(newIndexerHTTPClient(t, 1), 13, "SunnyPT", endpoint, map[string]any{
				"api_url": endpoint, "apikey": "fixture-key",
			})
			if err != nil {
				t.Fatalf("构造适配器: %v", err)
			}
			testErr := adapter.TestSite(context.Background())
			if tc.message == "" {
				if testErr != nil {
					t.Fatalf("应成功，实际 %v", testErr)
				}
				return
			}
			if testErr == nil || testErr.Error() != tc.message {
				t.Fatalf("错误 = %v，期望 %q", testErr, tc.message)
			}
		})
	}
}

// TestHDDolbyTestSiteBranches 覆盖 HDDolby 探测的数据容器判定。
func TestHDDolbyTestSiteBranches(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		message string
	}{
		{name: "对象数据", body: `{"status":0,"data":{"user_id":1}}`},
		{name: "数组数据", body: `{"status":0,"data":[{"user_id":1}]}`},
		{name: "状态非零", body: `{"status":1,"data":{"user_id":1}}`, message: "API Key 已失效"},
		{name: "数据为空", body: `{"status":0,"data":{}}`, message: "API Key 已失效"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			endpoint := probeServer(t, func(w http.ResponseWriter, r *http.Request) {
				if cookie := r.Header.Get("Cookie"); cookie != "" {
					t.Fatalf("探测请求不应携带 Cookie: %q", cookie)
				}
				_, _ = w.Write([]byte(tc.body))
			})
			adapter, err := NewHDDolbyAdapter(newIndexerHTTPClient(t, 1), 14, "HDDolby", endpoint, map[string]any{
				"api_url": endpoint, "apikey": "fixture-key", "cookie": "saved=1",
			})
			if err != nil {
				t.Fatalf("构造适配器: %v", err)
			}
			testErr := adapter.TestSite(context.Background())
			if tc.message == "" {
				if testErr != nil {
					t.Fatalf("应成功，实际 %v", testErr)
				}
				return
			}
			if testErr == nil || testErr.Error() != tc.message {
				t.Fatalf("错误 = %v，期望 %q", testErr, tc.message)
			}
		})
	}
}

// TestRousiTestSiteBranches 覆盖 Rousi 探测的状态码判定。
func TestRousiTestSiteBranches(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		message string
	}{
		{name: "有效", body: `{"code":0,"data":{"id":1}}`},
		{name: "Passkey 失效", body: `{"code":7,"data":{"id":1}}`, message: "Passkey 已失效"},
		{name: "数据为空", body: `{"code":0,"data":{}}`, message: "Passkey 已失效"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			endpoint := probeServer(t, func(w http.ResponseWriter, r *http.Request) {
				if !strings.Contains(r.URL.Query().Get("include_fields[user]"), "seeding_leeching_data") {
					t.Fatalf("缺少查询参数: %s", r.URL.RawQuery)
				}
				_, _ = w.Write([]byte(tc.body))
			})
			adapter, err := NewRousiAdapter(newIndexerHTTPClient(t, 1), 15, "Rousi", endpoint, map[string]any{
				"apikey": "fixture-key",
			})
			if err != nil {
				t.Fatalf("构造适配器: %v", err)
			}
			testErr := adapter.TestSite(context.Background())
			if tc.message == "" {
				if testErr != nil {
					t.Fatalf("应成功，实际 %v", testErr)
				}
				return
			}
			if testErr == nil || testErr.Error() != tc.message {
				t.Fatalf("错误 = %v，期望 %q", testErr, tc.message)
			}
		})
	}
}

// TestYemaTestSiteBranches 覆盖 Yema 探测的成功标志判定。
func TestYemaTestSiteBranches(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		message string
	}{
		{name: "有效", body: `{"success":true,"data":{"id":1}}`},
		{name: "AuthKey 失效", body: `{"success":false,"data":{"id":1}}`, message: "API AuthKey 已失效"},
		{name: "缺少成功标志", body: `{"data":{"id":1}}`, message: "API AuthKey 已失效"},
		{name: "数据为空", body: `{"success":true,"data":{}}`, message: "API AuthKey 已失效"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			endpoint := probeServer(t, func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(tc.body))
			})
			adapter, err := NewYemaAdapter(newIndexerHTTPClient(t, 1), 16, "Yema", endpoint, map[string]any{
				"apikey": "fixture-key",
			})
			if err != nil {
				t.Fatalf("构造适配器: %v", err)
			}
			testErr := adapter.TestSite(context.Background())
			if tc.message == "" {
				if testErr != nil {
					t.Fatalf("应成功，实际 %v", testErr)
				}
				return
			}
			if testErr == nil || testErr.Error() != tc.message {
				t.Fatalf("错误 = %v，期望 %q", testErr, tc.message)
			}
		})
	}
}

// TestProbeHelpersBranches 覆盖探测辅助函数。
func TestProbeHelpersBranches(t *testing.T) {
	if _, err := decodeProbeJSON([]byte("   ")); err == nil ||
		!strings.Contains(err.Error(), "站点响应为空") {
		t.Fatalf("空响应 = %v", err)
	}
	if _, err := decodeProbeJSON([]byte("[]")); err == nil ||
		!strings.Contains(err.Error(), "站点响应格式无效") {
		t.Fatalf("非对象响应 = %v", err)
	}
	document, err := decodeProbeJSON([]byte(`{"code":0}`))
	if err != nil || fmt.Sprint(document["code"]) != "0" {
		t.Fatalf("合法响应 = %+v（%v）", document, err)
	}

	for _, value := range []any{0, "0", " 0 "} {
		if !probeZero(value) {
			t.Fatalf("%v 应为零值", value)
		}
	}
	for _, value := range []any{nil, 1, "1", ""} {
		if probeZero(value) {
			t.Fatalf("%v 不应为零值", value)
		}
	}

	if !probeDataObject(map[string]any{"a": 1}) || probeDataObject(map[string]any{}) ||
		probeDataObject([]any{1}) {
		t.Fatal("probeDataObject 判定错误")
	}
	if !probeDataContainer(map[string]any{"a": 1}) || !probeDataContainer([]any{1}) ||
		probeDataContainer([]any{}) || probeDataContainer(nil) {
		t.Fatal("probeDataContainer 判定错误")
	}

	// selectionAttributeContains：属性缺失、大小写、多个元素。
	document2, err := goquery.NewDocumentFromReader(strings.NewReader(`<html><body>
		<a href="/a">a</a>
		<a href="/LogOut">b</a>
		<form action="/usercp"></form>
	</body></html>`))
	if err != nil {
		t.Fatalf("解析 HTML: %v", err)
	}
	if !selectionAttributeContains(document2.Find("a"), "href", "logout") {
		t.Fatal("应匹配大小写不同的 logout")
	}
	if selectionAttributeContains(document2.Find("a"), "href", "mybonus") {
		t.Fatal("不应匹配 mybonus")
	}
	if !selectionAttributeContains(document2.Find("form"), "action", "usercp") {
		t.Fatal("应匹配表单 action")
	}
	if selectionAttributeContains(document2.Find("div"), "href", "logout") {
		t.Fatal("空集合不应匹配")
	}
}

package indexer

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// TestValidateHeadersBranches 覆盖站点请求头的拒绝理由。
func TestValidateHeadersBranches(t *testing.T) {
	if err := validateHeaders(nil); err != nil {
		t.Fatalf("空请求头: %v", err)
	}
	if err := validateHeaders(map[string]string{"X-A": "v"}); err != nil {
		t.Fatalf("合法请求头: %v", err)
	}
	cases := []struct {
		headers map[string]string
		message string
	}{
		{headers: map[string]string{"  ": "v"}, message: "站点请求头名称无效"},
		{headers: map[string]string{"X-Bad\r\nX-Injected": "v"}, message: "站点请求头名称无效"},
		{headers: map[string]string{"host": "v"}, message: "站点请求头名称无效"},
		{headers: map[string]string{"X-A": "v\r\nX-Injected: 1"}, message: "站点请求头值无效"},
	}
	for _, tc := range cases {
		err := validateHeaders(tc.headers)
		if err == nil || err.Error() != tc.message {
			t.Fatalf("%v = %v，期望 %q", tc.headers, err, tc.message)
		}
	}
}

// TestValidateBaseURLBranches 覆盖站点地址的协议、凭据与内网限制。
func TestValidateBaseURLBranches(t *testing.T) {
	cases := []struct {
		value        string
		allowPrivate bool
		message      string
	}{
		{value: "", message: "站点地址必须是 HTTP 或 HTTPS URL"},
		{value: "ftp://index.example.com", message: "站点地址必须是 HTTP 或 HTTPS URL"},
		{value: "index.example.com", message: "站点地址必须是 HTTP 或 HTTPS URL"},
		{value: "https://user:pass@index.example.com", message: "站点地址不允许内嵌用户信息"},
		{value: "https://localhost", message: "站点地址不允许使用 localhost"},
		{value: "https://sub.localhost", message: "站点地址不允许使用 localhost"},
		{value: "https://10.0.0.1", message: "站点地址不允许使用受保护网络"},
		{value: "https://127.0.0.1", message: "站点地址不允许使用受保护网络"},
	}
	for _, tc := range cases {
		if _, err := validateBaseURL(tc.value, tc.allowPrivate); err == nil || err.Error() != tc.message {
			t.Fatalf("%q = %v，期望 %q", tc.value, err, tc.message)
		}
	}

	// 放开内网限制后私有地址可用，公网地址始终可用。
	if _, err := validateBaseURL("http://127.0.0.1:8080", true); err != nil {
		t.Fatalf("允许内网: %v", err)
	}
	parsed, err := validateBaseURL(" HTTPS://Index.Example.com/tracker/ ", false)
	if err != nil {
		t.Fatalf("公网地址: %v", err)
	}
	if parsed.Host != "Index.Example.com" {
		t.Fatalf("解析结果 = %+v", parsed)
	}
}

// TestBlockedIPBranches 覆盖受保护网段判定。
func TestBlockedIPBranches(t *testing.T) {
	blocked := []string{
		"10.1.2.3", "172.16.0.1", "192.168.1.1", "127.0.0.1", "169.254.1.1",
		"224.0.0.1", "0.0.0.0", "100.64.0.1", "198.18.0.1", "::1", "fe80::1",
	}
	for _, raw := range blocked {
		if !blockedIP(net.ParseIP(raw)) {
			t.Fatalf("%s 应被判定为受保护地址", raw)
		}
	}
	for _, raw := range []string{"8.8.8.8", "1.1.1.1", "203.0.113.5"} {
		if blockedIP(net.ParseIP(raw)) {
			t.Fatalf("%s 不应被判定为受保护地址", raw)
		}
	}
}

// TestResolveSiteURLBranches 覆盖站点链接的相对解析与跨主机拒绝。
func TestResolveSiteURLBranches(t *testing.T) {
	base, err := url.Parse("https://index.example.com/tracker")
	if err != nil {
		t.Fatalf("解析基准地址: %v", err)
	}
	if resolved, err := resolveSiteURL(base, "   "); err != nil || resolved != "" {
		t.Fatalf("空链接 = %q（%v）", resolved, err)
	}
	resolved, err := resolveSiteURL(base, "/api/search")
	if err != nil || resolved != "https://index.example.com/api/search" {
		t.Fatalf("相对链接 = %q（%v）", resolved, err)
	}
	if _, err := resolveSiteURL(base, "https://other.example.com/x"); err == nil ||
		!strings.Contains(err.Error(), "不同主机") {
		t.Fatalf("跨主机 = %v", err)
	}
	if _, err := resolveSiteURL(base, "ftp://index.example.com/x"); err == nil ||
		!strings.Contains(err.Error(), "协议不受支持") {
		t.Fatalf("非法协议 = %v", err)
	}
	if _, err := resolveSiteURL(base, "http://index.example.com/x"); err == nil ||
		!strings.Contains(err.Error(), "不同主机") {
		t.Fatalf("协议降级 = %v", err)
	}
	if _, err := resolveSiteURL(base, "://bad"); err == nil ||
		!strings.Contains(err.Error(), "链接无效") {
		t.Fatalf("非法链接 = %v", err)
	}
}

// TestSameHostRedirectBranches 覆盖重定向策略。
func TestSameHostRedirectBranches(t *testing.T) {
	request := &http.Request{URL: &url.URL{Scheme: "https", Host: "index.example.com"}}
	if err := sameHostRedirect(request, nil); err != nil {
		t.Fatalf("首次重定向: %v", err)
	}
	same := &http.Request{URL: &url.URL{Scheme: "https", Host: "INDEX.example.com"}}
	via := []*http.Request{{URL: &url.URL{Scheme: "https", Host: "index.example.com"}}}
	if err := sameHostRedirect(same, via); err != nil {
		t.Fatalf("同主机重定向: %v", err)
	}
	other := &http.Request{URL: &url.URL{Scheme: "https", Host: "other.example.com"}}
	if err := sameHostRedirect(other, via); err == nil ||
		!strings.Contains(err.Error(), "不同主机") {
		t.Fatalf("跨主机重定向 = %v", err)
	}
	over := []*http.Request{{}, {}, {}}
	if err := sameHostRedirect(request, over); err == nil ||
		!strings.Contains(err.Error(), "次数超过限制") {
		t.Fatalf("重定向次数 = %v", err)
	}
}

// TestSignInFormBranches 覆盖登录表单提交的守卫与 Cookie 收集。
func TestSignInFormBranches(t *testing.T) {
	if _, _, err := (*HTTPClient)(nil).signInForm(context.Background(), "https://x", http.Header{}, url.Values{}); err == nil ||
		!strings.Contains(err.Error(), "站点 HTTP 客户端未初始化") {
		t.Fatalf("空客户端 = %v", err)
	}

	client := newIndexerHTTPClient(t, 1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := client.signInForm(ctx, "https://index.example.com/login", http.Header{}, url.Values{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("取消上下文 = %v", err)
	}
	if _, _, err := client.signInForm(context.Background(), "://bad", http.Header{}, url.Values{}); err == nil ||
		!strings.Contains(err.Error(), "登录地址无效") {
		t.Fatalf("非法地址 = %v", err)
	}

	limited := newIndexerHTTPClient(t, 1)
	limited.maxRequestBytes = 4
	if _, _, err := limited.signInForm(context.Background(), "https://index.example.com/login",
		http.Header{}, url.Values{"username": {"alice"}}); err == nil ||
		!strings.Contains(err.Error(), "超过大小限制") {
		t.Fatalf("超长请求 = %v", err)
	}

	endpoint := probeServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("方法 = %s", r.Method)
		}
		w.Header().Set("Set-Cookie", "session=fixture; Path=/")
		w.WriteHeader(http.StatusFound)
	})
	cookies, status, err := client.signInForm(context.Background(), endpoint,
		http.Header{"X-Test": {"1"}}, url.Values{"username": {"alice"}})
	if err != nil || status != http.StatusFound {
		t.Fatalf("登录表单 = %d（%v）", status, err)
	}
	if len(cookies) != 1 || cookies[0].Name != "session" {
		t.Fatalf("Cookie = %+v", cookies)
	}
}

package metadata

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestHTTPClientSetProxyRoutesThroughProxy 确认 SetProxy 真的把流量送到代理。
//
// 背景：本机 api.bgm.tv 直连不通（IPv4 超时、IPv6 TLS 握手失败），经代理 1.4s
// 就返回——V2 的 PROXY_HOST 正是干这个的，光把地址记下来不算数，要能换连接池。
func TestHTTPClientSetProxyRoutesThroughProxy(t *testing.T) {
	var mu sync.Mutex
	var targets []string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		targets = append(targets, r.URL.String())
		mu.Unlock()
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer proxy.Close()

	client, err := NewHTTPClient(2, 5*time.Second, 1<<20)
	if err != nil {
		t.Fatalf("创建客户端失败: %v", err)
	}
	defer client.CloseIdleConnections()

	if err := client.SetProxy(proxy.URL); err != nil {
		t.Fatalf("设置代理失败: %v", err)
	}
	var payload map[string]any
	// 域名故意不可解析：能成功说明请求确实走了代理，而不是本地 DNS 直连。
	err = client.DoJSON(context.Background(), http.MethodGet, "http://metadata.invalid/ping", nil, nil, &payload)
	if err != nil {
		t.Fatalf("经代理请求失败: %v", err)
	}
	if payload["ok"] != true {
		t.Fatalf("响应 = %#v", payload)
	}
	mu.Lock()
	got := append([]string(nil), targets...)
	mu.Unlock()
	if len(got) != 1 || !strings.Contains(got[0], "metadata.invalid/ping") {
		t.Fatalf("代理收到的目标 = %#v", got)
	}

	// 清空代理要能回落到直连/环境变量，非法协议要明确报错而不是静默直连。
	if err := client.SetProxy(""); err != nil {
		t.Fatalf("清空代理失败: %v", err)
	}
	for _, invalid := range []string{"ftp://127.0.0.1:7890", "127.0.0.1:7890", "http://"} {
		if err := client.SetProxy(invalid); err == nil {
			t.Fatalf("非法代理地址应报错: %q", invalid)
		}
	}
}

// TestProxyFuncAcceptsSupportedSchemes 覆盖支持的代理协议。
func TestProxyFuncAcceptsSupportedSchemes(t *testing.T) {
	for _, raw := range []string{
		"http://127.0.0.1:7890",
		"https://proxy.example:8443",
		"socks5://127.0.0.1:1080",
		"socks5h://127.0.0.1:1080",
		"  http://127.0.0.1:10808  ",
	} {
		if _, err := proxyFunc(raw); err != nil {
			t.Fatalf("proxyFunc(%q) 不应报错: %v", raw, err)
		}
	}
	if _, err := proxyFunc("   "); err != nil {
		t.Fatalf("空代理应回落到环境变量: %v", err)
	}
}

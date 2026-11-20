package indexer

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 本文件覆盖站点发现/探测里「凭证透传」与「请求失败」两条分支。

func TestSiteDiscoveryPropagatesCredentials(t *testing.T) {
	var pageCookie, pageAgent, pageAccept, iconCookie string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			pageCookie = r.Header.Get("Cookie")
			pageAgent = r.Header.Get("User-Agent")
			pageAccept = r.Header.Get("Accept")
			_, _ = w.Write([]byte(`<html><head>` +
				`<link rel="stylesheet" href="/theme.css">` +
				`<link rel="shortcut" href="/fav.ico">` +
				`<link rel="alternate" type="application/rss+xml" href="/feed.xml">` +
				`</head><body></body></html>`))
		case "/fav.ico":
			iconCookie = r.Header.Get("Cookie")
			_, _ = w.Write([]byte("icon-bytes"))
		case "/feed.xml":
			_, _ = w.Write([]byte(`<rss></rss>`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := newIndexerHTTPClient(t, 1)
	iconURL, iconData, err := client.DiscoverSiteFavicon(context.Background(), server.URL, "session=fixture", "agent/1.0")
	if err != nil {
		t.Fatalf("DiscoverSiteFavicon() error = %v", err)
	}
	if !strings.HasSuffix(iconURL, "/fav.ico") {
		t.Fatalf("图标地址 = %q", iconURL)
	}
	if iconData != base64.StdEncoding.EncodeToString([]byte("icon-bytes")) {
		t.Fatalf("图标数据 = %q", iconData)
	}
	if pageCookie != "session=fixture" || pageAgent != "agent/1.0" ||
		!strings.Contains(pageAccept, "text/html") || iconCookie != "session=fixture" {
		t.Fatalf("凭证未透传: cookie=%q agent=%q accept=%q iconCookie=%q",
			pageCookie, pageAgent, pageAccept, iconCookie)
	}

	feedURL, err := client.DiscoverSiteRSS(context.Background(), server.URL, "session=fixture", "agent/1.0")
	if err != nil || !strings.HasSuffix(feedURL, "/feed.xml") {
		t.Fatalf("DiscoverSiteRSS() = %q, %v", feedURL, err)
	}
}

func TestSiteTestHelpersPropagateCredentials(t *testing.T) {
	var cookie, agent string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie = r.Header.Get("Cookie")
		agent = r.Header.Get("User-Agent")
		_, _ = w.Write([]byte(`<html><head><title>登录</title></head><body></body></html>`))
	}))
	defer server.Close()

	client := newIndexerHTTPClient(t, 1)
	_ = client.TestSiteLoginPage(context.Background(), server.URL, "session=fixture", "agent/1.0")
	if cookie != "session=fixture" || agent != "agent/1.0" {
		t.Fatalf("登录页探测未透传凭证: cookie=%q agent=%q", cookie, agent)
	}

	cookie, agent = "", ""
	if err := client.TestSiteAvailability(context.Background(), server.URL, "session=fixture", "agent/1.0"); err != nil {
		t.Fatalf("TestSiteAvailability() error = %v", err)
	}
	if cookie != "session=fixture" || agent != "agent/1.0" {
		t.Fatalf("可用性探测未透传凭证: cookie=%q agent=%q", cookie, agent)
	}
}

func TestSiteDiscoveryReportsTransportFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	endpoint := server.URL
	server.Close()

	client := newIndexerHTTPClient(t, 1)
	if _, _, err := client.DiscoverSiteFavicon(context.Background(), endpoint, "", ""); err == nil {
		t.Fatal("首页请求失败时 DiscoverSiteFavicon 应返回错误")
	}
	if _, err := client.DiscoverSiteRSS(context.Background(), endpoint, "", ""); err == nil {
		t.Fatal("首页请求失败时 DiscoverSiteRSS 应返回错误")
	}
	if err := client.TestSiteAvailability(context.Background(), endpoint, "", ""); err == nil {
		t.Fatal("首页请求失败时 TestSiteAvailability 应返回错误")
	}
}

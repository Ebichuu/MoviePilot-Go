package indexer

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestNexusPHPAdapterSearchDownloadProbeUserDataAndRSS(t *testing.T) {
	t.Parallel()

	const searchHTML = `<!doctype html><html><body>
<table class="torrents">
  <tr><th>类型</th><th>标题</th><th>评论</th><th>时间</th><th>大小</th><th>做种</th><th>下载</th><th>完成</th></tr>
  <tr>
    <td><img alt="Movies" /></td>
    <td>
      <table class="torrentname"><tr><td class="embedded">
        <a title="Dune.Part.Two.2024.2160p.WEB-DL.H265" href="details.php?id=12345&hit=1"><b>Dune.Part.Two.2024.2160p</b></a>
        <img class="pro_free2up" alt="2X Free" />
        <img class="hitandrun" title="H&R" />
        <span class="tags">中字</span><span class="tags">官组</span>
        <a href="https://www.imdb.com/title/tt15239678/">IMDb</a>
        <br />沙丘2 4K 中文字幕
      </td></tr></table>
      <a href="download.php?id=12345">下载</a>
    </td>
    <td>3</td>
    <td><span title="2026-11-20 12:30:00">1小时前</span></td>
    <td>22.50 GB</td>
    <td>128</td>
    <td>14</td>
    <td>1,024</td>
  </tr>
</table></body></html>`

	const indexHTML = `<!doctype html><html><body>
<div id="info_block">
  欢迎回来, <a href="userdetails.php?id=9527" class="PowerUser_Name">Ebichuu</a>
  魔力值: 12,345.50
  分享率: 3.45 上传量: 2.50 TB 下载量: 500.00 GB
  <span><img class="arrowup" /> 42 <img class="arrowdown" /> 3</span>
  <a href="messages.php">未读消息(2)</a>
</div></body></html>`

	const rssXML = `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0"><channel><title>NexusRSS</title>
  <item>
    <title>Dune.Part.Two.2024.2160p.WEB-DL</title>
    <guid>nexus-12345</guid>
    <link>/details.php?id=12345</link>
    <pubDate>Fri, 20 Nov 2026 12:30:00 +0000</pubDate>
    <enclosure url="/download.php?id=12345" length="24159191040" type="application/x-bittorrent" />
  </item>
</channel></rss>`

	var gotSearchArea string
	var gotPasskey string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Cookie") != "c_secure_uid=abc" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/torrents.php":
			gotSearchArea = r.URL.Query().Get("search_area")
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte(searchHTML))
		case "/index.php":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte(indexHTML))
		case "/download.php":
			gotPasskey = r.URL.Query().Get("passkey")
			w.Header().Set("Content-Type", "application/x-bittorrent")
			_, _ = w.Write([]byte("d8:announce1:xe"))
		case "/torrentrss.php":
			if r.Header.Get("If-None-Match") == `"v1"` {
				w.WriteHeader(http.StatusNotModified)
				return
			}
			w.Header().Set("ETag", `"v1"`)
			w.Header().Set("Content-Type", "application/rss+xml")
			_, _ = w.Write([]byte(rssXML))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	httpClient, err := NewHTTPClient(2, 5*time.Second, 1<<20)
	if err != nil {
		t.Fatalf("NewHTTPClient() error = %v", err)
	}
	httpClient.SetAllowPrivateHosts(true)

	adapterAny, err := NewAdapterFromConfig(httpClient, 7, "DemoNexus", server.URL, "nexusphp", map[string]any{
		"cookie":  "c_secure_uid=abc",
		"passkey": "pk-secret",
		"rss":     "/torrentrss.php",
	})
	if err != nil {
		t.Fatalf("NewAdapterFromConfig() error = %v", err)
	}
	adapter, ok := adapterAny.(*NexusPHPAdapter)
	if !ok {
		t.Fatalf("adapter type = %T, want *NexusPHPAdapter", adapterAny)
	}

	ctx := context.Background()
	if err := adapter.TestSite(ctx); err != nil {
		t.Fatalf("TestSite() error = %v", err)
	}

	resources, err := adapter.Search(ctx, Query{Keyword: "tt15239678", Limit: 10})
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if gotSearchArea != "4" {
		t.Fatalf("search_area = %q, want 4 for IMDb query", gotSearchArea)
	}
	if len(resources) != 1 {
		t.Fatalf("len(resources) = %d, want 1", len(resources))
	}
	res := resources[0]
	if res.Title != "Dune.Part.Two.2024.2160p.WEB-DL.H265" || res.TorrentID != "12345" {
		t.Fatalf("unexpected resource identity: %+v", res)
	}
	if res.IMDBID != "tt15239678" || !res.HitAndRun || res.Seeders != 128 || res.Peers != 14 || res.Grabs != 1024 {
		t.Fatalf("unexpected resource metadata: %+v", res)
	}
	if res.DownloadVolumeFactor == nil || *res.DownloadVolumeFactor != 0 || res.UploadVolumeFactor == nil || *res.UploadVolumeFactor != 2 {
		t.Fatalf("unexpected promo factors: down=%v up=%v", res.DownloadVolumeFactor, res.UploadVolumeFactor)
	}
	if res.SizeBytes <= 0 || len(res.Labels) != 2 || res.Category != "Movies" {
		t.Fatalf("unexpected size/labels/category: size=%d labels=%v cat=%q", res.SizeBytes, res.Labels, res.Category)
	}

	resolved, err := adapter.ResolveDownload(ctx, res.DownloadID)
	if err != nil {
		t.Fatalf("ResolveDownload() error = %v", err)
	}
	if string(resolved.Body) != "d8:announce1:xe" || gotPasskey != "pk-secret" {
		t.Fatalf("ResolveDownload() body=%q passkey=%q", resolved.Body, gotPasskey)
	}

	userData, err := adapter.FetchSiteUserData(ctx)
	if err != nil {
		t.Fatalf("FetchSiteUserData() error = %v", err)
	}
	if userData.Username == nil || *userData.Username != "Ebichuu" || userData.UserID != 9527 {
		t.Fatalf("unexpected user identity: %+v", userData)
	}
	if userData.Ratio != 3.45 || userData.Bonus != 12345.5 || userData.Upload <= 0 || userData.Download <= 0 {
		t.Fatalf("unexpected user stats: %+v", userData)
	}
	if userData.Seeding != 42 || userData.Leeching != 3 || userData.MessageUnread != 2 {
		t.Fatalf("unexpected seeding/messages: %+v", userData)
	}

	if adapter.RSSCursorKey() == "" {
		t.Fatal("expected non-empty RSSCursorKey")
	}
	rssResp, err := adapter.FetchRSS(ctx, RSSRequest{Limit: 10})
	if err != nil {
		t.Fatalf("FetchRSS() error = %v", err)
	}
	if len(rssResp.Items) != 1 || rssResp.ETag != `"v1"` {
		t.Fatalf("unexpected RSS response: %+v", rssResp)
	}
	notMod, err := adapter.FetchRSS(ctx, RSSRequest{ETag: `"v1"`, Limit: 10})
	if err != nil || !notMod.NotModified {
		t.Fatalf("expected 304 NotModified, got %+v err=%v", notMod, err)
	}
}

func TestNexusPHPRejectsLoginPage(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<form action="takelogin.php"><input name="username"><input name="password"></form>`))
	}))
	defer server.Close()

	httpClient, _ := NewHTTPClient(1, 5*time.Second, 1<<20)
	httpClient.SetAllowPrivateHosts(true)
	adapter, err := NewNexusPHPAdapter(httpClient, 1, "Expired", server.URL, map[string]any{"cookie": "bad=1"})
	if err != nil {
		t.Fatalf("NewNexusPHPAdapter() error = %v", err)
	}
	if err := adapter.TestSite(context.Background()); err == nil || !strings.Contains(err.Error(), "Cookie") {
		t.Fatalf("TestSite() = %v, want cookie failure", err)
	}
}

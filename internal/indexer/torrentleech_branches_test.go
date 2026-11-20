package indexer

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
)

// TestNewTorrentLeechAdapterValidation 覆盖 TorrentLeech 构造期的配置校验。
func TestNewTorrentLeechAdapterValidation(t *testing.T) {
	client := newIndexerHTTPClient(t, 1)
	if _, err := NewTorrentLeechAdapter(nil, 1, "x", "https://index.example.com", nil); err == nil ||
		!strings.Contains(err.Error(), "站点 HTTP 客户端不能为空") {
		t.Fatalf("空客户端 = %v", err)
	}
	if _, err := NewTorrentLeechAdapter(client, 1, "x", "ftp://index.example.com", nil); err == nil {
		t.Fatal("非法站点地址应报错")
	}
	if _, err := NewTorrentLeechAdapter(client, 1, "x", "https://index.example.com", nil); err == nil ||
		!strings.Contains(err.Error(), "未配置 Cookie") {
		t.Fatalf("缺 Cookie = %v", err)
	}
	if _, err := NewTorrentLeechAdapter(client, 1, "x", "https://index.example.com", map[string]any{
		"cookie": strings.Repeat("c", 64<<10+1),
	}); err == nil || !strings.Contains(err.Error(), "Cookie 无效") {
		t.Fatalf("超长 Cookie = %v", err)
	}
	if _, err := NewTorrentLeechAdapter(client, 1, "x", "https://index.example.com", map[string]any{
		"cookie": "c\r\nX-Injected: 1",
	}); err == nil || !strings.Contains(err.Error(), "Cookie 无效") {
		t.Fatalf("换行 Cookie = %v", err)
	}
	if _, err := NewTorrentLeechAdapter(client, 1, "x", "https://index.example.com", map[string]any{
		"cookie": "c", "ua": strings.Repeat("u", 1025),
	}); err == nil || !strings.Contains(err.Error(), "User-Agent 无效") {
		t.Fatalf("超长 UA = %v", err)
	}
	if _, err := NewTorrentLeechAdapter(client, 1, "x", "https://index.example.com", map[string]any{
		"cookie": "c", "ua": "bad\r\nua",
	}); err == nil || !strings.Contains(err.Error(), "User-Agent 无效") {
		t.Fatalf("换行 UA = %v", err)
	}

	adapter, err := NewTorrentLeechAdapter(client, 9, "  ", "https://index.example.com", map[string]any{
		"cookie": "session=abc", "ua": "fixture/1",
	})
	if err != nil {
		t.Fatalf("合法配置: %v", err)
	}
	if adapter.name != "TorrentLeech" || adapter.headers.Get("Cookie") != "session=abc" ||
		adapter.headers.Get("User-Agent") != "fixture/1" {
		t.Fatalf("适配器字段 = %+v", adapter)
	}
	if adapter.Name() != "TorrentLeech" || adapter.SiteID() != 9 || adapter.ResolverKind() != "torrentleech" {
		t.Fatal("适配器元信息错误")
	}
}

// TestTorrentLeechSearchBranches 覆盖中文短路、分页路径与条目清洗。
func TestTorrentLeechSearchBranches(t *testing.T) {
	requests := 0
	endpoint := probeServer(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		if strings.Contains(r.URL.Path, "/query/") {
			_, _ = w.Write([]byte(`{"torrentList":[]}`))
			return
		}
		_, _ = w.Write([]byte(`{"torrentList":[
			"skip",
			{"fid":"","name":"缺 fid","filename":"a.torrent"},
			{"fid":"1","name":"","filename":"a.torrent"},
			{"fid":"2","name":"缺文件名"},
			{"fid":"3","name":"超长文件名","filename":"` + strings.Repeat("f", 513) + `"},
			{"fid":"4","name":"保留","filename":"movie.torrent","size":4096,"seeders":5,
				"leechers":1,"completed":3,"download_multiplier":0.5,"imdbID":"tt123",
				"addedTimestamp":"2026-03-04 05:06:07"}
		]}`))
	})
	adapter, err := NewTorrentLeechAdapter(newIndexerHTTPClient(t, 1), 9, "TL", endpoint, map[string]any{
		"cookie": "session=abc",
	})
	if err != nil {
		t.Fatalf("构造适配器: %v", err)
	}

	// 含汉字的关键字直接短路，不发请求。
	short, err := adapter.Search(context.Background(), Query{Keyword: "某剧"})
	if err != nil || len(short) != 0 {
		t.Fatalf("中文关键字 = %+v（%v）", short, err)
	}
	if requests != 0 {
		t.Fatalf("中文关键字不应发请求，实际 %d 次", requests)
	}

	resources, err := adapter.Search(context.Background(), Query{Keyword: "", Page: 1, Limit: 10})
	if err != nil {
		t.Fatalf("搜索失败: %v", err)
	}
	if requests != 1 {
		t.Fatalf("分页搜索请求数 = %d", requests)
	}
	if len(resources) != 1 {
		t.Fatalf("资源条数 = %d", len(resources))
	}
	resource := resources[0]
	if resource.Title != "保留" || resource.SizeBytes != 4096 || resource.Seeders != 5 ||
		resource.Peers != 1 || resource.Grabs != 3 || resource.IMDBID != "tt123" {
		t.Fatalf("资源字段 = %+v", resource)
	}
	if resource.DownloadVolumeFactor == nil || *resource.DownloadVolumeFactor != 0.5 {
		t.Fatalf("下载系数 = %+v", resource.DownloadVolumeFactor)
	}
	if !strings.Contains(resource.DetailURL, "/torrent/4") ||
		!strings.Contains(resource.DownloadURL, "/api/v1/resource/download/9/") {
		t.Fatalf("链接字段 = %+v", resource)
	}
	if resource.PublishedAt.IsZero() {
		t.Fatalf("发布时间 = %+v", resource.PublishedAt)
	}
	// 下载标识应能还原出 fid 与文件名。
	if fid, filename, err := decodeTorrentLeechDownloadID(resource.DownloadID); err != nil ||
		fid != "4" || filename != "movie.torrent" {
		t.Fatalf("下载标识 = %q/%q（%v）", fid, filename, err)
	}

	// 关键字走 query 路径；结果非数组报错。
	if _, err := adapter.Search(context.Background(), Query{Keyword: "dune", Page: -1}); err != nil {
		t.Fatalf("关键字搜索: %v", err)
	}
	brokenEndpoint := probeServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"torrentList":{}}`))
	})
	broken, err := NewTorrentLeechAdapter(newIndexerHTTPClient(t, 1), 9, "TL", brokenEndpoint, map[string]any{
		"cookie": "session=abc",
	})
	if err != nil {
		t.Fatalf("构造适配器: %v", err)
	}
	if _, err := broken.Search(context.Background(), Query{Keyword: "dune"}); err == nil ||
		!strings.Contains(err.Error(), "搜索结果不是数组") {
		t.Fatalf("结果非数组 = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := adapter.Search(ctx, Query{Keyword: ""}); !errors.Is(err, context.Canceled) {
		t.Fatalf("取消上下文 = %v", err)
	}
}

// TestTorrentLeechResolveDownloadBranches 覆盖下载标识解码与下载。
func TestTorrentLeechResolveDownloadBranches(t *testing.T) {
	endpoint := probeServer(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/download/4/movie.torrent") {
			t.Fatalf("下载路径 = %s", r.URL.Path)
		}
		_, _ = w.Write([]byte("d4:infod4:name2:tlee"))
	})
	adapter, err := NewTorrentLeechAdapter(newIndexerHTTPClient(t, 1), 9, "TL", endpoint, map[string]any{
		"cookie": "session=abc",
	})
	if err != nil {
		t.Fatalf("构造适配器: %v", err)
	}
	download, err := adapter.ResolveDownload(context.Background(), encodeTorrentLeechDownloadID("4", "movie.torrent"))
	if err != nil || string(download.Body) != "d4:infod4:name2:tlee" ||
		download.Filename != "movie.torrent" || download.ContentType != "application/x-bittorrent" {
		t.Fatalf("下载结果 = %+v（%v）", download, err)
	}
	if _, err := adapter.ResolveDownload(context.Background(), "!!!"); err == nil ||
		!strings.Contains(err.Error(), "下载标识无效") {
		t.Fatalf("非法标识 = %v", err)
	}
}

// TestDecodeTorrentLeechDownloadIDBranches 覆盖下载标识的各类非法形态。
func TestDecodeTorrentLeechDownloadIDBranches(t *testing.T) {
	cases := []string{
		"",
		strings.Repeat("A", 1025),
		"!!!not-base64",
		encodeTorrentLeechDownloadID("abc", "x.torrent"),
		encodeTorrentLeechDownloadID("4", ""),
		encodeTorrentLeechDownloadID("4", strings.Repeat("f", 513)),
		encodeTorrentLeechDownloadID("4", "bad\r\nname"),
	}
	for _, value := range cases {
		if _, _, err := decodeTorrentLeechDownloadID(value); err == nil {
			t.Fatalf("%q 应判定为非法标识", value)
		}
	}
	// 不含分隔符的裸 base64 也非法。
	if _, _, err := decodeTorrentLeechDownloadID(encodeTorrentLeechDownloadID("", "")); err == nil {
		t.Fatal("空 fid 与空文件名应非法")
	}
}

// TestContainsHanBranches 覆盖汉字检测。
func TestContainsHanBranches(t *testing.T) {
	if !containsHan("dune 沙丘") || containsHan("dune") || containsHan("") {
		t.Fatal("containsHan 判定错误")
	}
}

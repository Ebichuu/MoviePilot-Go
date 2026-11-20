package indexer

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

// TestYemaSearchBranches 覆盖 YemaPT 搜索的分页载荷、失败码与条目清洗。
func TestYemaSearchBranches(t *testing.T) {
	var payload string
	endpoint := probeServer(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		payload = string(body)
		_, _ = w.Write([]byte(`{"success":true,"data":[
			"skip",
			{"id":"","showName":"缺 ID"},
			{"id":"1","showName":""},
			{"id":"2","showName":"保留","fileSize":2048,"seedNum":4,"leechNum":2,"completedNum":7,
				"downloadPromotion":"free","uploadPromotion":"double_upload",
				"categoryId":5,"tagList":["1","6"],"listingTime":"2026-02-03 04:05:06",
				"downloadPromotionEndTime":"2026-02-04 00:00:00","hrPunishEnable":true},
			{"id":"3","showName":"半折","downloadPromotion":"half","uploadPromotion":"one_half","categoryId":4}
		]}`))
	})
	adapter, err := NewYemaAdapter(newIndexerHTTPClient(t, 1), 21, "YemaPT", endpoint, map[string]any{
		"apikey": "k",
	})
	if err != nil {
		t.Fatalf("构造适配器: %v", err)
	}
	resources, err := adapter.Search(context.Background(), Query{Keyword: "某剧", Page: 1, Limit: 10})
	if err != nil {
		t.Fatalf("搜索失败: %v", err)
	}
	if !strings.Contains(payload, `"current":2`) || !strings.Contains(payload, "某剧") {
		t.Fatalf("请求载荷 = %s", payload)
	}
	if len(resources) != 2 {
		t.Fatalf("资源条数 = %d", len(resources))
	}
	first := resources[0]
	if first.Title != "保留" || first.SizeBytes != 2048 || first.Seeders != 4 || first.Peers != 2 ||
		first.Grabs != 7 || !first.HitAndRun {
		t.Fatalf("首条字段 = %+v", first)
	}
	if first.Category != "电视剧" || len(first.Labels) != 2 || first.Labels[0] != "禁转" {
		t.Fatalf("分类/标签 = %+v", first)
	}
	if first.DownloadVolumeFactor == nil || *first.DownloadVolumeFactor != 0 ||
		first.UploadVolumeFactor == nil || *first.UploadVolumeFactor != 2 {
		t.Fatalf("促销系数 = %+v", first)
	}
	if first.PublishedAt.IsZero() || first.FreeDate.IsZero() {
		t.Fatalf("时间字段 = %+v", first)
	}
	if !strings.Contains(first.DetailURL, "/#/torrent/detail/2/") ||
		!strings.Contains(first.DownloadURL, "/api/v1/resource/download/21/2") {
		t.Fatalf("链接字段 = %+v", first)
	}
	second := resources[1]
	if !strings.Contains(second.Title, "半折") || second.Category != "电影" {
		t.Fatalf("次条字段 = %+v", second)
	}
	if second.DownloadVolumeFactor == nil || *second.DownloadVolumeFactor != 0.5 ||
		second.UploadVolumeFactor == nil || *second.UploadVolumeFactor != 1.5 {
		t.Fatalf("半折促销 = %+v", second)
	}

	// 空关键字不进载荷；业务失败与结果非数组都报错。
	_, _ = adapter.Search(context.Background(), Query{Keyword: "  "})
	if strings.Contains(payload, "keyword") {
		t.Fatalf("空关键字不应进载荷: %s", payload)
	}
	for _, body := range []string{`{"success":false}`, `{"success":true,"data":{}}`} {
		brokenEndpoint := probeServer(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(body))
		})
		broken, err := NewYemaAdapter(newIndexerHTTPClient(t, 1), 21, "YemaPT", brokenEndpoint, map[string]any{
			"apikey": "k",
		})
		if err != nil {
			t.Fatalf("构造适配器: %v", err)
		}
		if _, searchErr := broken.Search(context.Background(), Query{Keyword: "某剧"}); searchErr == nil {
			t.Fatalf("%s 应报错", body)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := adapter.Search(ctx, Query{Keyword: "某剧"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("取消上下文 = %v", err)
	}
}

// TestYemaResolveDownloadBranches 覆盖下载凭证生成与下载。
func TestYemaResolveDownloadBranches(t *testing.T) {
	endpoint := probeServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "generateDownloadKey"):
			_, _ = w.Write([]byte(`{"success":true,"data":"fixture-token"}`))
		case strings.Contains(r.URL.Path, "download1"):
			if r.URL.Query().Get("token") != "fixture-token" {
				t.Fatalf("token 参数 = %q", r.URL.Query().Get("token"))
			}
			_, _ = w.Write([]byte("d4:infod4:name5:yemeee"))
		default:
			t.Fatalf("未预期路径 = %s", r.URL.Path)
		}
	})
	adapter, err := NewYemaAdapter(newIndexerHTTPClient(t, 1), 21, "YemaPT", endpoint, map[string]any{
		"apikey": "k",
	})
	if err != nil {
		t.Fatalf("构造适配器: %v", err)
	}

	if _, err := adapter.ResolveDownload(context.Background(), "abc"); err == nil ||
		!strings.Contains(err.Error(), "种子 ID 无效") {
		t.Fatalf("非法 ID = %v", err)
	}
	if _, err := adapter.ResolveDownload(context.Background(), "99999999999999999999"); err == nil ||
		!strings.Contains(err.Error(), "out of range") {
		t.Fatalf("超范围 ID = %v", err)
	}

	download, err := adapter.ResolveDownload(context.Background(), "42")
	if err != nil || string(download.Body) != "d4:infod4:name5:yemeee" ||
		download.Filename != "42.torrent" || download.ContentType != "application/x-bittorrent" {
		t.Fatalf("下载结果 = %+v（%v）", download, err)
	}

	for _, body := range []string{`{"success":false}`, `{"success":true,"data":""}`, `not json`} {
		brokenEndpoint := probeServer(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(body))
		})
		broken, err := NewYemaAdapter(newIndexerHTTPClient(t, 1), 21, "YemaPT", brokenEndpoint, map[string]any{
			"apikey": "k",
		})
		if err != nil {
			t.Fatalf("构造适配器: %v", err)
		}
		if _, downloadErr := broken.ResolveDownload(context.Background(), "42"); downloadErr == nil {
			t.Fatalf("%s 应报错", body)
		}
	}

	tooLong := probeServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"success":true,"data":"` + strings.Repeat("t", 4097) + `"}`))
	})
	broken, err := NewYemaAdapter(newIndexerHTTPClient(t, 1), 21, "YemaPT", tooLong, map[string]any{
		"apikey": "k",
	})
	if err != nil {
		t.Fatalf("构造适配器: %v", err)
	}
	if _, err := broken.ResolveDownload(context.Background(), "42"); err == nil ||
		!strings.Contains(err.Error(), "下载凭证无效") {
		t.Fatalf("超长凭证 = %v", err)
	}
}

// TestNewYemaAdapterValidatesConfig 覆盖构造期校验：空客户端、默认站点名、
// 非法站点地址、超长 AuthKey 与非法 User-Agent。
func TestNewYemaAdapterValidatesConfig(t *testing.T) {
	client := newIndexerHTTPClient(t, 1)
	cases := []struct {
		name    string
		client  *HTTPClient
		baseURL string
		config  map[string]any
		want    string
	}{
		{
			name:    "空客户端",
			client:  nil,
			baseURL: "https://yema.example",
			config:  map[string]any{"apikey": "key"},
			want:    "站点 HTTP 客户端不能为空",
		},
		{
			name:    "站点地址无效",
			client:  client,
			baseURL: "://bad",
			config:  map[string]any{"apikey": "key"},
			want:    "",
		},
		{
			name:    "AuthKey 超长",
			client:  client,
			baseURL: "https://yema.example",
			config:  map[string]any{"apikey": strings.Repeat("k", 4097)},
			want:    "超过长度限制",
		},
		{
			name:    "User-Agent 含换行",
			client:  client,
			baseURL: "https://yema.example",
			config:  map[string]any{"apikey": "key", "ua": "agent\r\nX-Injected: 1"},
			want:    "User-Agent 无效",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewYemaAdapter(tc.client, 9, "YemaPT", tc.baseURL, tc.config)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want 包含 %q", err, tc.want)
			}
		})
	}

	adapter, err := NewYemaAdapter(client, 9, "  ", "https://yema.example", map[string]any{"apikey": "key"})
	if err != nil {
		t.Fatalf("构造适配器: %v", err)
	}
	if adapter.Name() != "YemaPT" {
		t.Fatalf("空站点名应回退为 YemaPT，得到 %q", adapter.Name())
	}
}

// TestYemaResolveDownloadTransportFailure 覆盖凭证生成成功但下载请求失败的路径。
func TestYemaResolveDownloadTransportFailure(t *testing.T) {
	endpoint := probeServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"success":true,"data":"fixture-token"}`))
	})
	client := newIndexerHTTPClient(t, 1)
	client.client.Transport = &failAfterRequestsWith{
		base: http.DefaultTransport, after: 1, err: errors.New("下载接口不可用"),
	}
	adapter, err := NewYemaAdapter(client, 21, "YemaPT", endpoint, map[string]any{"apikey": "k"})
	if err != nil {
		t.Fatalf("构造适配器: %v", err)
	}
	if _, err := adapter.ResolveDownload(context.Background(), "42"); err == nil {
		t.Fatal("下载请求失败时应返回错误")
	}
}

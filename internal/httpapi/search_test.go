package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/moviepilot-go/moviepilot-go/internal/config"
	"github.com/moviepilot-go/moviepilot-go/internal/indexer"
)

type searchTestAdapter struct {
	id        int
	name      string
	resources []indexer.Resource
}

type downloadTestAdapter struct {
	searchTestAdapter
	download indexer.ResolvedDownload
	err      error
}

const validTorrentMetainfo = "d4:infod6:lengthi7e4:name7:fixture12:piece lengthi16384e6:pieces20:01234567890123456789ee"

func (a downloadTestAdapter) ResolveDownload(context.Context, string) (indexer.ResolvedDownload, error) {
	return a.download, a.err
}

func (a searchTestAdapter) SiteID() int  { return a.id }
func (a searchTestAdapter) Name() string { return a.name }

func (a searchTestAdapter) Search(context.Context, indexer.Query) ([]indexer.Resource, error) {
	return a.resources, nil
}

func TestSearchTitleStreamUsesCoordinatorAndV2Events(t *testing.T) {
	coordinator, err := indexer.NewCoordinator(2)
	if err != nil {
		t.Fatal(err)
	}
	deps := Dependencies{
		Search: coordinator,
		SearchAdapters: []indexer.Adapter{
			searchTestAdapter{id: 1, name: "alpha", resources: []indexer.Resource{{
				Title: "Show S01E01 1080p WEB-DL", DetailURL: "https://example.test/1",
				Description: "fixture-description", DownloadURL: "https://example.test/1.torrent", SizeBytes: 1024, Seeders: 7,
				Peers: 3, Grabs: 5, IMDBID: "tt1234567", Labels: []string{"免费"}, Category: "电视剧",
				PublishedAt:          time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC),
				DownloadVolumeFactor: testFloat64(0), UploadVolumeFactor: testFloat64(2),
			}}},
			searchTestAdapter{id: 2, name: "beta", resources: []indexer.Resource{{Title: "Show S01E02"}}},
		},
	}
	request := httptest.NewRequest(http.MethodGet, "/search/title/stream?keyword=Show", nil)
	response := httptest.NewRecorder()

	deps.searchTitleStream(response, request)

	if response.Code != http.StatusOK || !strings.HasPrefix(response.Header().Get("Content-Type"), "text/event-stream") {
		t.Fatalf("SSE 响应无效: status=%d content-type=%s", response.Code, response.Header().Get("Content-Type"))
	}
	events := decodeSSEEvents(t, response.Body.String())
	if len(events) != 4 {
		t.Fatalf("事件数量错误: %d body=%s", len(events), response.Body.String())
	}
	if events[0]["type"] != "progress" || events[len(events)-1]["type"] != "done" {
		t.Fatalf("首尾事件错误: %#v", events)
	}
	appendItems := 0
	for _, event := range events {
		if event["type"] != "append" {
			continue
		}
		items, ok := event["items"].([]any)
		if !ok || len(items) != 1 {
			t.Fatalf("append items 无效: %#v", event)
		}
		contextItem := items[0].(map[string]any)
		if contextItem["meta_info"] == nil || contextItem["torrent_info"] == nil {
			t.Fatalf("V2 搜索上下文字段缺失: %#v", contextItem)
		}
		torrent := contextItem["torrent_info"].(map[string]any)
		if torrent["site"] == float64(0) {
			t.Fatalf("搜索结果缺少数值站点 ID: %#v", torrent)
		}
		if torrent["title"] == "Show S01E01 1080p WEB-DL" {
			if torrent["description"] != "fixture-description" || torrent["peers"] != float64(3) || torrent["grabs"] != float64(5) {
				t.Fatalf("V2 种子基础字段映射错误: %#v", torrent)
			}
			if torrent["pubdate"] != "2026-08-10 12:00:00" || torrent["imdbid"] != "tt1234567" || torrent["volume_factor"] != "2X免费" {
				t.Fatalf("V2 时间或 IMDB 字段映射错误: %#v", torrent)
			}
		}
		appendItems += len(items)
	}
	if appendItems != 2 || events[len(events)-1]["total_items"] != float64(2) {
		t.Fatalf("流式结果总数错误: items=%d done=%#v", appendItems, events[len(events)-1])
	}
}

func testFloat64(value float64) *float64 {
	return &value
}

func TestSearchTitleStreamFiltersSiteIDs(t *testing.T) {
	coordinator, _ := indexer.NewCoordinator(2)
	deps := Dependencies{
		Search: coordinator,
		SearchAdapters: []indexer.Adapter{
			searchTestAdapter{id: 1, name: "alpha", resources: []indexer.Resource{{Title: "alpha"}}},
			searchTestAdapter{id: 2, name: "beta", resources: []indexer.Resource{{Title: "beta"}}},
		},
	}
	request := httptest.NewRequest(http.MethodGet, "/search/title/stream?keyword=test&sites=2", nil)
	response := httptest.NewRecorder()

	deps.searchTitleStream(response, request)

	events := decodeSSEEvents(t, response.Body.String())
	if len(events) != 3 || events[1]["site"] != "beta" {
		t.Fatalf("站点筛选未生效: %#v", events)
	}
}

func TestSearchContextsUsesOnlyLocalResourceEnclosure(t *testing.T) {
	t.Parallel()
	items := searchContexts([]indexer.Resource{
		{SiteID: 7, DownloadID: "123/opaque", DownloadURL: "https://evil.invalid/secret.torrent"},
		{SiteID: 7, DownloadURL: "https://evil.invalid/other.torrent"},
	}, time.UTC)
	firstTorrent := items[0]["torrent_info"].(map[string]any)
	if got, want := firstTorrent["enclosure"], "/api/v1/resource/download/7/123%2Fopaque"; got != want {
		t.Fatalf("本地 enclosure = %#v, want %q", got, want)
	}
	secondTorrent := items[1]["torrent_info"].(map[string]any)
	if got := secondTorrent["enclosure"]; got != "" {
		t.Fatalf("缺少资源 ID 时泄漏外部 enclosure: %#v", got)
	}
}

func TestSearchTitleRejectsInvalidSites(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/search/title?sites=1,bad", nil)
	response := httptest.NewRecorder()
	Dependencies{}.searchTitle(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("无效站点参数状态码错误: %d", response.Code)
	}
}

func TestResolveResourceDownloadStreamsBoundedTorrent(t *testing.T) {
	deps := Dependencies{
		Config: config.Config{RequestTimeout: time.Second, MaxResponseBody: 1 << 20},
		SearchAdapters: []indexer.Adapter{downloadTestAdapter{
			searchTestAdapter: searchTestAdapter{id: 7, name: "fixture"},
			download:          indexer.ResolvedDownload{Body: []byte(validTorrentMetainfo), ContentType: "application/x-bittorrent", Filename: "42.torrent"},
		}},
	}
	request := httptest.NewRequest(http.MethodGet, "/resource/download/7/42", nil)
	routeContext := chi.NewRouteContext()
	routeContext.URLParams.Add("siteid", "7")
	routeContext.URLParams.Add("resourceid", "42")
	request = request.WithContext(context.WithValue(request.Context(), chi.RouteCtxKey, routeContext))
	response := httptest.NewRecorder()

	deps.resolveResourceDownload(response, request)

	if response.Code != http.StatusOK || response.Body.String() != validTorrentMetainfo {
		t.Fatalf("站点资源下载响应错误: status=%d body=%q", response.Code, response.Body.String())
	}
	if response.Header().Get("Cache-Control") != "no-store" || !strings.Contains(response.Header().Get("Content-Disposition"), "42.torrent") {
		t.Fatalf("站点资源下载响应头错误: %#v", response.Header())
	}
}

func TestResolveResourceDownloadRejectsInvalidMetainfo(t *testing.T) {
	deps := Dependencies{
		Config: config.Config{RequestTimeout: time.Second, MaxResponseBody: 1 << 20},
		SearchAdapters: []indexer.Adapter{downloadTestAdapter{
			searchTestAdapter: searchTestAdapter{id: 7, name: "fixture"},
			download:          indexer.ResolvedDownload{Body: []byte("<html>login required</html>"), Filename: "42.torrent"},
		}},
	}
	request := httptest.NewRequest(http.MethodGet, "/resource/download/7/42", nil)
	routeContext := chi.NewRouteContext()
	routeContext.URLParams.Add("siteid", "7")
	routeContext.URLParams.Add("resourceid", "42")
	request = request.WithContext(context.WithValue(request.Context(), chi.RouteCtxKey, routeContext))
	response := httptest.NewRecorder()

	deps.resolveResourceDownload(response, request)

	if response.Code != http.StatusBadGateway {
		t.Fatalf("invalid torrent metainfo status=%d body=%q", response.Code, response.Body.String())
	}
}

func TestParseSearchPageUsesZeroBasedSemantics(t *testing.T) {
	for raw, want := range map[string]int{"": 0, "0": 0, "1": 1, "-1": 0, "bad": 0} {
		if got := parseSearchPage(raw); got != want {
			t.Errorf("parseSearchPage(%q)=%d want=%d", raw, got, want)
		}
	}
}

func decodeSSEEvents(t *testing.T, body string) []map[string]any {
	t.Helper()
	chunks := strings.Split(strings.TrimSpace(body), "\n\n")
	events := make([]map[string]any, 0, len(chunks))
	for _, chunk := range chunks {
		payload := strings.TrimPrefix(chunk, "data: ")
		var event map[string]any
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			t.Fatalf("解析 SSE 事件失败: %v payload=%s", err, payload)
		}
		events = append(events, event)
	}
	return events
}

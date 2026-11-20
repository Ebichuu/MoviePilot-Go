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

// searchMediaRequest 构造带 chi 路由参数的请求。
func searchMediaRequest(target string, params map[string]string) *http.Request {
	request := httptest.NewRequest(http.MethodGet, target, nil)
	routeContext := chi.NewRouteContext()
	for name, value := range params {
		routeContext.URLParams.Add(name, value)
	}
	return request.WithContext(context.WithValue(request.Context(), chi.RouteCtxKey, routeContext))
}

func searchMediaDeps(t *testing.T) Dependencies {
	t.Helper()
	coordinator, err := indexer.NewCoordinator(2)
	if err != nil {
		t.Fatalf("创建协调器失败: %v", err)
	}
	t.Cleanup(coordinator.Close)
	return Dependencies{
		Search: coordinator,
		SearchAdapters: []indexer.Adapter{
			searchTestAdapter{id: 1, name: "alpha", resources: []indexer.Resource{
				{Title: "Show S01E01", DownloadURL: "https://example.test/1.torrent", SizeBytes: 1024},
			}},
		},
		Config: config.Config{Location: time.UTC},
	}
}

func TestSearchMediaJSONUsesTitleOrMediaID(t *testing.T) {
	deps := searchMediaDeps(t)

	// title 优先于 mediaid。
	recorder := httptest.NewRecorder()
	deps.searchMedia(recorder, searchMediaRequest("/search/media?title=Show", map[string]string{"mediaid": "ignored"}))
	var body struct {
		Success bool             `json:"success"`
		Data    []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析搜索响应: %v; body=%s", err, recorder.Body.String())
	}
	if !body.Success || len(body.Data) != 1 {
		t.Fatalf("title 搜索 = %s", recorder.Body.String())
	}
	if torrent, _ := body.Data[0]["torrent_info"].(map[string]any); torrent["title"] != "Show S01E01" {
		t.Fatalf("搜索结果 = %#v", body.Data[0])
	}

	// 没有 title 时用路径里的 mediaid 兜底。
	recorder = httptest.NewRecorder()
	deps.searchMedia(recorder, searchMediaRequest("/search/media/tmdb:603", map[string]string{"mediaid": "tmdb:603"}))
	body.Data = nil
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析 mediaid 搜索: %v", err)
	}
	if !body.Success || len(body.Data) != 1 {
		t.Fatalf("mediaid 搜索 = %s", recorder.Body.String())
	}

	// 非法 sites 参数直接 400。
	recorder = httptest.NewRecorder()
	deps.searchMedia(recorder, searchMediaRequest("/search/media?title=x&sites=abc", nil))
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "sites必须是逗号分隔的正整数") {
		t.Fatalf("非法 sites = (%d, %s)", recorder.Code, recorder.Body.String())
	}

	// 没有搜索协调器时按「未搜索到」降级（HTTP 200）。
	recorder = httptest.NewRecorder()
	Dependencies{Config: config.Config{Location: time.UTC}}.searchMedia(recorder, searchMediaRequest("/search/media?title=x", nil))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "未搜索到任何资源") {
		t.Fatalf("无协调器 = (%d, %s)", recorder.Code, recorder.Body.String())
	}
}

func TestSearchMediaStreamEmitsV2SSE(t *testing.T) {
	deps := searchMediaDeps(t)

	recorder := httptest.NewRecorder()
	deps.searchMediaStream(recorder, searchMediaRequest("/search/media/stream?title=Show", map[string]string{"mediaid": "tmdb:603"}))
	if recorder.Code != http.StatusOK || !strings.HasPrefix(recorder.Header().Get("Content-Type"), "text/event-stream") {
		t.Fatalf("SSE 响应无效: status=%d content-type=%s", recorder.Code, recorder.Header().Get("Content-Type"))
	}
	events := decodeSSEEvents(t, recorder.Body.String())
	if len(events) < 2 || events[0]["type"] != "progress" || events[len(events)-1]["type"] != "done" {
		t.Fatalf("事件序列 = %#v", events)
	}
	appendCount := 0
	for _, event := range events {
		if event["type"] == "append" {
			appendCount++
		}
	}
	if appendCount != 1 {
		t.Fatalf("append 事件数 = %d, body=%s", appendCount, recorder.Body.String())
	}

	// 只有 mediaid 没有 title 时同样能搜索（关键词取路径参数）。
	recorder = httptest.NewRecorder()
	deps.searchMediaStream(recorder, searchMediaRequest("/search/media/tmdb:603/stream", map[string]string{"mediaid": "tmdb:603"}))
	events = decodeSSEEvents(t, recorder.Body.String())
	if events[0]["type"] != "progress" || events[len(events)-1]["type"] != "done" {
		t.Fatalf("mediaid 兜底事件 = %#v", events)
	}

	// 没有协调器：progress + done，不 panic。
	recorder = httptest.NewRecorder()
	Dependencies{Config: config.Config{Location: time.UTC}}.searchMediaStream(recorder, searchMediaRequest("/search/media/stream?title=x", nil))
	events = decodeSSEEvents(t, recorder.Body.String())
	if len(events) != 2 || events[0]["type"] != "progress" || events[1]["type"] != "done" {
		t.Fatalf("无协调器事件 = %#v", events)
	}

	// 非法 sites 在写流之前就 400。
	recorder = httptest.NewRecorder()
	deps.searchMediaStream(recorder, searchMediaRequest("/search/media/stream?title=x&sites=0", nil))
	if recorder.Code != http.StatusBadRequest || strings.HasPrefix(recorder.Header().Get("Content-Type"), "text/event-stream") {
		t.Fatalf("非法 sites = (%d, %s)", recorder.Code, recorder.Header().Get("Content-Type"))
	}
}

package httpapi

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/moviepilot-go/moviepilot-go/internal/indexer"
)

// searchStreamAdapter 是流式搜索用的最小适配器，资源与错误都可注入。
type searchStreamAdapter struct {
	name      string
	siteID    int
	resources []indexer.Resource
	err       error
}

func (a searchStreamAdapter) Name() string { return a.name }

func (a searchStreamAdapter) Search(context.Context, indexer.Query) ([]indexer.Resource, error) {
	return a.resources, a.err
}

func (a searchStreamAdapter) SiteID() int { return a.siteID }

// searchPlainAdapter 故意不实现 SiteIDAdapter，用来覆盖「按站点筛选时被跳过」。
type searchPlainAdapter struct {
	name      string
	resources []indexer.Resource
}

func (a searchPlainAdapter) Name() string { return a.name }

func (a searchPlainAdapter) Search(context.Context, indexer.Query) ([]indexer.Resource, error) {
	return a.resources, nil
}

// plainResponseWriter 不实现 http.Flusher。
type plainResponseWriter struct {
	header http.Header
	body   bytes.Buffer
	status int
}

func (w *plainResponseWriter) Header() http.Header { return w.header }

func (w *plainResponseWriter) Write(payload []byte) (int, error) { return w.body.Write(payload) }

func (w *plainResponseWriter) WriteHeader(status int) { w.status = status }

func searchDeps(adapters ...indexer.Adapter) Dependencies {
	coordinator, _ := indexer.NewCoordinator(1)
	return Dependencies{SearchRegistry: indexer.NewRegistry(adapters), Search: coordinator}
}

// TestStreamSearchBranches 覆盖流式搜索的三条出口与 SSE 事件序列。
func TestStreamSearchBranches(t *testing.T) {
	// 站点参数非法：直接 400，不建立流。
	recorder := httptest.NewRecorder()
	Dependencies{}.streamSearch(recorder, httptest.NewRequest(http.MethodGet, "/search/title/stream?sites=abc", nil), "kw")
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("站点参数非法 = %d", recorder.Code)
	}

	// 响应不支持 Flush：500。
	plain := &plainResponseWriter{header: http.Header{}}
	Dependencies{}.streamSearch(plain, httptest.NewRequest(http.MethodGet, "/search/title/stream", nil), "kw")
	if plain.status != http.StatusInternalServerError {
		t.Fatalf("不支持流式 = %d", plain.status)
	}

	// 没有搜索引擎：先发一帧 progress，再直接 done。
	recorder = httptest.NewRecorder()
	Dependencies{SearchRegistry: indexer.NewRegistry([]indexer.Adapter{searchStreamAdapter{name: "站A"}})}.
		streamSearch(recorder, httptest.NewRequest(http.MethodGet, "/search/title/stream", nil), "kw")
	body := recorder.Body.String()
	if !strings.Contains(body, `"stage":"searching"`) || !strings.Contains(body, `"type":"done"`) {
		t.Fatalf("无搜索引擎 = %s", body)
	}
	if contentType := recorder.Header().Get("Content-Type"); !strings.Contains(contentType, "text/event-stream") {
		t.Fatalf("Content-Type = %q", contentType)
	}

	// 有资源：append 事件里带资源条目，done 里带总数。
	published := time.Now().Add(-90 * time.Minute)
	recorder = httptest.NewRecorder()
	searchDeps(searchStreamAdapter{name: "站A", siteID: 7, resources: []indexer.Resource{
		{Site: "站A", SiteID: 7, Title: "某剧.S01E01.1080p", DownloadID: "abc", PublishedAt: published},
		{Site: "站A", SiteID: 7, Title: "另一部.2020", DownloadID: "def"},
	}}).streamSearch(recorder, httptest.NewRequest(http.MethodGet, "/search/title/stream?page=1", nil), "某剧")
	body = recorder.Body.String()
	if !strings.Contains(body, `"type":"append"`) || !strings.Contains(body, "某剧.S01E01.1080p") {
		t.Fatalf("资源事件 = %s", body)
	}
	if !strings.Contains(body, `"total_items":2`) || !strings.Contains(body, "搜索完成，共 2 个资源") {
		t.Fatalf("完成事件 = %s", body)
	}

	// 适配器失败不中断流：该站点发一帧 success=false 的 append，随后照常 done。
	recorder = httptest.NewRecorder()
	searchDeps(searchStreamAdapter{name: "站B", err: errors.New("上游 502")}).
		streamSearch(recorder, httptest.NewRequest(http.MethodGet, "/search/title/stream", nil), "kw")
	body = recorder.Body.String()
	if !strings.Contains(body, `"success":false`) || !strings.Contains(body, "上游 502") ||
		!strings.Contains(body, `"type":"done"`) {
		t.Fatalf("站点失败 = %s", body)
	}
}

// TestSearchJSONBranches 覆盖 JSON 搜索的四条出口。
func TestSearchJSONBranches(t *testing.T) {
	// 站点参数非法：400。
	recorder := httptest.NewRecorder()
	Dependencies{}.searchJSON(recorder, httptest.NewRequest(http.MethodGet, "/search?sites=abc", nil), "kw")
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("站点参数非法 = %d", recorder.Code)
	}

	// 没有搜索引擎：200 + 业务提示。
	recorder = httptest.NewRecorder()
	Dependencies{}.searchJSON(recorder, httptest.NewRequest(http.MethodGet, "/search", nil), "kw")
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "未搜索到任何资源") {
		t.Fatalf("无搜索引擎 = %d %s", recorder.Code, recorder.Body.String())
	}

	// 适配器失败但协调器不报错：按「没有结果」返回业务提示。
	recorder = httptest.NewRecorder()
	searchDeps(searchStreamAdapter{name: "站B", err: errors.New("上游 502")}).
		searchJSON(recorder, httptest.NewRequest(http.MethodGet, "/search", nil), "kw")
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "未搜索到任何资源") {
		t.Fatalf("站点失败 = %d %s", recorder.Code, recorder.Body.String())
	}

	// 没有结果：200 + 业务提示。
	recorder = httptest.NewRecorder()
	searchDeps(searchStreamAdapter{name: "站C"}).
		searchJSON(recorder, httptest.NewRequest(http.MethodGet, "/search", nil), "kw")
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "未搜索到任何资源") {
		t.Fatalf("空结果 = %d %s", recorder.Code, recorder.Body.String())
	}

	// 正常结果：data 里是 V2 Context 投影。
	recorder = httptest.NewRecorder()
	searchDeps(searchStreamAdapter{name: "站D", siteID: 3, resources: []indexer.Resource{
		{Site: "站D", SiteID: 3, Title: "某剧.S02E03.2160p", DownloadID: "xyz", Seeders: 12},
	}}).searchJSON(recorder, httptest.NewRequest(http.MethodGet, "/search?page=2", nil), "某剧")
	body := recorder.Body.String()
	if recorder.Code != http.StatusOK || !strings.Contains(body, "某剧.S02E03.2160p") ||
		!strings.Contains(body, "/api/v1/resource/download/3/xyz") {
		t.Fatalf("正常结果 = %d %s", recorder.Code, body)
	}
}

// TestSelectSearchAdaptersBySite 覆盖按站点 ID 筛选：没有站点 ID 的适配器被跳过，
// 未知站点 ID 报错。
func TestSelectSearchAdaptersBySite(t *testing.T) {
	adapters := []indexer.Adapter{
		searchPlainAdapter{name: "无站点ID"},
		searchStreamAdapter{name: "站E", siteID: 5},
	}
	selected, err := selectSearchAdapters(adapters, "5")
	if err != nil || len(selected) != 1 || selected[0].Name() != "站E" {
		t.Fatalf("筛选 = %v, %v", selected, err)
	}
	if _, err := selectSearchAdapters(adapters, "5,5"); err != nil {
		t.Fatalf("重复站点 ID = %v", err)
	}
	if selected, err := selectSearchAdapters(adapters, " "); err != nil || len(selected) != len(adapters) {
		t.Fatalf("空筛选 = %d, %v", len(selected), err)
	}
	// 未知站点 ID 不报错，只是选不到适配器。
	if selected, err := selectSearchAdapters(adapters, "5,9"); err != nil || len(selected) != 1 {
		t.Fatalf("部分未知站点 = %d, %v", len(selected), err)
	}
	if selected, err := selectSearchAdapters(adapters, "9"); err != nil || len(selected) != 0 {
		t.Fatalf("未知站点 = %d, %v", len(selected), err)
	}
	if _, err := selectSearchAdapters(adapters, "abc"); err == nil {
		t.Fatal("非数字站点 ID 应报错")
	}
}

// TestSearchProjectionHelpers 覆盖分页、时间与资源落地地址的边界。
func TestSearchProjectionHelpers(t *testing.T) {
	if page := parseSearchPage("abc"); page != 0 {
		t.Fatalf("非法页码 = %d", page)
	}
	if page := parseSearchPage("-2"); page != 0 {
		t.Fatalf("负页码 = %d", page)
	}
	if page := parseSearchPage(" 3 "); page != 3 {
		t.Fatalf("页码 = %d", page)
	}

	if value := publishedDate(time.Time{}, nil); value != nil {
		t.Fatalf("零时间 = %v", value)
	}
	moment := time.Date(2024, 3, 4, 5, 6, 7, 0, time.UTC)
	if value := publishedDate(moment, nil); value != "2024-03-04 05:06:07" {
		t.Fatalf("UTC 时间 = %v", value)
	}
	shanghai, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Skipf("缺少时区数据: %v", err)
	}
	if value := publishedDate(moment, shanghai); value != "2024-03-04 13:06:07" {
		t.Fatalf("本地时间 = %v", value)
	}

	if value := elapsed(time.Time{}); value != "" {
		t.Fatalf("零时间间隔 = %q", value)
	}
	if value := elapsed(time.Now().Add(2 * time.Hour)); value != "" {
		t.Fatalf("未来时间间隔 = %q", value)
	}
	if value := elapsed(time.Now().Add(-30 * time.Minute)); !strings.HasSuffix(value, "分钟") {
		t.Fatalf("分钟间隔 = %q", value)
	}
	if value := elapsed(time.Now().Add(-(3*time.Hour + 10*time.Minute))); !strings.Contains(value, "小时") {
		t.Fatalf("小时间隔 = %q", value)
	}
	if value := elapsed(time.Now().Add(-(49 * time.Hour))); !strings.Contains(value, "天") {
		t.Fatalf("天间隔 = %q", value)
	}

	if value := localResourceEnclosure(indexer.Resource{SiteID: 0, DownloadID: "abc"}); value != "" {
		t.Fatalf("无站点 ID = %q", value)
	}
	if value := localResourceEnclosure(indexer.Resource{SiteID: 7}); value != "" {
		t.Fatalf("无下载 ID = %q", value)
	}
	if value := localResourceEnclosure(indexer.Resource{SiteID: 7, DownloadID: "a/b"}); value != "/api/v1/resource/download/7/a%2Fb" {
		t.Fatalf("落地地址 = %q", value)
	}

	if items := searchAppendEvent("站", nil, 1, 0, 0)["items"]; items == nil {
		t.Fatal("空条目应归一成空数组")
	}
	if done := searchDoneEvent(0); done["type"] != "done" {
		t.Fatalf("完成事件 = %v", done)
	}
}

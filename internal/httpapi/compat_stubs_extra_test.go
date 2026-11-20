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

// responseBody 是这些兼容端点共用的 `Response` 子集。
type responseBody struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
}

func decodeResponseBody(t *testing.T, body []byte) responseBody {
	t.Helper()
	var decoded responseBody
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("解析 Response: %v; body=%s", err, body)
	}
	return decoded
}

func TestSystemAndTailStubsFollowV2FailureBranches(t *testing.T) {
	tests := []struct {
		name    string
		handler http.HandlerFunc
		want    responseBody
	}{
		{name: "版本列表", handler: systemVersions, want: responseBody{Message: "无法获取版本列表"}},
		{name: "整理进度", handler: systemProgress, want: responseBody{Message: "没有正在执行的任务"}},
		{name: "模块安装测试", handler: systemModuleTest, want: responseBody{Message: "当前版本不支持模块安装"}},
		{name: "重启拒绝", handler: systemRestart, want: responseBody{Message: "请通过容器编排重启服务"}},
		{name: "日志下载", handler: systemLoggingDownload, want: responseBody{Message: "日志文件不存在"}},
		{name: "AI 重命名", handler: transferAIRedo, want: responseBody{Message: "未配置 AI 模型，无法执行 AI 重命名"}},
		{name: "AI 历史重命名", handler: transferAIHistoryRedo, want: responseBody{Message: "未配置 AI 模型，无法执行 AI 重命名"}},
		{name: "Webhook 接收", handler: webhookReceive, want: responseBody{Message: "未配置 Webhook 处理"}},
		{name: "字幕下载", handler: downloadSubtitle, want: responseBody{Message: "未配置字幕站点，无法下载字幕"}},
		{name: "令牌交换", handler: authExchange, want: responseBody{Message: "请直接使用登录令牌（access_token）"}},
		{name: "Webhook 探测", handler: webhookProbe, want: responseBody{Success: true}},
		{name: "删除整理队列项", handler: removeTransferQueueItem, want: responseBody{Success: true}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			test.handler(recorder, httptest.NewRequest(http.MethodGet, "/stub", nil))
			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d", recorder.Code)
			}
			got := decodeResponseBody(t, recorder.Body.Bytes())
			if got.Success != test.want.Success || got.Message != test.want.Message {
				t.Fatalf("body = %+v, want %+v", got, test.want)
			}
		})
	}

	// 纯数组/空对象占位不能变成 null 或 404。
	emptyList := []struct {
		name    string
		handler http.HandlerFunc
	}{
		{name: "模块列表", handler: systemModuleList},
		{name: "日志列表", handler: systemLogging},
		{name: "整理队列", handler: transferQueueList},
		{name: "当前整理", handler: transferNowList},
	}
	for _, test := range emptyList {
		recorder := httptest.NewRecorder()
		test.handler(recorder, httptest.NewRequest(http.MethodGet, "/stub", nil))
		if strings.TrimSpace(recorder.Body.String()) != "[]" {
			t.Fatalf("%s body = %s", test.name, recorder.Body.String())
		}
	}

	recorder := httptest.NewRecorder()
	pluginStatistic(recorder, httptest.NewRequest(http.MethodGet, "/plugin/statistic", nil))
	if strings.TrimSpace(recorder.Body.String()) != "{}" {
		t.Fatalf("插件统计 body = %s", recorder.Body.String())
	}
}

// pluginFolderRequest 构造带 chi 路由参数的请求，避免为单测搭整棵树。
func pluginFolderRequest(method, body, folderName string) *http.Request {
	request := httptest.NewRequest(method, "/plugin/folder", strings.NewReader(body))
	routeContext := chi.NewRouteContext()
	routeContext.URLParams.Add("folder_name", folderName)
	return request.WithContext(context.WithValue(request.Context(), chi.RouteCtxKey, routeContext))
}

func TestPluginFolderHandlersWithoutStore(t *testing.T) {
	deps := Dependencies{}

	// 没有设置存储时列表降级为空对象。
	recorder := httptest.NewRecorder()
	deps.pluginFolders(recorder, pluginFolderRequest(http.MethodGet, "", "电影"))
	if strings.TrimSpace(recorder.Body.String()) != "{}" {
		t.Fatalf("列表 = %s", recorder.Body.String())
	}

	// 读取不需要存储，所以"读取失败"只可能在存储报错时出现；这里验证保存失败分支。
	save := decodeResponseBody(t, func() []byte {
		recorder := httptest.NewRecorder()
		deps.savePluginFolders(recorder, pluginFolderRequest(http.MethodPost, `{"电影":["A"]}`, ""))
		return recorder.Body.Bytes()
	}())
	if save.Success || save.Message != "保存插件文件夹失败" {
		t.Fatalf("保存 = %+v", save)
	}

	invalid := decodeResponseBody(t, func() []byte {
		recorder := httptest.NewRecorder()
		deps.savePluginFolders(recorder, pluginFolderRequest(http.MethodPost, `{"电影":`, ""))
		return recorder.Body.Bytes()
	}())
	if invalid.Success || invalid.Message != "参数错误" {
		t.Fatalf("坏 JSON = %+v", invalid)
	}

	blank := decodeResponseBody(t, func() []byte {
		recorder := httptest.NewRecorder()
		deps.createPluginFolder(recorder, pluginFolderRequest(http.MethodPost, "", "   "))
		return recorder.Body.Bytes()
	}())
	if blank.Success || blank.Message != "文件夹名称不能为空" {
		t.Fatalf("空名称 = %+v", blank)
	}

	create := decodeResponseBody(t, func() []byte {
		recorder := httptest.NewRecorder()
		deps.createPluginFolder(recorder, pluginFolderRequest(http.MethodPost, "", "电影"))
		return recorder.Body.Bytes()
	}())
	if create.Success || create.Message != "保存插件文件夹失败" {
		t.Fatalf("创建 = %+v", create)
	}

	remove := decodeResponseBody(t, func() []byte {
		recorder := httptest.NewRecorder()
		deps.deletePluginFolder(recorder, pluginFolderRequest(http.MethodDelete, "", "电影"))
		return recorder.Body.Bytes()
	}())
	if remove.Success || remove.Message != "文件夹 '电影' 不存在" {
		t.Fatalf("删除 = %+v", remove)
	}

	updateBad := decodeResponseBody(t, func() []byte {
		recorder := httptest.NewRecorder()
		deps.updateFolderPlugins(recorder, pluginFolderRequest(http.MethodPost, `not-json`, "电影"))
		return recorder.Body.Bytes()
	}())
	if updateBad.Success || updateBad.Message != "参数错误" {
		t.Fatalf("更新坏 JSON = %+v", updateBad)
	}

	update := decodeResponseBody(t, func() []byte {
		recorder := httptest.NewRecorder()
		deps.updateFolderPlugins(recorder, pluginFolderRequest(http.MethodPost, `["A","B"]`, "电影"))
		return recorder.Body.Bytes()
	}())
	if update.Success || update.Message != "保存插件文件夹失败" {
		t.Fatalf("更新 = %+v", update)
	}
}

func TestPluginFolderHelpers(t *testing.T) {
	if err := decodePluginFoldersBody(nil, &map[string][]string{}); err == nil {
		t.Fatal("nil 请求应报错")
	}
	request := httptest.NewRequest(http.MethodPost, "/", nil)
	if err := decodePluginFoldersBody(request, &map[string][]string{}); err == nil {
		t.Fatal("nil body 应报错")
	}
	var folders map[string][]string
	if err := decodePluginFoldersBody(pluginFolderRequest(http.MethodPost, `{"电影":["A","B"]}`, ""), &folders); err != nil {
		t.Fatalf("解码失败: %v", err)
	}
	if len(folders["电影"]) != 2 {
		t.Fatalf("folders = %#v", folders)
	}

	deps := Dependencies{}
	loaded, err := deps.loadPluginFolders(pluginFolderRequest(http.MethodGet, "", ""))
	if err != nil || len(loaded) != 0 {
		t.Fatalf("loadPluginFolders() = (%#v, %v)", loaded, err)
	}
	if err := deps.storePluginFolders(pluginFolderRequest(http.MethodPost, "", ""), nil); err == nil {
		t.Fatal("没有存储时保存必须报错")
	}

	names := sortedPluginFolderNames(map[string][]string{"b": nil, "a": {}, "c": {"X"}})
	if len(names) != 3 || names[0] != "a" || names[1] != "b" || names[2] != "c" {
		t.Fatalf("sortedPluginFolderNames() = %v", names)
	}
	if empty := sortedPluginFolderNames(nil); len(empty) != 0 {
		t.Fatalf("空输入 = %v", empty)
	}
}

func TestLastSearchEndpointsExposeLastQuery(t *testing.T) {
	// 没有搜索源时两端都返回空状态。
	empty := Dependencies{}
	recorder := httptest.NewRecorder()
	empty.lastSearch(recorder, httptest.NewRequest(http.MethodGet, "/search/last", nil))
	if strings.TrimSpace(recorder.Body.String()) != "[]" {
		t.Fatalf("无搜索源 lastSearch = %s", recorder.Body.String())
	}
	recorder = httptest.NewRecorder()
	empty.lastSearchContext(recorder, httptest.NewRequest(http.MethodGet, "/search/last/context", nil))
	contextBody := decodeResponseBody(t, recorder.Body.Bytes())
	if !contextBody.Success {
		t.Fatalf("无搜索源 lastSearchContext = %s", recorder.Body.String())
	}

	coordinator, err := indexer.NewCoordinator(1)
	if err != nil {
		t.Fatalf("创建协调器失败: %v", err)
	}
	t.Cleanup(coordinator.Close)
	adapter := searchTestAdapter{id: 1, name: "alpha", resources: []indexer.Resource{{
		Title: "Show S01E01 1080p", DownloadURL: "https://example.test/1.torrent",
		SizeBytes: 1024, Seeders: 3, PublishedAt: time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC),
	}}}
	if _, err := coordinator.Search(t.Context(), []indexer.Adapter{adapter}, indexer.Query{
		Keyword: "show", Page: 1, Limit: 5,
	}, nil); err != nil {
		t.Fatalf("执行搜索失败: %v", err)
	}

	deps := Dependencies{Search: coordinator, Config: config.Config{Location: time.UTC}}
	recorder = httptest.NewRecorder()
	deps.lastSearch(recorder, httptest.NewRequest(http.MethodGet, "/search/last", nil))
	var results []map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &results); err != nil {
		t.Fatalf("解析 lastSearch: %v; body=%s", err, recorder.Body.String())
	}
	if len(results) == 0 {
		t.Fatalf("上次搜索结果为空: %s", recorder.Body.String())
	}

	recorder = httptest.NewRecorder()
	deps.lastSearchContext(recorder, httptest.NewRequest(http.MethodGet, "/search/last/context", nil))
	var body struct {
		Success bool `json:"success"`
		Data    struct {
			Params struct {
				Keyword string `json:"keyword"`
				Page    int    `json:"page"`
				Limit   int    `json:"limit"`
			} `json:"params"`
			Results []map[string]any `json:"results"`
		} `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析 lastSearchContext: %v; body=%s", err, recorder.Body.String())
	}
	if !body.Success || body.Data.Params.Keyword != "show" || body.Data.Params.Page != 1 ||
		body.Data.Params.Limit != 5 || len(body.Data.Results) == 0 {
		t.Fatalf("lastSearchContext = %s", recorder.Body.String())
	}
}

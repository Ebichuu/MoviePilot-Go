package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/moviepilot-go/moviepilot-go/internal/pluginengine"
)

// 本文件覆盖原生插件的 HTTP 边界：V2 前端实际读取的字段、安装即启用、
// 配置校验、重置、以及「注册表未装配 / 能力未接线」两种降级路径。

// pluginRouteRequest 构造带 chi 路由参数的请求，避免为单测搭整棵路由树。
func pluginRouteRequest(method, body, pluginID string) *http.Request {
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	request := httptest.NewRequest(method, "/plugin/"+pluginID, reader)
	routeContext := chi.NewRouteContext()
	routeContext.URLParams.Add("plugin_id", pluginID)
	return request.WithContext(context.WithValue(request.Context(), chi.RouteCtxKey, routeContext))
}

// newPluginHTTPRegistry 造一个装了 5 个内置插件的注册表；services 决定能力是否接线。
func newPluginHTTPRegistry(t *testing.T, services pluginengine.Services) *pluginengine.Registry {
	t.Helper()
	registry := pluginengine.NewRegistry(pluginengine.NewMemoryStore())
	if err := registry.RegisterAll(pluginengine.BuiltinDefinitions(services)); err != nil {
		t.Fatalf("注册失败: %v", err)
	}
	return registry
}

func decodePluginList(t *testing.T, body []byte) []map[string]any {
	t.Helper()
	var plugins []map[string]any
	if err := json.Unmarshal(body, &plugins); err != nil {
		t.Fatalf("解析插件列表: %v; body=%s", err, body)
	}
	return plugins
}

func TestPluginListStateFiltering(t *testing.T) {
	deps := Dependencies{Plugins: newPluginHTTPRegistry(t, pluginengine.Services{})}
	if err := deps.Plugins.Install(context.Background(), pluginengine.PluginSiteSignIn); err != nil {
		t.Fatalf("安装失败: %v", err)
	}

	t.Run("不带 state 返回全部", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		deps.listPlugins(recorder, httptest.NewRequest(http.MethodGet, "/plugin/", nil))
		plugins := decodePluginList(t, recorder.Body.Bytes())
		if len(plugins) != 5 {
			t.Fatalf("插件数量 = %d", len(plugins))
		}
		// V2 前端按这些键渲染卡片，键名错了卡片就是空白。
		first := plugins[0]
		for _, key := range []string{"id", "plugin_name", "plugin_desc", "plugin_icon", "plugin_version", "installed", "state", "has_update", "has_page", "repo_url", "author_url", "plugin_config_prefix", "auth_level", "is_local", "system_version_compatible"} {
			if _, present := first[key]; !present {
				t.Fatalf("缺少字段 %s: %#v", key, first)
			}
		}
		if first["has_page"] != false || first["has_update"] != false {
			t.Fatalf("内置插件不应声明页面或更新: %#v", first)
		}
	})

	t.Run("state=installed 只返回已安装", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		deps.listPlugins(recorder, httptest.NewRequest(http.MethodGet, "/plugin/?state=installed", nil))
		plugins := decodePluginList(t, recorder.Body.Bytes())
		if len(plugins) != 1 || plugins[0]["id"] != pluginengine.PluginSiteSignIn {
			t.Fatalf("plugins = %#v", plugins)
		}
		// V2 的 state 是「运行中」布尔值：前端状态点颜色与「运行中」筛选都按它判断，
		// 给字符串的话在 JS 里恒为真，禁用的插件也会显示成运行中。
		if _, ok := plugins[0]["state"].(bool); !ok {
			t.Fatalf("state 应为 bool，实际 %#v", plugins[0]["state"])
		}
	})

	t.Run("state=market 返回内置清单供安装", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		deps.listPlugins(recorder, httptest.NewRequest(http.MethodGet, "/plugin/?state=market", nil))
		plugins := decodePluginList(t, recorder.Body.Bytes())
		// 市场为空会让前端插件页连「安装」按钮都没有；内置插件就是可安装项。
		if len(plugins) != 5 {
			t.Fatalf("插件数量 = %d", len(plugins))
		}
		installed := 0
		for _, plugin := range plugins {
			if plugin["installed"] == true {
				installed++
			}
		}
		if installed != 1 {
			t.Fatalf("已安装标记 = %d, 期望 1: %#v", installed, plugins)
		}
	})
}

func TestPluginInstallEnablesAndRuns(t *testing.T) {
	ran := 0
	deps := Dependencies{Plugins: newPluginHTTPRegistry(t, pluginengine.Services{
		SignInAll: func(context.Context, pluginengine.RunInput) (pluginengine.Result, error) {
			ran++
			return pluginengine.Result{Message: "已入队：站点签到"}, nil
		},
	})}

	recorder := httptest.NewRecorder()
	deps.installPlugin(recorder, pluginRouteRequest(http.MethodGet, "", pluginengine.PluginSiteSignIn))
	if body := decodeResponseBody(t, recorder.Body.Bytes()); !body.Success {
		t.Fatalf("安装失败: %+v", body)
	}
	plugin, err := deps.Plugins.Get(pluginengine.PluginSiteSignIn)
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	if !plugin.Installed || !plugin.Enabled {
		t.Fatalf("安装即启用未生效: %#v", plugin)
	}

	// 动作端点：成功只代表已入队。
	recorder = httptest.NewRecorder()
	deps.runPluginAction(recorder, pluginRouteRequest(http.MethodPost, "", pluginengine.PluginSiteSignIn))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
	var result struct {
		Success bool   `json:"success"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil {
		t.Fatalf("解析动作结果: %v", err)
	}
	if !result.Success || result.Message != "已入队：站点签到" || ran != 1 {
		t.Fatalf("result = %+v, ran = %d", result, ran)
	}

	// 未安装的插件不能直接执行。
	recorder = httptest.NewRecorder()
	deps.runPluginAction(recorder, pluginRouteRequest(http.MethodPost, "", pluginengine.PluginCloudBackup))
	if body := decodeResponseBody(t, recorder.Body.Bytes()); body.Success {
		t.Fatalf("未安装插件不应执行成功: %+v", body)
	}
}

func TestPluginConfigSaveAndReset(t *testing.T) {
	deps := Dependencies{Plugins: newPluginHTTPRegistry(t, pluginengine.Services{})}
	ctx := context.Background()
	if err := deps.Plugins.Install(ctx, pluginengine.PluginCloudBackup); err != nil {
		t.Fatalf("安装失败: %v", err)
	}

	t.Run("合法配置保存成功", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		deps.savePluginConfig(recorder, pluginRouteRequest(http.MethodPut, `{"keep":3,"encrypted":false}`, pluginengine.PluginCloudBackup))
		if body := decodeResponseBody(t, recorder.Body.Bytes()); !body.Success {
			t.Fatalf("保存失败: %+v", body)
		}
		plugin, _ := deps.Plugins.Get(pluginengine.PluginCloudBackup)
		if plugin.Config["keep"] != float64(3) && plugin.Config["keep"] != int64(3) {
			t.Fatalf("keep = %#v", plugin.Config["keep"])
		}
	})

	t.Run("未知键被拒绝", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		deps.savePluginConfig(recorder, pluginRouteRequest(http.MethodPut, `{"nope":1}`, pluginengine.PluginCloudBackup))
		body := decodeResponseBody(t, recorder.Body.Bytes())
		if body.Success || !strings.Contains(body.Message, "插件配置不合法") {
			t.Fatalf("body = %+v", body)
		}
	})

	t.Run("坏 JSON 被拒绝", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		deps.savePluginConfig(recorder, pluginRouteRequest(http.MethodPut, `{`, pluginengine.PluginCloudBackup))
		if body := decodeResponseBody(t, recorder.Body.Bytes()); body.Success {
			t.Fatalf("坏 JSON 不应成功: %+v", body)
		}
	})

	t.Run("重置回默认值", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		deps.resetPlugin(recorder, pluginRouteRequest(http.MethodGet, "", pluginengine.PluginCloudBackup))
		if body := decodeResponseBody(t, recorder.Body.Bytes()); !body.Success {
			t.Fatalf("重置失败: %+v", body)
		}
		plugin, _ := deps.Plugins.Get(pluginengine.PluginCloudBackup)
		if plugin.Config["keep"] != int64(7) || plugin.Config["encrypted"] != true {
			t.Fatalf("重置后配置 = %#v", plugin.Config)
		}
	})
}

func TestPluginFormAndDetail(t *testing.T) {
	deps := Dependencies{Plugins: newPluginHTTPRegistry(t, pluginengine.Services{})}

	recorder := httptest.NewRecorder()
	deps.pluginForm(recorder, pluginRouteRequest(http.MethodGet, "", pluginengine.PluginRSSEnhance))
	var form struct {
		RenderMode string           `json:"render_mode"`
		Model      map[string]any   `json:"model"`
		Fields     []map[string]any `json:"fields"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &form); err != nil {
		t.Fatalf("解析表单: %v", err)
	}
	if form.RenderMode != "vuetify" {
		t.Fatalf("render_mode 必须是 vuetify（空串会被前端当成加载失败）: %q", form.RenderMode)
	}
	if len(form.Fields) == 0 {
		t.Fatalf("字段声明不应为空")
	}
	if form.Fields[0]["key"] == "" || form.Fields[0]["type"] == "" {
		t.Fatalf("字段声明缺少键或类型: %#v", form.Fields[0])
	}
	if _, ok := form.Model["free_only"]; !ok {
		t.Fatalf("默认配置未带上: %#v", form.Model)
	}

	// conf 是前端 FormRender 真正渲染的东西：没有它，对话框就是一片空白。
	var confForm struct {
		Conf []map[string]any `json:"conf"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &confForm); err != nil {
		t.Fatalf("解析 conf: %v", err)
	}
	if len(confForm.Conf) != 1 {
		t.Fatalf("conf = %#v", confForm.Conf)
	}
	container := confForm.Conf[0]
	if container["component"] != "div" {
		t.Fatalf("外层容器 = %#v", container)
	}
	children, _ := container["content"].([]any)
	if len(children) != 2 {
		t.Fatalf("RSS 增强应有 2 个控件: %#v", children)
	}
	for _, child := range children {
		item, _ := child.(map[string]any)
		props, _ := item["props"].(map[string]any)
		if item["component"] != "VSwitch" && item["component"] != "VTextField" {
			t.Fatalf("控件类型 = %#v", item["component"])
		}
		// FormRender 靠 props.model 绑定 v-model，键名必须与字段名一致。
		if props["model"] != "free_only" && props["model"] != "keyword_filter" {
			t.Fatalf("props.model = %#v", props["model"])
		}
	}

	recorder = httptest.NewRecorder()
	deps.getPlugin(recorder, pluginRouteRequest(http.MethodGet, "", pluginengine.PluginRSSEnhance))
	var detail map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &detail); err != nil {
		t.Fatalf("解析详情: %v", err)
	}
	if detail["id"] != pluginengine.PluginRSSEnhance {
		t.Fatalf("detail = %#v", detail)
	}

	recorder = httptest.NewRecorder()
	deps.getPlugin(recorder, pluginRouteRequest(http.MethodGet, "", "ghost"))
	if body := decodeResponseBody(t, recorder.Body.Bytes()); body.Success {
		t.Fatalf("不存在的插件不应成功: %+v", body)
	}
}

func TestPluginUninstallAndReload(t *testing.T) {
	deps := Dependencies{Plugins: newPluginHTTPRegistry(t, pluginengine.Services{})}
	ctx := context.Background()
	if err := deps.Plugins.Install(ctx, pluginengine.PluginMediaLibrarySync); err != nil {
		t.Fatalf("安装失败: %v", err)
	}
	if err := deps.Plugins.Enable(ctx, pluginengine.PluginMediaLibrarySync); err != nil {
		t.Fatalf("启用失败: %v", err)
	}

	recorder := httptest.NewRecorder()
	deps.reloadPlugin(recorder, pluginRouteRequest(http.MethodGet, "", pluginengine.PluginMediaLibrarySync))
	if body := decodeResponseBody(t, recorder.Body.Bytes()); !body.Success {
		t.Fatalf("重载失败: %+v", body)
	}

	recorder = httptest.NewRecorder()
	deps.uninstallPlugin(recorder, pluginRouteRequest(http.MethodDelete, "", pluginengine.PluginMediaLibrarySync))
	if body := decodeResponseBody(t, recorder.Body.Bytes()); !body.Success {
		t.Fatalf("卸载失败: %+v", body)
	}
	plugin, _ := deps.Plugins.Get(pluginengine.PluginMediaLibrarySync)
	if plugin.Installed || plugin.Enabled {
		t.Fatalf("卸载后状态 = %#v", plugin)
	}
}

func TestPluginHandlersWithoutRegistry(t *testing.T) {
	deps := Dependencies{}

	recorder := httptest.NewRecorder()
	deps.listPlugins(recorder, httptest.NewRequest(http.MethodGet, "/plugin/", nil))
	if strings.TrimSpace(recorder.Body.String()) != "[]" {
		t.Fatalf("列表 = %s", recorder.Body.String())
	}

	recorder = httptest.NewRecorder()
	deps.listInstalledPlugins(recorder, httptest.NewRequest(http.MethodGet, "/plugin/installed", nil))
	if strings.TrimSpace(recorder.Body.String()) != "[]" {
		t.Fatalf("已安装列表 = %s", recorder.Body.String())
	}

	recorder = httptest.NewRecorder()
	deps.installPlugin(recorder, pluginRouteRequest(http.MethodGet, "", pluginengine.PluginSiteSignIn))
	if body := decodeResponseBody(t, recorder.Body.Bytes()); body.Success || !strings.Contains(body.Message, "插件体系未启用") {
		t.Fatalf("body = %+v", body)
	}
}

func TestPluginRunWithoutWiring(t *testing.T) {
	deps := Dependencies{Plugins: newPluginHTTPRegistry(t, pluginengine.Services{})}
	ctx := context.Background()
	if err := deps.Plugins.Install(ctx, pluginengine.PluginSiteSignIn); err != nil {
		t.Fatalf("安装失败: %v", err)
	}
	if err := deps.Plugins.Enable(ctx, pluginengine.PluginSiteSignIn); err != nil {
		t.Fatalf("启用失败: %v", err)
	}

	recorder := httptest.NewRecorder()
	deps.runPluginAction(recorder, pluginRouteRequest(http.MethodPost, "", pluginengine.PluginSiteSignIn))
	body := decodeResponseBody(t, recorder.Body.Bytes())
	if body.Success || !strings.Contains(body.Message, "插件能力未接线") {
		t.Fatalf("未接线时应当如实失败: %+v", body)
	}

	recorder = httptest.NewRecorder()
	deps.runPluginAction(recorder, pluginRouteRequest(http.MethodPost, "", pluginengine.PluginSiteSignIn+"?action=explode"))
	if body := decodeResponseBody(t, recorder.Body.Bytes()); body.Success {
		t.Fatalf("不存在的动作不应成功: %+v", body)
	}
}

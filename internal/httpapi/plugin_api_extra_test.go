package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/moviepilot-go/moviepilot-go/internal/pluginengine"
)

// 本文件补的是「失败分支」：注册表未装配时每个端点都要给出可读失败，
// 错误映射覆盖所有哨兵错误，避免前端只看到一句「插件操作失败」。

func TestPluginHandlersWithoutRegistryCoverAllRoutes(t *testing.T) {
	deps := Dependencies{}
	routes := []struct {
		name    string
		handler http.HandlerFunc
		body    string
	}{
		{name: "详情", handler: deps.getPlugin},
		{name: "安装", handler: deps.installPlugin},
		{name: "卸载", handler: deps.uninstallPlugin},
		{name: "重置", handler: deps.resetPlugin},
		{name: "表单", handler: deps.pluginForm},
		{name: "重载", handler: deps.reloadPlugin},
		{name: "动作", handler: deps.runPluginAction},
		{name: "保存配置", handler: deps.savePluginConfig, body: `{"keep":1}`},
	}
	for _, route := range routes {
		t.Run(route.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			route.handler(recorder, pluginRouteRequest(http.MethodPost, route.body, pluginengine.PluginSiteSignIn))
			body := decodeResponseBody(t, recorder.Body.Bytes())
			if body.Success || !strings.Contains(body.Message, "插件体系未启用") {
				t.Fatalf("body = %+v", body)
			}
		})
	}
}

func TestPluginFailureMapping(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{name: "不存在", err: pluginengine.ErrPluginNotFound, want: "插件不存在"},
		{name: "已注册", err: pluginengine.ErrPluginExists, want: "插件已注册"},
		{name: "未安装", err: pluginengine.ErrPluginNotInstalled, want: "插件未安装"},
		{name: "未启用", err: pluginengine.ErrPluginDisabled, want: "插件未启用"},
		{name: "配置非法", err: pluginengine.ErrInvalidConfig, want: "插件配置不合法"},
		{name: "动作不存在", err: pluginengine.ErrUnknownAction, want: "插件动作不存在"},
		{name: "能力未接线", err: pluginengine.ErrServiceUnavailable, want: "插件能力未接线"},
		{name: "其他错误", err: errors.New("底层故障"), want: "插件操作失败"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			pluginFailure(recorder, test.err)
			body := decodeResponseBody(t, recorder.Body.Bytes())
			if body.Success {
				t.Fatalf("失败路径不应返回 success: %+v", body)
			}
			if !strings.Contains(body.Message, test.want) {
				t.Fatalf("message = %q, want 含 %q", body.Message, test.want)
			}
		})
	}
}

func TestPluginFieldViewWithOptions(t *testing.T) {
	view := pluginFieldView(pluginengine.Field{
		Key:      "mode",
		Label:    "模式",
		Type:     pluginengine.FieldSelect,
		Required: true,
		Help:     "选择模式",
		Default:  "a",
		Options:  []pluginengine.Option{{Value: "a", Label: "A"}},
	})
	if view["type"] != "select" || view["required"] != true {
		t.Fatalf("view = %#v", view)
	}
	options, ok := view["options"].([]map[string]string)
	if !ok || len(options) != 1 || options[0]["value"] != "a" || options[0]["label"] != "A" {
		t.Fatalf("options = %#v", view["options"])
	}
}

func TestDecodePluginBodyRejectsMissingBody(t *testing.T) {
	var target map[string]any
	if err := decodePluginBody(nil, &target); err == nil {
		t.Fatalf("nil 请求应当失败")
	}
	if err := decodePluginBody(&http.Request{}, &target); err == nil {
		t.Fatalf("没有 Body 应当失败")
	}
}

func TestPluginActionBranches(t *testing.T) {
	ctx := context.Background()
	deps := Dependencies{Plugins: newPluginHTTPRegistry(t, pluginengine.Services{
		SignInAll: func(context.Context, pluginengine.RunInput) (pluginengine.Result, error) {
			return pluginengine.Result{Message: "已入队：站点签到"}, nil
		},
	})}
	if err := deps.Plugins.Install(ctx, pluginengine.PluginSiteSignIn); err != nil {
		t.Fatalf("安装失败: %v", err)
	}
	if err := deps.Plugins.Enable(ctx, pluginengine.PluginSiteSignIn); err != nil {
		t.Fatalf("启用失败: %v", err)
	}

	t.Run("带请求体的动作会被解码", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		request := pluginRouteRequest(http.MethodPost, `{"scope":"all"}`, pluginengine.PluginSiteSignIn)
		if request.ContentLength == 0 {
			t.Fatalf("测试前提不成立：请求体应为非空")
		}
		deps.runPluginAction(recorder, request)
		var result struct {
			Success bool   `json:"success"`
			Message string `json:"message"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil {
			t.Fatalf("解析失败: %v", err)
		}
		if !result.Success || !strings.Contains(result.Message, "已入队") {
			t.Fatalf("result = %+v", result)
		}
	})

	t.Run("坏请求体被拒绝", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		deps.runPluginAction(recorder, pluginRouteRequest(http.MethodPost, `{`, pluginengine.PluginSiteSignIn))
		if body := decodeResponseBody(t, recorder.Body.Bytes()); body.Success {
			t.Fatalf("坏请求体不应成功: %+v", body)
		}
	})

	t.Run("未知动作被拒绝", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		request := pluginRouteRequest(http.MethodPost, "", pluginengine.PluginSiteSignIn)
		request.URL.RawQuery = "action=explode"
		deps.runPluginAction(recorder, request)
		body := decodeResponseBody(t, recorder.Body.Bytes())
		if body.Success || !strings.Contains(body.Message, "插件动作不存在") {
			t.Fatalf("body = %+v", body)
		}
	})

	t.Run("未安装时拒绝执行", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		deps.runPluginAction(recorder, pluginRouteRequest(http.MethodPost, "", pluginengine.PluginCloudBackup))
		body := decodeResponseBody(t, recorder.Body.Bytes())
		if body.Success || !strings.Contains(body.Message, "插件未安装") {
			t.Fatalf("body = %+v", body)
		}
	})
}

func TestPluginInstallUnknownIDAndConfigOnUninstalled(t *testing.T) {
	deps := Dependencies{Plugins: newPluginHTTPRegistry(t, pluginengine.Services{})}

	recorder := httptest.NewRecorder()
	deps.installPlugin(recorder, pluginRouteRequest(http.MethodGet, "", "ghost"))
	if body := decodeResponseBody(t, recorder.Body.Bytes()); body.Success || !strings.Contains(body.Message, "插件不存在") {
		t.Fatalf("body = %+v", body)
	}

	recorder = httptest.NewRecorder()
	deps.savePluginConfig(recorder, pluginRouteRequest(http.MethodPut, `{"keep":1}`, pluginengine.PluginCloudBackup))
	if body := decodeResponseBody(t, recorder.Body.Bytes()); body.Success || !strings.Contains(body.Message, "插件未安装") {
		t.Fatalf("body = %+v", body)
	}

	recorder = httptest.NewRecorder()
	deps.reloadPlugin(recorder, pluginRouteRequest(http.MethodGet, "", "ghost"))
	if body := decodeResponseBody(t, recorder.Body.Bytes()); body.Success {
		t.Fatalf("body = %+v", body)
	}
}

// failingPluginStore 让 Restore/写入失败，用于覆盖「状态存储故障」这一层。
type failingPluginStore struct{ err error }

func (f failingPluginStore) Load(context.Context) (map[string]pluginengine.PersistedState, error) {
	return nil, f.err
}

func (f failingPluginStore) Save(context.Context, map[string]pluginengine.PersistedState) error {
	return f.err
}

func TestPluginInstalledIDList(t *testing.T) {
	deps := Dependencies{Plugins: newPluginHTTPRegistry(t, pluginengine.Services{})}
	if err := deps.Plugins.Install(context.Background(), pluginengine.PluginSiteSignIn); err != nil {
		t.Fatalf("安装失败: %v", err)
	}
	recorder := httptest.NewRecorder()
	deps.listInstalledPlugins(recorder, httptest.NewRequest(http.MethodGet, "/plugin/installed", nil))
	var ids []string
	if err := json.Unmarshal(recorder.Body.Bytes(), &ids); err != nil {
		t.Fatalf("解析: %v", err)
	}
	if len(ids) != 1 || ids[0] != pluginengine.PluginSiteSignIn {
		t.Fatalf("ids = %#v", ids)
	}
}

func TestPluginMutationErrors(t *testing.T) {
	deps := Dependencies{Plugins: newPluginHTTPRegistry(t, pluginengine.Services{})}
	cases := []struct {
		name    string
		handler http.HandlerFunc
		method  string
	}{
		{name: "卸载不存在", handler: deps.uninstallPlugin, method: http.MethodDelete},
		{name: "重置不存在", handler: deps.resetPlugin, method: http.MethodGet},
		{name: "表单不存在", handler: deps.pluginForm, method: http.MethodGet},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			test.handler(recorder, pluginRouteRequest(test.method, "", "ghost"))
			body := decodeResponseBody(t, recorder.Body.Bytes())
			if body.Success || !strings.Contains(body.Message, "插件不存在") {
				t.Fatalf("body = %+v", body)
			}
		})
	}
}

func TestPluginReloadWithBrokenStore(t *testing.T) {
	store := failingPluginStore{err: errors.New("设置表不可用")}
	registry := pluginengine.NewRegistry(store)
	if err := registry.RegisterAll(pluginengine.BuiltinDefinitions(pluginengine.Services{})); err != nil {
		t.Fatalf("注册失败: %v", err)
	}
	deps := Dependencies{Plugins: registry}
	recorder := httptest.NewRecorder()
	deps.reloadPlugin(recorder, pluginRouteRequest(http.MethodGet, "", pluginengine.PluginSiteSignIn))
	body := decodeResponseBody(t, recorder.Body.Bytes())
	if body.Success {
		t.Fatalf("存储故障时不应成功: %+v", body)
	}
}

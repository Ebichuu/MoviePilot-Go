//go:build integration

package httpapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/moviepilot-go/moviepilot-go/internal/auth"
	"github.com/moviepilot-go/moviepilot-go/internal/command"
	"github.com/moviepilot-go/moviepilot-go/internal/database"
)

// TestSettingsHandlersIntegration 覆盖系统设置写入：通用键、坏 JSON、
// 通知模板与发送时段的重载钩子（含重载失败只记日志）。
func TestSettingsHandlersIntegration(t *testing.T) {
	deps := newHTTPAPIIntegrationDeps(t)
	deps.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	claims := auth.Claims{Username: "admin", SuperUser: true}
	seedHTTPAPIUser(t, deps, "admin", true)

	ctx := t.Context()
	recorder := httptest.NewRecorder()
	deps.setSetting(recorder, httpapiJSONRequest(http.MethodPut, "/setting/custom_key",
		map[string]string{"key": "custom_key"}, claims, `{"limit":5}`))
	if body := decodeResponseBody(t, recorder.Body.Bytes()); !body.Success {
		t.Fatalf("写入设置 = %+v", body)
	}
	var stored map[string]any
	found, err := deps.Store.GetSetting(ctx, "custom_key", &stored)
	if err != nil || !found {
		t.Fatalf("读取设置 = (%v, %v)", found, err)
	}
	if value := fmt.Sprint(stored["limit"]); value != "5" {
		t.Fatalf("设置值 = %q, want 5", value)
	}

	recorder = httptest.NewRecorder()
	deps.setSetting(recorder, httpapiJSONRequest(http.MethodPut, "/setting/custom_key",
		map[string]string{"key": "custom_key"}, claims, `{bad`))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("坏 JSON 状态码 = %d", recorder.Code)
	}

	var templateReloads, sendTimeReloads int
	deps.ReloadNotificationTemplates = func(context.Context) error {
		templateReloads++
		return errors.New("重载失败")
	}
	deps.ReloadNotificationSendTime = func(context.Context) error {
		sendTimeReloads++
		return errors.New("重载失败")
	}

	recorder = httptest.NewRecorder()
	deps.setSetting(recorder, httpapiJSONRequest(http.MethodPut, "/setting/templates",
		map[string]string{"key": database.NotificationTemplatesSettingKey}, claims, `[]`))
	if body := decodeResponseBody(t, recorder.Body.Bytes()); !body.Success || templateReloads != 1 {
		t.Fatalf("通知模板设置 = (%+v, reloads=%d)", body, templateReloads)
	}

	recorder = httptest.NewRecorder()
	deps.setSetting(recorder, httpapiJSONRequest(http.MethodPut, "/setting/send_time",
		map[string]string{"key": database.NotificationSendTimeSettingKey}, claims, `{}`))
	if body := decodeResponseBody(t, recorder.Body.Bytes()); !body.Success || sendTimeReloads != 1 {
		t.Fatalf("发送时段设置 = (%+v, reloads=%d)", body, sendTimeReloads)
	}
}

// TestPluginFolderStoreIntegration 覆盖插件文件夹在真实设置存储上的读写往返。
func TestPluginFolderStoreIntegration(t *testing.T) {
	deps := newHTTPAPIIntegrationDeps(t)
	claims := auth.Claims{Username: "admin", SuperUser: true}
	seedHTTPAPIUser(t, deps, "admin", true)
	request := httpapiClaimsRequest(http.MethodPost, "/plugin/folders", nil, claims)

	if err := deps.storePluginFolders(request, map[string][]string{
		"默认": {"plugin-a"}, "空目录": nil,
	}); err != nil {
		t.Fatalf("写入插件文件夹 = %v", err)
	}
	folders, err := deps.loadPluginFolders(request)
	if err != nil {
		t.Fatalf("读取插件文件夹 = %v", err)
	}
	if len(folders["默认"]) != 1 || folders["默认"][0] != "plugin-a" {
		t.Fatalf("默认文件夹 = %#v", folders)
	}
	if plugins, ok := folders["空目录"]; !ok || len(plugins) != 0 {
		t.Fatalf("空目录应保持空列表 = %#v", folders)
	}

	// 从未写入过的键返回空 map 而不是报错。
	fresh := newHTTPAPIIntegrationDeps(t)
	empty, err := fresh.loadPluginFolders(httpapiClaimsRequest(http.MethodGet, "/plugin/folders", nil, claims))
	if err != nil || len(empty) != 0 {
		t.Fatalf("首次读取 = (%#v, %v)", empty, err)
	}
}

// TestSubscribeCommandIntegration 覆盖订阅命令在真实订阅存储上的执行。
func TestSubscribeCommandIntegration(t *testing.T) {
	deps := newHTTPAPIIntegrationDeps(t)
	seedHTTPAPIUser(t, deps, "admin", true)

	result, err := deps.runSubscribeCommand(t.Context(), command.Request{Username: "admin", Admin: true})
	if err != nil {
		t.Fatalf("执行订阅命令 = %v", err)
	}
	if result.Text != "共 0 个订阅" {
		t.Fatalf("空订阅命令文本 = %q", result.Text)
	}
	if details, ok := result.Details["total"]; !ok || details != 0 {
		t.Fatalf("命令明细 = %#v", result.Details)
	}

	// 普通用户同样可以执行，只是范围仅限本人。
	if _, err := deps.runSubscribeCommand(t.Context(), command.Request{Username: "admin"}); err != nil {
		t.Fatalf("非管理员执行订阅命令 = %v", err)
	}
}

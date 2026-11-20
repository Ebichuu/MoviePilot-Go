//go:build integration

package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/moviepilot-go/moviepilot-go/internal/auth"
	"github.com/moviepilot-go/moviepilot-go/internal/database"
)

func settingRequest(method, key, body string) *http.Request {
	return httpapiJSONRequest(method, "/api/v2/system/setting/"+key, map[string]string{"key": key}, auth.Claims{}, body)
}

// TestSetSettingReloadHooksIntegration 覆盖「写入后按 key 触发运行态重载」的两条钩子，
// 其中模板重载失败只记日志、不影响写入结果。
func TestSetSettingReloadHooksIntegration(t *testing.T) {
	deps := newHTTPAPIIntegrationDeps(t)
	deps.Logger = discardHTTPAPILogger()
	var templatesCalls, sendTimeCalls int
	deps.ReloadNotificationTemplates = func(context.Context) error {
		templatesCalls++
		return errors.New("boom")
	}
	deps.ReloadNotificationSendTime = func(context.Context) error {
		sendTimeCalls++
		return nil
	}

	recorder := httptest.NewRecorder()
	deps.setSetting(recorder, settingRequest(http.MethodPut, database.NotificationTemplatesSettingKey, `[]`))
	if recorder.Code != http.StatusOK || templatesCalls != 1 || sendTimeCalls != 0 {
		t.Fatalf("模板设置写入 = %d，重载次数 = (%d,%d)", recorder.Code, templatesCalls, sendTimeCalls)
	}

	recorder = httptest.NewRecorder()
	deps.setSetting(recorder, settingRequest(http.MethodPut, database.NotificationSendTimeSettingKey,
		`{"enabled":false,"start":"08:00","end":"22:00"}`))
	if recorder.Code != http.StatusOK || sendTimeCalls != 1 {
		t.Fatalf("发送时段写入 = %d，重载次数 = (%d,%d)", recorder.Code, sendTimeCalls, templatesCalls)
	}

	// 与通知无关的 key 不触发任何重载。
	recorder = httptest.NewRecorder()
	deps.setSetting(recorder, settingRequest(http.MethodPut, "CustomSetting", `{"a":1}`))
	if recorder.Code != http.StatusOK || sendTimeCalls != 1 || templatesCalls != 1 {
		t.Fatalf("普通设置写入 = %d，重载次数 = (%d,%d)", recorder.Code, templatesCalls, sendTimeCalls)
	}
	recorder = httptest.NewRecorder()
	deps.getSetting(recorder, settingRequest(http.MethodGet, "CustomSetting", ""))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"a":1`) {
		t.Fatalf("普通设置读回 = %d %s", recorder.Code, recorder.Body.String())
	}

	// 未配置的 key 读回空对象，而不是 404。
	recorder = httptest.NewRecorder()
	deps.getSetting(recorder, settingRequest(http.MethodGet, "MissingSetting", ""))
	if recorder.Code != http.StatusOK || strings.TrimSpace(recorder.Body.String()) != `{"success":true,"data":{}}` {
		t.Fatalf("未配置设置读回 = %d %s", recorder.Code, recorder.Body.String())
	}

	// 请求体不是 JSON 时直接 400，不写库。
	recorder = httptest.NewRecorder()
	deps.setSetting(recorder, settingRequest(http.MethodPut, "BrokenSetting", "nope"))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("非法设置值 = %d", recorder.Code)
	}
}

// TestGetSettingDispatchesIntegration 覆盖三个专用设置 key 的分派：
// 通知、下载器与媒体服务器各自走专用 handler，而不是落库读取。
func TestGetSettingDispatchesIntegration(t *testing.T) {
	deps := newHTTPAPIIntegrationDeps(t)
	// 三个专用 key 的写入体各自不同：通知是对象，下载器与媒体服务器是列表。
	bodies := map[string]string{
		notificationSettingsKey:         `{}`,
		database.DownloadersSettingKey:  `[]`,
		database.MediaServersSettingKey: `[]`,
	}
	for _, key := range []string{
		notificationSettingsKey,
		database.DownloadersSettingKey,
		database.MediaServersSettingKey,
	} {
		t.Run(key, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			deps.getSetting(recorder, settingRequest(http.MethodGet, key, ""))
			if recorder.Code != http.StatusOK {
				t.Fatalf("读取 %s = %d（%s）", key, recorder.Code, recorder.Body.String())
			}
		})
		recorder := httptest.NewRecorder()
		deps.setSetting(recorder, settingRequest(http.MethodPut, key, bodies[key]))
		if recorder.Code != http.StatusOK {
			t.Fatalf("写入 %s = %d（%s）", key, recorder.Code, recorder.Body.String())
		}
	}
}

// TestSetSettingTMDBKeyReloadsMetadataIntegration 确认媒体源密钥/域名写入后会触发
// 热加载（对齐 V2 CONFIG_WATCH），且热加载失败不影响设置本身的写入结果。
func TestSetSettingTMDBKeyReloadsMetadataIntegration(t *testing.T) {
	deps := newHTTPAPIIntegrationDeps(t)
	deps.Logger = discardHTTPAPILogger()
	reloads := 0
	deps.ReloadMetadata = func(context.Context) error {
		reloads++
		return nil
	}

	recorder := httptest.NewRecorder()
	deps.setSetting(recorder, settingRequest(http.MethodPut, database.TMDBAPIKeySettingKey, `"v3-key"`))
	if recorder.Code != http.StatusOK || reloads != 1 {
		t.Fatalf("TMDB 密钥写入 = %d，热加载次数 = %d", recorder.Code, reloads)
	}

	recorder = httptest.NewRecorder()
	deps.setSetting(recorder, settingRequest(http.MethodPut, database.TMDBAPIDomainSettingKey, `"api.themoviedb.org"`))
	if recorder.Code != http.StatusOK || reloads != 2 {
		t.Fatalf("TMDB 域名写入 = %d，热加载次数 = %d", recorder.Code, reloads)
	}

	// 其他 key 不该触发媒体源热加载。
	recorder = httptest.NewRecorder()
	deps.setSetting(recorder, settingRequest(http.MethodPut, "CustomSetting", `{"a":1}`))
	if recorder.Code != http.StatusOK || reloads != 2 {
		t.Fatalf("普通设置写入 = %d，热加载次数 = %d", recorder.Code, reloads)
	}

	// 热加载失败只记日志：设置已经写进库，响应仍是成功。
	deps.ReloadMetadata = func(context.Context) error {
		reloads++
		return errors.New("boom")
	}
	recorder = httptest.NewRecorder()
	deps.setSetting(recorder, settingRequest(http.MethodPut, database.TMDBAPIKeySettingKey, `"v4-token"`))
	if recorder.Code != http.StatusOK || reloads != 3 {
		t.Fatalf("热加载失败时写入 = %d，热加载次数 = %d", recorder.Code, reloads)
	}
}

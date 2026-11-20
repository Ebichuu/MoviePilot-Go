package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/moviepilot-go/moviepilot-go/internal/config"
)

func publicSettingRequest(key string) *http.Request {
	request := httptest.NewRequest("GET", "/api/v1/system/setting/"+key, nil)
	routeContext := chi.NewRouteContext()
	routeContext.URLParams.Add("key", key)
	return request.WithContext(context.WithValue(request.Context(), chi.RouteCtxKey, routeContext))
}

// 公开配置接口是白名单：非白名单键必须 404，且响应体不能带出任何配置内容。
func TestPublicSettingRejectsKeysOutsideWhitelist(t *testing.T) {
	deps := Dependencies{Config: config.Config{Environment: "test", MaxRequestBody: 1 << 20}}
	// 这些键要么是敏感配置、要么根本不存在：都不允许公开读取。
	for _, key := range []string{"AppSettings", "TokenConfig", "UserMFA:admin", "NotificationConfig", "不存在的键"} {
		recorder := httptest.NewRecorder()
		deps.publicSetting(recorder, publicSettingRequest(key))
		if recorder.Code != http.StatusNotFound {
			t.Fatalf("%q 应返回 404，实际 %d: %s", key, recorder.Code, recorder.Body.String())
		}
		if body := recorder.Body.String(); strings.Contains(body, `"success":true`) {
			t.Fatalf("%q 不应返回成功信封: %s", key, body)
		}
	}
}

func TestPublicSettingServesWhitelistedKeyWithoutStore(t *testing.T) {
	deps := Dependencies{Config: config.Config{Environment: "test", MaxRequestBody: 1 << 20}}
	recorder := httptest.NewRecorder()
	deps.publicSetting(recorder, publicSettingRequest("Directories"))
	if recorder.Code != http.StatusOK {
		t.Fatalf("白名单键应返回 200，实际 %d: %s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	if !strings.Contains(body, `"success":true`) || !strings.Contains(body, `"value":null`) {
		t.Fatalf("没有存储时 value 应为 null: %s", body)
	}
}

// 白名单必须与 V2 的公开配置集合一致：这里锁住当前集合，任何新增键都需要显式评审。
func TestPublicSettingWhitelistContents(t *testing.T) {
	expected := []string{
		"Directories", "Storages", "IndexerSites", "EpisodeFormatRuleTable",
		"DefaultMovieSubscribeConfig", "DefaultTvSubscribeConfig", "FollowSubscribers",
	}
	if len(publicSystemConfigKeys) != len(expected) {
		t.Fatalf("白名单数量变化（%d），请确认是否真的要把新键公开", len(publicSystemConfigKeys))
	}
	for _, key := range expected {
		if _, ok := publicSystemConfigKeys[key]; !ok {
			t.Fatalf("白名单缺少 %q", key)
		}
	}
}

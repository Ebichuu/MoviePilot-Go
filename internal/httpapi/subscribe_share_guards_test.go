package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/moviepilot-go/moviepilot-go/internal/auth"
)

// TestSubscribeShareHandlerGuards 覆盖分享/关注各端点的两档守卫：
// 没有登录态时给出「未登录」，有登录态但存储缺失时给出稳定降级响应。
func TestSubscribeShareHandlerGuards(t *testing.T) {
	type endpoint struct {
		name       string
		method     string
		call       func(http.ResponseWriter, *http.Request)
		noStore    string
		noStoreRaw bool
	}
	endpoints := []endpoint{
		{name: "分享列表", method: http.MethodGet, call: Dependencies{}.listSubscribeShares, noStoreRaw: true},
		{name: "删除分享", method: http.MethodDelete, call: Dependencies{}.deleteSubscribeShare, noStore: "存储未配置"},
		{name: "分享统计", method: http.MethodGet, call: Dependencies{}.subscribeShareStatistics, noStoreRaw: true},
		{name: "关注列表", method: http.MethodGet, call: Dependencies{}.listSubscribeFollows, noStoreRaw: true},
		{name: "关注分享", method: http.MethodPost, call: Dependencies{}.followSubscribeShare, noStore: "存储未配置"},
		{name: "取消关注", method: http.MethodDelete, call: Dependencies{}.unfollowSubscribeShare, noStore: "存储未配置"},
		{name: "分享转订阅", method: http.MethodPost, call: Dependencies{}.forkSubscribeShare, noStore: "存储未配置"},
	}
	claims := auth.Claims{Username: "alice"}
	for _, item := range endpoints {
		t.Run(item.name+"/未登录", func(t *testing.T) {
			request := httptest.NewRequest(item.method, "/api/v1/subscribe/share", nil)
			recorder := httptest.NewRecorder()
			item.call(recorder, request)
			if !strings.Contains(recorder.Body.String(), "未登录") {
				t.Fatalf("响应 = %s", recorder.Body.String())
			}
		})
		t.Run(item.name+"/无存储", func(t *testing.T) {
			request := httptest.NewRequest(item.method, "/api/v1/subscribe/share", strings.NewReader(`{"share_id":1}`))
			request = request.WithContext(context.WithValue(request.Context(), claimsContextKey, claims))
			recorder := httptest.NewRecorder()
			item.call(recorder, request)
			body := strings.TrimSpace(recorder.Body.String())
			switch {
			case item.noStoreRaw && item.name == "分享统计":
				if !strings.Contains(body, `"total":0`) {
					t.Fatalf("统计降级响应 = %s", body)
				}
			case item.noStoreRaw:
				if body != "[]" {
					t.Fatalf("列表降级响应 = %s", body)
				}
			default:
				if !strings.Contains(body, item.noStore) {
					t.Fatalf("响应 = %s，期望包含 %q", body, item.noStore)
				}
			}
		})
	}

	// 订阅文件清单：ID 非法与存储缺失都返回空数组。
	recorder := httptest.NewRecorder()
	deps := Dependencies{}
	deps.subscribeFileList(recorder, siteParamRequest(http.MethodGet, "/api/v1/subscribe/files/abc", "subscribe_id", "abc", ""))
	if strings.TrimSpace(recorder.Body.String()) != "[]" {
		t.Fatalf("ID 非法文件清单 = %s", recorder.Body.String())
	}
	recorder = httptest.NewRecorder()
	deps.subscribeFileList(recorder, siteParamRequest(http.MethodGet, "/api/v1/subscribe/files/1", "subscribe_id", "1", ""))
	if strings.TrimSpace(recorder.Body.String()) != "[]" {
		t.Fatalf("无存储文件清单 = %s", recorder.Body.String())
	}
}

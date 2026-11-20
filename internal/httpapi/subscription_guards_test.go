package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/moviepilot-go/moviepilot-go/internal/auth"
	"github.com/moviepilot-go/moviepilot-go/internal/config"
	"github.com/moviepilot-go/moviepilot-go/internal/database"
)

func subscriptionRequestWith(method, body string, params map[string]string, query string) *http.Request {
	target := "/api/v1/subscribe/"
	if query != "" {
		target += "?" + query
	}
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	routeContext := chi.NewRouteContext()
	for name, value := range params {
		routeContext.URLParams.Add(name, value)
	}
	claims := auth.Claims{Username: "alice", SuperUser: true}
	ctx := context.WithValue(request.Context(), chi.RouteCtxKey, routeContext)
	ctx = context.WithValue(ctx, claimsContextKey, claims)
	return request.WithContext(ctx)
}

func subscriptionGuardDeps() Dependencies {
	return Dependencies{Config: config.Config{MaxRequestBody: 1 << 20}}
}

// TestCreateSubscriptionGuards 覆盖创建订阅在进存储前的三道校验。
func TestCreateSubscriptionGuards(t *testing.T) {
	deps := subscriptionGuardDeps()
	cases := []struct {
		name   string
		body   string
		code   int
		substr string
	}{
		{name: "JSON 非法", body: "nope", code: http.StatusBadRequest, substr: "订阅参数无效"},
		{name: "多个 JSON 值", body: `{"name":"某剧","type":"电视剧"} {"x":1}`, code: http.StatusBadRequest,
			substr: "订阅参数无效"},
		{name: "名称缺失", body: `{"type":"电视剧"}`, code: http.StatusOK, substr: "订阅名称或类型无效"},
		{name: "类型非法", body: `{"name":"某剧","type":"动画"}`, code: http.StatusOK, substr: "订阅名称或类型无效"},
		{name: "集数越界", body: `{"name":"某剧","type":"电视剧","total_episode":6000}`, code: http.StatusOK,
			substr: "订阅集数超出有效范围"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			deps.createSubscription(recorder,
				subscriptionRequestWith(http.MethodPost, tc.body, map[string]string{}, ""))
			if recorder.Code != tc.code {
				t.Fatalf("状态码 = %d，期望 %d（%s）", recorder.Code, tc.code, recorder.Body.String())
			}
			if !strings.Contains(recorder.Body.String(), tc.substr) {
				t.Fatalf("响应 = %s，期望包含 %q", recorder.Body.String(), tc.substr)
			}
		})
	}
}

// TestUpdateSubscriptionGuards 覆盖局部更新的四道校验。
func TestUpdateSubscriptionGuards(t *testing.T) {
	deps := subscriptionGuardDeps()
	cases := []struct {
		name   string
		body   string
		code   int
		substr string
	}{
		{name: "JSON 非法", body: "nope", code: http.StatusUnprocessableEntity, substr: "订阅参数无效"},
		{name: "多个 JSON 值", body: `{"id":1} {"x":1}`, code: http.StatusUnprocessableEntity,
			substr: "订阅参数无效"},
		{name: "ID 缺失", body: `{"name":"某剧"}`, code: http.StatusOK,
			substr: database.ErrSubscriptionNotFound.Error()},
		{name: "ID 非正", body: `{"id":0}`, code: http.StatusOK, substr: database.ErrSubscriptionNotFound.Error()},
		{name: "字段越界", body: `{"id":1,"total_episode":6000}`, code: http.StatusOK, substr: "订阅参数无效"},
		{name: "季号越界", body: `{"id":1,"season":1000}`, code: http.StatusOK, substr: "订阅参数无效"},
		{name: "没有存储", body: `{"id":1,"sites":[1,2]}`, code: http.StatusInternalServerError, substr: "更新订阅失败"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			deps.updateSubscription(recorder,
				subscriptionRequestWith(http.MethodPut, tc.body, map[string]string{}, ""))
			if recorder.Code != tc.code {
				t.Fatalf("状态码 = %d，期望 %d（%s）", recorder.Code, tc.code, recorder.Body.String())
			}
			if !strings.Contains(recorder.Body.String(), tc.substr) {
				t.Fatalf("响应 = %s，期望包含 %q", recorder.Body.String(), tc.substr)
			}
		})
	}
}

// TestSubscriptionIDGuards 覆盖订阅相关端点在解析 ID 阶段的 400。
func TestSubscriptionIDGuards(t *testing.T) {
	deps := subscriptionGuardDeps()
	cases := []struct {
		name   string
		call   func(http.ResponseWriter, *http.Request)
		params map[string]string
		query  string
		substr string
	}{
		{name: "删除订阅", call: deps.deleteSubscription,
			params: map[string]string{"subid": "abc"}, substr: "订阅ID无效"},
		{name: "删除历史", call: deps.deleteSubscriptionHistory,
			params: map[string]string{"history_id": "abc"}, substr: "订阅历史ID无效"},
		{name: "删除历史（负数）", call: deps.deleteSubscriptionHistory,
			params: map[string]string{"history_id": "-1"}, substr: "订阅历史ID无效"},
		{name: "更新状态", call: deps.updateSubscriptionStatus,
			params: map[string]string{"subid": "abc"}, substr: "订阅ID无效"},
		{name: "重置订阅", call: deps.resetSubscription,
			params: map[string]string{"subid": "abc"}, substr: "订阅ID无效"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			tc.call(recorder, subscriptionRequestWith(http.MethodPost, "", tc.params, tc.query))
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("状态码 = %d（%s）", recorder.Code, recorder.Body.String())
			}
			if !strings.Contains(recorder.Body.String(), tc.substr) {
				t.Fatalf("响应 = %s，期望包含 %q", recorder.Body.String(), tc.substr)
			}
		})
	}

	// 状态值非法：200 + 业务错误文案。
	recorder := httptest.NewRecorder()
	deps.updateSubscriptionStatus(recorder, subscriptionRequestWith(http.MethodPut, "",
		map[string]string{"subid": "1"}, "state=不存在"))
	if recorder.Code != http.StatusOK ||
		!strings.Contains(recorder.Body.String(), database.ErrInvalidSubscriptionState.Error()) {
		t.Fatalf("状态非法 = %d %s", recorder.Code, recorder.Body.String())
	}
}

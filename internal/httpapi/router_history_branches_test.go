package httpapi

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/moviepilot-go/moviepilot-go/internal/auth"
	"github.com/moviepilot-go/moviepilot-go/internal/database"
)

type subscriptionHistoryFake struct {
	items     []database.Subscription
	listErr   error
	deleteErr error
	deleted   bool
	lastMtype string
	lastPage  int
	lastCount int
}

func (f *subscriptionHistoryFake) ListCompletedSubscriptionsByType(
	_ context.Context, _ string, _ bool, mtype string, page, count int,
) ([]database.Subscription, error) {
	f.lastMtype, f.lastPage, f.lastCount = mtype, page, count
	return f.items, f.listErr
}

func (f *subscriptionHistoryFake) DeleteCompletedSubscription(context.Context, int64, string, bool) (bool, error) {
	return f.deleted, f.deleteErr
}

func discardHTTPAPILogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func historyRequest(target, paramValue string) *http.Request {
	request := httptest.NewRequest(http.MethodGet, target, nil)
	rctx := chi.NewRouteContext()
	if paramValue != "" {
		// 同时挂上订阅历史与订阅控制端点使用的路径参数名。
		rctx.URLParams.Add("history_id", paramValue)
		rctx.URLParams.Add("subid", paramValue)
	}
	ctx := context.WithValue(request.Context(), chi.RouteCtxKey, rctx)
	ctx = context.WithValue(ctx, claimsContextKey, auth.Claims{Username: "alice"})
	return request.WithContext(ctx)
}

// TestListSubscriptionHistoryBranches 覆盖分页校验、存储缺失、分页错误与正常返回。
func TestListSubscriptionHistoryBranches(t *testing.T) {
	cases := []struct {
		name     string
		deps     Dependencies
		target   string
		code     int
		contains string
	}{
		{name: "页码非法", deps: Dependencies{SubscriptionHistory: &subscriptionHistoryFake{}},
			target: "/api/v2/subscribe/history?page=abc", code: http.StatusBadRequest},
		{name: "数量超限", deps: Dependencies{SubscriptionHistory: &subscriptionHistoryFake{}},
			target: "/api/v2/subscribe/history?count=1000", code: http.StatusBadRequest},
		{name: "无存储", deps: Dependencies{},
			target: "/api/v2/subscribe/history", code: http.StatusInternalServerError},
		{name: "存储分页错误", deps: Dependencies{
			SubscriptionHistory: &subscriptionHistoryFake{listErr: database.ErrInvalidSubscriptionHistoryPage},
		}, target: "/api/v2/subscribe/history", code: http.StatusBadRequest},
		{name: "存储一般错误", deps: Dependencies{
			SubscriptionHistory: &subscriptionHistoryFake{listErr: errors.New("boom")},
			Logger:              discardHTTPAPILogger(),
		}, target: "/api/v2/subscribe/history", code: http.StatusInternalServerError},
		{name: "正常", deps: Dependencies{
			SubscriptionHistory: &subscriptionHistoryFake{items: []database.Subscription{
				{ID: 1, Name: "已完成的剧", MediaType: "电视剧"},
			}},
		}, target: "/api/v2/subscribe/history?page=2&count=10", code: http.StatusOK, contains: "已完成的剧"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			tc.deps.listSubscriptionHistory(recorder, historyRequest(tc.target, ""))
			if recorder.Code != tc.code {
				t.Fatalf("状态码 = %d，期望 %d（%s）", recorder.Code, tc.code, recorder.Body.String())
			}
			if tc.contains != "" && !strings.Contains(recorder.Body.String(), tc.contains) {
				t.Fatalf("响应缺少 %q: %s", tc.contains, recorder.Body.String())
			}
		})
	}

	// 分页参数确实传进了存储。
	store := &subscriptionHistoryFake{}
	recorder := httptest.NewRecorder()
	Dependencies{SubscriptionHistory: store}.
		listSubscriptionHistory(recorder, historyRequest("/api/v2/subscribe/history?page=3&count=7", ""))
	if store.lastPage != 3 || store.lastCount != 7 {
		t.Fatalf("分页参数 = (%d,%d)", store.lastPage, store.lastCount)
	}
}

// TestDeleteSubscriptionHistoryBranches 覆盖订阅历史删除的四条出口。
func TestDeleteSubscriptionHistoryBranches(t *testing.T) {
	cases := []struct {
		name    string
		deps    Dependencies
		id      string
		code    int
		success bool
	}{
		{name: "ID 非法", deps: Dependencies{SubscriptionHistory: &subscriptionHistoryFake{}},
			id: "abc", code: http.StatusBadRequest},
		{name: "无存储", deps: Dependencies{}, id: "1", code: http.StatusInternalServerError},
		{name: "删除失败", deps: Dependencies{
			SubscriptionHistory: &subscriptionHistoryFake{deleteErr: errors.New("boom")},
			Logger:              discardHTTPAPILogger(),
		}, id: "1", code: http.StatusInternalServerError},
		{name: "幂等成功", deps: Dependencies{
			SubscriptionHistory: &subscriptionHistoryFake{deleted: false},
		}, id: "1", code: http.StatusOK, success: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			tc.deps.deleteSubscriptionHistory(recorder, historyRequest("/api/v2/subscribe/history/1", tc.id))
			if recorder.Code != tc.code {
				t.Fatalf("状态码 = %d，期望 %d（%s）", recorder.Code, tc.code, recorder.Body.String())
			}
			if tc.success && !strings.Contains(recorder.Body.String(), `"success":true`) {
				t.Fatalf("响应 = %s", recorder.Body.String())
			}
		})
	}
}

// TestSubscriptionHandlersGuards 覆盖订阅控制类端点在不合法参数/缺存储时的稳定响应。
func TestSubscriptionHandlersGuards(t *testing.T) {
	recorder := httptest.NewRecorder()
	Dependencies{}.updateSubscriptionStatus(recorder, historyRequest("/api/v2/subscribe/status/abc?state=P", "abc"))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("状态更新 ID 非法 = %d", recorder.Code)
	}
	recorder = httptest.NewRecorder()
	Dependencies{}.updateSubscriptionStatus(recorder, historyRequest("/api/v2/subscribe/status/1?state=??", "1"))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), database.ErrInvalidSubscriptionState.Error()) {
		t.Fatalf("状态非法响应 = %d %s", recorder.Code, recorder.Body.String())
	}

	recorder = httptest.NewRecorder()
	Dependencies{}.resetSubscription(recorder, historyRequest("/api/v2/subscribe/reset/abc", "abc"))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("重置 ID 非法 = %d", recorder.Code)
	}
	recorder = httptest.NewRecorder()
	Dependencies{}.resetSubscription(recorder, historyRequest("/api/v2/subscribe/reset/1", "1"))
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("重置无存储 = %d", recorder.Code)
	}

	recorder = httptest.NewRecorder()
	Dependencies{}.deleteSubscription(recorder, historyRequest("/api/v2/subscribe/abc", "abc"))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("删除订阅 ID 非法 = %d", recorder.Code)
	}
}

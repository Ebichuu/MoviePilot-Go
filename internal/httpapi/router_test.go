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
	"github.com/golang-jwt/jwt/v5"

	"github.com/moviepilot-go/moviepilot-go/internal/auth"
	"github.com/moviepilot-go/moviepilot-go/internal/config"
	"github.com/moviepilot-go/moviepilot-go/internal/database"
)

func TestCreateSubscriptionRejectsInvalidBusinessInputAsV2Response(t *testing.T) {
	negativeSeason := -1
	tests := []struct {
		name        string
		body        string
		wantMessage string
	}{
		{name: "缺少名称", body: `{"type":"电影"}`, wantMessage: "订阅名称或类型无效"},
		{name: "类型无效", body: `{"name":"示例","type":"动漫"}`, wantMessage: "订阅名称或类型无效"},
		{name: "季号为负数", body: subscriptionRequestJSON(t, subscriptionRequest{Name: "示例", Type: "电视剧", Season: &negativeSeason}), wantMessage: "订阅季号不能为负数"},
		{name: "只有媒体来源", body: `{"name":"示例","type":"电影","media_source":"douban"}`, wantMessage: "订阅媒体来源和媒体 ID 必须同时提供"},
		{name: "集数为负数", body: `{"name":"示例","type":"电视剧","total_episode":-1}`, wantMessage: "订阅集数超出有效范围"},
		{name: "站点 ID 非法", body: `{"name":"示例","type":"电影","sites":[0]}`, wantMessage: "订阅站点 ID 无效"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			request := subscriptionHandlerRequest(tt.body)
			response := httptest.NewRecorder()
			Dependencies{Config: config.Config{MaxRequestBody: 1 << 20}}.createSubscription(response, request)

			if response.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d; body=%s", response.Code, http.StatusOK, response.Body.String())
			}
			var payload Response
			if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
				t.Fatalf("解析响应: %v", err)
			}
			if payload.Success || payload.Message != tt.wantMessage {
				t.Fatalf("response = %#v, want success=false message=%q", payload, tt.wantMessage)
			}
		})
	}
}

func TestCreateSubscriptionRejectsMalformedOrMultipleJSONValues(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "非法 JSON", body: `{"name":`},
		{name: "多个 JSON 值", body: `{"name":"示例","type":"电影"} {}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			Dependencies{Config: config.Config{MaxRequestBody: 1 << 20}}.createSubscription(response, subscriptionHandlerRequest(tt.body))
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d; body=%s", response.Code, http.StatusBadRequest, response.Body.String())
			}
		})
	}
}

func TestUpdateSubscriptionStatusRejectsInvalidControlStateAsV2Response(t *testing.T) {
	tests := []struct {
		name  string
		state string
	}{
		{name: "N 只用于展示", state: "N"},
		{name: "空状态", state: ""},
		{name: "小写状态", state: "r"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPut, "/api/v1/subscribe/status/42?state="+tt.state, nil)
			request = withRouteParam(request, "subid", "42")
			claims := auth.Claims{Username: "alice", RegisteredClaims: jwt.RegisteredClaims{Subject: "1"}}
			request = request.WithContext(context.WithValue(request.Context(), claimsContextKey, claims))
			response := httptest.NewRecorder()

			Dependencies{}.updateSubscriptionStatus(response, request)

			if response.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d; body=%s", response.Code, http.StatusOK, response.Body.String())
			}
			var payload Response
			if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
				t.Fatalf("解析响应: %v", err)
			}
			if payload.Success || payload.Message != "无效的订阅状态" {
				t.Fatalf("response = %#v, want success=false message=无效的订阅状态", payload)
			}
		})
	}
}

func TestResetSubscriptionRejectsInvalidID(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/api/v1/subscribe/reset/invalid", nil)
	request = withRouteParam(request, "subid", "invalid")
	request = request.WithContext(context.WithValue(request.Context(), claimsContextKey, auth.Claims{Username: "alice"}))
	response := httptest.NewRecorder()

	Dependencies{}.resetSubscription(response, request)

	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
	}
}

func TestUpdateSubscriptionRequestUsesV2FullReplacementDefaults(t *testing.T) {
	input := newSubscriptionUpdateRequest()
	if err := json.NewDecoder(strings.NewReader(`{"id":42,"name":"示例","type":"电影"}`)).Decode(&input); err != nil {
		t.Fatal(err)
	}
	if input.TotalEpisode == nil || *input.TotalEpisode != 0 || input.StartEpisode == nil || *input.StartEpisode != 0 ||
		input.SearchIMDBID == nil || *input.SearchIMDBID != 0 || input.Sites == nil || len(input.Sites) != 0 ||
		input.FilterGroups == nil || len(input.FilterGroups) != 0 || input.Keyword != nil {
		t.Fatalf("update defaults=%#v", input)
	}
	input = newSubscriptionUpdateRequest()
	if err := json.NewDecoder(strings.NewReader(`{"id":42,"name":"示例","type":"电视剧","total_episode":null}`)).Decode(&input); err != nil {
		t.Fatal(err)
	}
	if input.TotalEpisode != nil {
		t.Fatalf("explicit NULL total=%#v", input.TotalEpisode)
	}
}

func TestUpdateSubscriptionRejectsMalformedJSONAndMissingID(t *testing.T) {
	tests := []struct {
		name string
		body string
		want int
	}{
		{name: "malformed", body: `{`, want: http.StatusUnprocessableEntity},
		{name: "multiple values", body: `{"id":1} {}`, want: http.StatusUnprocessableEntity},
		{name: "missing id", body: `{"name":"示例","type":"电影"}`, want: http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPut, "/api/v1/subscribe/", strings.NewReader(tt.body))
			request = request.WithContext(context.WithValue(request.Context(), claimsContextKey, auth.Claims{Username: "alice"}))
			response := httptest.NewRecorder()
			Dependencies{Config: config.Config{MaxRequestBody: 1 << 20}}.updateSubscription(response, request)
			if response.Code != tt.want {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
}

func TestSubscriptionUpdateTotalNullnessMatchesV2Comparison(t *testing.T) {
	zero := 0
	tests := []struct {
		name   string
		config map[string]any
		total  *int
		want   bool
	}{
		{name: "omitted default zero differs from old null", config: map[string]any{"total_episode": nil}, total: &zero, want: true},
		{name: "explicit null differs from old zero", config: map[string]any{"total_episode": 0}, total: nil, want: true},
		{name: "null stays null", config: map[string]any{"total_episode": nil}, total: nil},
		{name: "zero stays zero", config: map[string]any{"total_episode": 0}, total: &zero},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := subscriptionUpdateTotalNullnessChanged(database.Subscription{Config: tt.config}, tt.total); got != tt.want {
				t.Fatalf("changed=%t want=%t", got, tt.want)
			}
		})
	}
}

func TestNormalizeSubscriptionIdentity(t *testing.T) {
	tests := []struct {
		name       string
		input      subscriptionRequest
		wantSource string
		wantID     string
	}{
		{name: "显式来源优先并规范化", input: subscriptionRequest{MediaSource: " DouBan ", MediaID: " 123 "}, wantSource: "douban", wantID: "123"},
		{name: "显式 TMDB 别名统一", input: subscriptionRequest{MediaSource: " TMDB ", MediaID: "42"}, wantSource: "themoviedb", wantID: "42"},
		{name: "TMDB", input: subscriptionRequest{TMDBID: 42}, wantSource: "themoviedb", wantID: "42"},
		{name: "豆瓣", input: subscriptionRequest{DoubanID: " db-1 "}, wantSource: "douban", wantID: "db-1"},
		{name: "媒体键别名", input: subscriptionRequest{MediaKey: "tmdb:99"}, wantSource: "themoviedb", wantID: "99"},
		{name: "不支持的媒体键", input: subscriptionRequest{MediaKey: "imdb:tt123"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotSource, gotID := normalizeSubscriptionIdentity(tt.input)
			if gotSource != tt.wantSource || gotID != tt.wantID {
				t.Fatalf("normalizeSubscriptionIdentity() = (%q, %q), want (%q, %q)", gotSource, gotID, tt.wantSource, tt.wantID)
			}
		})
	}
}

func TestListSubscriptionHistoryReturnsRawPaginatedArray(t *testing.T) {
	completedAt := time.Date(2026, time.August, 11, 9, 30, 45, 0, time.UTC)
	store := &fakeSubscriptionHistoryStore{
		items: []database.Subscription{{
			ID: 8, Username: "alice", Name: "示例剧", MediaType: "电视剧",
			CompletedAt: &completedAt, CreatedAt: completedAt.Add(-time.Hour), UpdatedAt: completedAt,
			Config: map[string]any{"poster": "https://example.invalid/poster.jpg"},
		}},
	}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/subscribe/history/%E7%94%B5%E8%A7%86%E5%89%A7?page=2&count=15", nil)
	request = withRouteParam(request, "mtype", "电视剧")
	request = request.WithContext(context.WithValue(request.Context(), claimsContextKey, auth.Claims{
		Username: "alice", RegisteredClaims: jwt.RegisteredClaims{Subject: "1"},
	}))
	response := httptest.NewRecorder()

	Dependencies{SubscriptionHistory: store}.listSubscriptionHistory(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", response.Code, http.StatusOK, response.Body.String())
	}
	var payload []map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("历史响应不是原始数组: %v; body=%s", err, response.Body.String())
	}
	if len(payload) != 1 || payload[0]["id"] != float64(8) || payload[0]["date"] != "2026-08-11 17:30:45" {
		t.Fatalf("payload = %#v", payload)
	}
	if store.username != "alice" || store.admin || store.mediaType != "电视剧" || store.page != 2 || store.count != 15 {
		t.Fatalf("store call = %#v", store)
	}
}

func TestListSubscriptionHistoryRejectsInvalidPagination(t *testing.T) {
	tests := []struct {
		name  string
		query string
	}{
		{name: "页码不是整数", query: "?page=x"},
		{name: "页码为零", query: "?page=0"},
		{name: "数量为负数", query: "?count=-1"},
		{name: "数量超过硬上限", query: "?count=101"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/api/v1/subscribe/history/电影"+tt.query, nil)
			request = withRouteParam(request, "mtype", "电影")
			response := httptest.NewRecorder()
			Dependencies{SubscriptionHistory: &fakeSubscriptionHistoryStore{}}.listSubscriptionHistory(response, request)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d; body=%s", response.Code, http.StatusBadRequest, response.Body.String())
			}
		})
	}
}

func TestDeleteSubscriptionHistoryIsIdempotentAndOwnerScoped(t *testing.T) {
	store := &fakeSubscriptionHistoryStore{}
	request := httptest.NewRequest(http.MethodDelete, "/api/v1/subscribe/history/11", nil)
	request = withRouteParam(request, "history_id", "11")
	request = request.WithContext(context.WithValue(request.Context(), claimsContextKey, auth.Claims{
		Username: "alice", RegisteredClaims: jwt.RegisteredClaims{Subject: "1"},
	}))
	response := httptest.NewRecorder()

	Dependencies{SubscriptionHistory: store}.deleteSubscriptionHistory(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", response.Code, http.StatusOK, response.Body.String())
	}
	var payload Response
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("解析响应: %v", err)
	}
	if !payload.Success || store.deletedID != 11 || store.username != "alice" || store.admin {
		t.Fatalf("payload=%#v store=%#v", payload, store)
	}
}

func subscriptionHandlerRequest(body string) *http.Request {
	request := httptest.NewRequest(http.MethodPost, "/api/v1/subscribe/", strings.NewReader(body))
	claims := auth.Claims{Username: "alice", RegisteredClaims: jwt.RegisteredClaims{Subject: "1"}}
	return request.WithContext(context.WithValue(request.Context(), claimsContextKey, claims))
}

func withRouteParam(request *http.Request, key, value string) *http.Request {
	routeContext := chi.NewRouteContext()
	routeContext.URLParams.Add(key, value)
	return request.WithContext(context.WithValue(request.Context(), chi.RouteCtxKey, routeContext))
}

func subscriptionRequestJSON(t *testing.T, input subscriptionRequest) string {
	t.Helper()
	encoded, err := json.Marshal(input)
	if err != nil {
		t.Fatalf("编码订阅请求: %v", err)
	}
	return string(encoded)
}

type fakeSubscriptionHistoryStore struct {
	items     []database.Subscription
	username  string
	admin     bool
	mediaType string
	page      int
	count     int
	deletedID int64
	listErr   error
	deleteErr error
}

func (s *fakeSubscriptionHistoryStore) ListCompletedSubscriptionsByType(
	_ context.Context,
	username string,
	admin bool,
	mediaType string,
	page int,
	count int,
) ([]database.Subscription, error) {
	s.username = username
	s.admin = admin
	s.mediaType = mediaType
	s.page = page
	s.count = count
	return s.items, s.listErr
}

func (s *fakeSubscriptionHistoryStore) DeleteCompletedSubscription(
	_ context.Context,
	id int64,
	username string,
	admin bool,
) (bool, error) {
	s.deletedID = id
	s.username = username
	s.admin = admin
	return false, s.deleteErr
}

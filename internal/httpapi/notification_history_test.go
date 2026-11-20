package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/moviepilot-go/moviepilot-go/internal/auth"
	"github.com/moviepilot-go/moviepilot-go/internal/config"
	"github.com/moviepilot-go/moviepilot-go/internal/notificationhistory"
)

func TestNotificationHistoryListV2Contract(t *testing.T) {
	t.Parallel()

	location := time.FixedZone("UTC+8", 8*60*60)
	publishedAt := time.Date(2026, time.August, 14, 4, 34, 56, 987000000, time.UTC)
	title := "整理完成"
	text := "文件已成功整理到媒体库"
	image := "https://example.invalid/poster.jpg"
	store := &notificationHistoryStoreFake{events: []notificationhistory.Event{
		{
			ID: 2, EventKey: "event-2", Channel: "telegram", MType: stringPointerForNotificationHTTP("整理入库"),
			Title: &title, Text: &text, Image: &image, Note: []byte(`{"tmdb_id":1}`),
			PublishedAt: &publishedAt, CreatedAt: publishedAt.Add(-time.Minute),
		},
		{
			ID: 1, EventKey: "event-1", Channel: "", PublishedAt: timePointerForNotificationHTTP(publishedAt.Add(-time.Hour)),
			CreatedAt: publishedAt.Add(-2 * time.Hour),
		},
	}}
	handler, accessToken := newNotificationHistoryTestHandler(t, store, location)

	response := performNotificationRequest(handler, http.MethodGet, "/api/v1/message/notification", "", accessToken)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", response.Code, http.StatusOK, response.Body.String())
	}
	if store.listedPage != 1 || store.listedCount != 20 {
		t.Fatalf("ListNotificationHistory page/count = %d/%d, want 1/20", store.listedPage, store.listedCount)
	}
	assertJSONEqual(t, response.Body.Bytes(), []byte(`[
		{
			"id":2,"channel":"telegram","source":null,"mtype":"整理入库","title":"整理完成",
			"text":"文件已成功整理到媒体库","image":"https://example.invalid/poster.jpg",
			"link":null,"userid":null,"reg_time":"2026-08-14 12:34:56","action":1,"note":{"tmdb_id":1}
		},
		{
			"id":1,"channel":"","source":null,"mtype":null,"title":null,"text":null,"image":null,
			"link":null,"userid":null,"reg_time":"2026-08-14 11:34:56","action":1,"note":{}
		}
	]`))
}

func TestNotificationHistoryListUsesLastQueryValue(t *testing.T) {
	t.Parallel()

	store := &notificationHistoryStoreFake{}
	handler, accessToken := newNotificationHistoryTestHandler(t, store, time.UTC)
	response := performNotificationRequest(
		handler,
		http.MethodGet,
		"/api/v1/message/notification?page=2&page=3&count=4&count=5",
		"",
		accessToken,
	)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", response.Code, http.StatusOK, response.Body.String())
	}
	if store.listedPage != 3 || store.listedCount != 5 {
		t.Fatalf("ListNotificationHistory page/count = %d/%d, want 3/5", store.listedPage, store.listedCount)
	}
	assertJSONEqual(t, response.Body.Bytes(), []byte(`[]`))
}

func TestNotificationHistoryListAcceptsV2UnboundedPositivePagination(t *testing.T) {
	t.Parallel()

	store := &notificationHistoryStoreFake{}
	handler, accessToken := newNotificationHistoryTestHandler(t, store, time.UTC)
	response := performNotificationRequest(
		handler,
		http.MethodGet,
		"/api/v1/message/notification?page=1000001&count=101",
		"",
		accessToken,
	)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", response.Code, http.StatusOK, response.Body.String())
	}
	if store.listedPage != 1_000_001 || store.listedCount != 101 {
		t.Fatalf("ListNotificationHistory page/count = %d/%d, want 1000001/101", store.listedPage, store.listedCount)
	}
}

func TestNotificationHistoryListRejectsInvalidPagination(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		query string
	}{
		{name: "页码不是整数", query: "?page=invalid"},
		{name: "页码为空", query: "?page="},
		{name: "页码为零", query: "?page=0"},
		{name: "条数为零", query: "?count=0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			store := &notificationHistoryStoreFake{}
			handler, accessToken := newNotificationHistoryTestHandler(t, store, time.UTC)
			response := performNotificationRequest(
				handler, http.MethodGet, "/api/v1/message/notification"+tt.query, "", accessToken,
			)
			if response.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want %d; body=%s", response.Code, http.StatusUnprocessableEntity, response.Body.String())
			}
			if store.listCalls != 0 {
				t.Fatalf("invalid pagination reached store %d times", store.listCalls)
			}
		})
	}
}

func TestNotificationHistoryListFailures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		store      NotificationHistoryStore
		wantStatus int
	}{
		{name: "存储未装配", wantStatus: http.StatusServiceUnavailable},
		{name: "存储失败", store: &notificationHistoryStoreFake{listErr: errors.New("database unavailable")}, wantStatus: http.StatusInternalServerError},
		{name: "损坏事件", store: &notificationHistoryStoreFake{events: []notificationhistory.Event{{ID: 1}}}, wantStatus: http.StatusInternalServerError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			handler, accessToken := newNotificationHistoryTestHandler(t, tt.store, time.UTC)
			response := performNotificationRequest(handler, http.MethodGet, "/api/v1/message/notification", "", accessToken)
			if response.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", response.Code, tt.wantStatus, response.Body.String())
			}
		})
	}
}

func TestNotificationHistoryClearV2Contract(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		query     string
		wantScope notificationhistory.Scope
	}{
		{name: "缺省为全部", wantScope: notificationhistory.ScopeAll},
		{name: "全部", query: "?scope=all", wantScope: notificationhistory.ScopeAll},
		{name: "系统", query: "?scope=system", wantScope: notificationhistory.ScopeSystem},
		{name: "媒体", query: "?scope=media", wantScope: notificationhistory.ScopeMedia},
		{name: "重复参数采用最后值", query: "?scope=system&scope=media", wantScope: notificationhistory.ScopeMedia},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			store := &notificationHistoryStoreFake{clearBefore: notificationhistory.ClearBefore{All: 11, System: 22, Media: 33}}
			handler, accessToken := newNotificationHistoryTestHandler(t, store, time.UTC)
			response := performNotificationRequest(
				handler, http.MethodDelete, "/api/v1/message/notification"+tt.query, "", accessToken,
			)
			if response.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d; body=%s", response.Code, http.StatusOK, response.Body.String())
			}
			if store.updatedScope != tt.wantScope || store.updatedBefore <= 0 {
				t.Fatalf("UpdateNotificationClearBefore scope/before = %q/%d", store.updatedScope, store.updatedBefore)
			}
			want := store.clearBefore
			assertJSONEqual(t, response.Body.Bytes(), []byte(`{
				"success":true,
				"data":{"clear_before":{"all":`+int64StringForNotificationHTTP(want.All)+`,"system":`+int64StringForNotificationHTTP(want.System)+`,"media":`+int64StringForNotificationHTTP(want.Media)+`}}
			}`))
		})
	}
}

func TestNotificationHistoryClearRejectsInvalidScope(t *testing.T) {
	t.Parallel()

	for _, query := range []string{"?scope=other", "?scope=", "?scope=ALL", "?scope=%20all"} {
		t.Run(query, func(t *testing.T) {
			t.Parallel()
			store := &notificationHistoryStoreFake{}
			handler, accessToken := newNotificationHistoryTestHandler(t, store, time.UTC)
			response := performNotificationRequest(handler, http.MethodDelete, "/api/v1/message/notification"+query, "", accessToken)
			if response.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want %d; body=%s", response.Code, http.StatusUnprocessableEntity, response.Body.String())
			}
			if store.updateCalls != 0 {
				t.Fatalf("invalid scope reached store %d times", store.updateCalls)
			}
		})
	}
}

func TestNotificationHistoryClearFailures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		store      NotificationHistoryStore
		wantStatus int
	}{
		{name: "存储未装配", wantStatus: http.StatusServiceUnavailable},
		{name: "存储失败", store: &notificationHistoryStoreFake{updateErr: errors.New("database unavailable")}, wantStatus: http.StatusInternalServerError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			handler, accessToken := newNotificationHistoryTestHandler(t, tt.store, time.UTC)
			response := performNotificationRequest(handler, http.MethodDelete, "/api/v1/message/notification", "", accessToken)
			if response.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", response.Code, tt.wantStatus, response.Body.String())
			}
		})
	}
}

type notificationHistoryStoreFake struct {
	events        []notificationhistory.Event
	clearBefore   notificationhistory.ClearBefore
	listErr       error
	updateErr     error
	listCalls     int
	updateCalls   int
	listedPage    int
	listedCount   int
	updatedScope  notificationhistory.Scope
	updatedBefore int64
}

func (s *notificationHistoryStoreFake) ListNotificationHistory(_ context.Context, page, count int) ([]notificationhistory.Event, error) {
	s.listCalls++
	s.listedPage = page
	s.listedCount = count
	return append([]notificationhistory.Event(nil), s.events...), s.listErr
}

func (s *notificationHistoryStoreFake) UpdateNotificationClearBefore(
	_ context.Context,
	scope notificationhistory.Scope,
	beforeMillis int64,
) (notificationhistory.ClearBefore, error) {
	s.updateCalls++
	s.updatedScope = scope
	s.updatedBefore = beforeMillis
	if s.updateErr != nil {
		return notificationhistory.ClearBefore{}, s.updateErr
	}
	switch scope {
	case notificationhistory.ScopeAll:
		s.clearBefore.All = beforeMillis
	case notificationhistory.ScopeSystem:
		s.clearBefore.System = beforeMillis
	case notificationhistory.ScopeMedia:
		s.clearBefore.Media = beforeMillis
	}
	return s.clearBefore, nil
}

func newNotificationHistoryTestHandler(
	t *testing.T,
	store NotificationHistoryStore,
	location *time.Location,
) (http.Handler, string) {
	t.Helper()
	authService := auth.NewService(nil, "notification-history-jwt-secret", "notification-history-resource-secret", time.Hour)
	accessToken := accessTokenForTest(t, authService, auth.User{ID: 7, Username: "alice"})
	return NewRouter(Dependencies{
		Config:              config.Config{MaxRequestBody: 1 << 20, Location: location},
		Auth:                authService,
		NotificationHistory: store,
	}), accessToken
}

func stringPointerForNotificationHTTP(value string) *string {
	return &value
}

func timePointerForNotificationHTTP(value time.Time) *time.Time {
	return &value
}

func int64StringForNotificationHTTP(value int64) string {
	return strconv.FormatInt(value, 10)
}

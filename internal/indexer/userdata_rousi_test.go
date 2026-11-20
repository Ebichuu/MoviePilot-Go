package indexer

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

type rousiReadAllErrorTransport struct {
	base         http.RoundTripper
	err          error
	readAllCalls int
}

func (t *rousiReadAllErrorTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.Method == http.MethodPost && request.URL.Path == "/api/messages/read-all" {
		t.readAllCalls++
		return nil, t.err
	}
	return t.base.RoundTrip(request)
}

func TestRousiAdapterFetchSiteUserDataReadsAndMarksMessages(t *testing.T) {
	requests := make([]string, 0, 4)
	readAllCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.Path)
		switch r.URL.Path {
		case "/api/v1/profile":
			if r.Header.Get("Authorization") != "Bearer fixture-passkey" {
				t.Fatalf("RousiPro profile 未使用 Passkey: %#v", r.Header)
			}
			if r.Method != http.MethodGet || r.URL.Query().Get("include_fields[user]") != "seeding_leeching_data" ||
				r.Header.Get("Content-Type") != "application/json" || r.Header.Get("Accept") != "application/json" {
				t.Fatalf("RousiPro profile 请求错误: method=%s query=%s headers=%#v", r.Method, r.URL.RawQuery, r.Header)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{
				"id": 77, "username": "rousi-user", "role_text": "Uploader",
				"registered_at": "2024-01-01T00:00:00Z",
				"uploaded":      3000, "downloaded": 1000, "ratio": 2.345, "karma": 987.6,
				"seeding_leeching_data": map[string]any{
					"seeding_count": 10, "seeding_size": 10_737_418_240,
					"leeching_count": 2, "leeching_size": 2_147_483_648,
				},
			}})
		case "/api/messages":
			if r.Header.Get("Authorization") != "Bearer fixture-message-token" || r.Header.Get("User-Agent") != "fixture-agent" {
				t.Fatalf("RousiPro messages 未使用独立 Token 或 UA: %#v", r.Header)
			}
			if r.Method != http.MethodGet || r.URL.Query().Get("page_size") != "100" || r.URL.Query().Get("unread_only") != "true" ||
				r.Header.Get("Accept") != "application/json, text/plain, */*" {
				t.Fatalf("RousiPro messages 请求错误: method=%s query=%s headers=%#v", r.Method, r.URL.RawQuery, r.Header)
			}
			if r.URL.Query().Get("page") == "0" {
				_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{
					"messages":    []any{map[string]any{"title": "第一页", "created_at": "2026-08-01T10:11:12+08:00", "content": "正文一"}},
					"total_pages": 2,
				}})
				return
			}
			if r.URL.Query().Get("page") != "1" {
				t.Fatalf("RousiPro 消息页码错误: %s", r.URL.RawQuery)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{
				"messages":    []any{map[string]any{"title": "第二页", "created_at": "2026-08-02 12:13:14", "content": "正文二"}},
				"total_pages": 2,
			}})
		case "/api/messages/read-all":
			if r.Header.Get("Authorization") != "Bearer fixture-message-token" || r.Header.Get("User-Agent") != "fixture-agent" {
				t.Fatalf("RousiPro read-all 未使用独立 Token 或 UA: %#v", r.Header)
			}
			if r.Method != http.MethodPost || r.Header.Get("Accept") != "application/json, text/plain, */*" {
				t.Fatalf("RousiPro read-all 请求错误: method=%s headers=%#v", r.Method, r.Header)
			}
			readAllCalls++
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("RousiPro 发生非预期请求: %s", r.URL.Path)
		}
	}))
	defer server.Close()

	adapter, err := NewRousiAdapter(newIndexerHTTPClient(t, 1), 14, "RousiPro", server.URL, map[string]any{
		"apikey": "fixture-passkey",
		"token":  "fixture-message-token",
		"ua":     "fixture-agent",
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := adapter.FetchSiteUserData(t.Context())
	if err != nil {
		t.Fatalf("FetchSiteUserData() error = %v", err)
	}
	if got.UserID != int64(77) || got.Username == nil || *got.Username != "rousi-user" ||
		got.UserLevel == nil || *got.UserLevel != "Uploader" || got.JoinAt == nil || *got.JoinAt != "2024-01-01 00:00:00" {
		t.Fatalf("RousiPro 用户基础字段映射错误: %#v", got)
	}
	if got.Upload != 3000 || got.Download != 1000 || got.Ratio != 2.35 || got.Bonus != 987.6 ||
		got.Seeding != 10 || got.SeedingSize != 10_737_418_240 || got.Leeching != 2 || got.LeechingSize != 2_147_483_648 {
		t.Fatalf("RousiPro 用户统计字段映射错误: %#v", got)
	}
	wantMessages := [][]any{
		{"第一页", "2026-08-01 10:11:12", "正文一"},
		{"第二页", "2026-08-02 12:13:14", "正文二"},
	}
	if got.MessageUnread != 2 || !reflect.DeepEqual(got.MessageUnreadContents, wantMessages) {
		t.Fatalf("RousiPro 未读消息映射错误: %#v", got)
	}
	if readAllCalls != 1 || !reflect.DeepEqual(requests, []string{
		"GET /api/v1/profile", "GET /api/messages", "GET /api/messages", "POST /api/messages/read-all",
	}) {
		t.Fatalf("RousiPro 请求顺序或 read-all 副作用错误: requests=%#v readAll=%d", requests, readAllCalls)
	}
}

func TestRousiAdapterFetchSiteUserDataPreservesBearerMessageToken(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		switch r.URL.Path {
		case "/api/v1/profile":
			if r.Header.Get("Authorization") != "Bearer fixture-passkey" {
				t.Fatalf("RousiPro profile 未使用 Passkey: %#v", r.Header)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{}})
		case "/api/messages":
			if r.Header.Get("Authorization") != "Bearer fixture-message-token" {
				t.Fatalf("RousiPro messages 重复添加 Bearer: %#v", r.Header)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{
				"messages": []any{}, "total_pages": 0,
			}})
		case "/api/messages/read-all":
			if r.Header.Get("Authorization") != "Bearer fixture-message-token" {
				t.Fatalf("RousiPro read-all 重复添加 Bearer: %#v", r.Header)
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("RousiPro 发生非预期请求: %s", r.URL.Path)
		}
	}))
	defer server.Close()

	adapter, err := NewRousiAdapter(newIndexerHTTPClient(t, 1), 14, "RousiPro", server.URL, map[string]any{
		"apikey": "fixture-passkey",
		"token":  "Bearer fixture-message-token",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.FetchSiteUserData(t.Context()); err != nil {
		t.Fatalf("FetchSiteUserData() error = %v", err)
	}
	if requests != 3 {
		t.Fatalf("RousiPro 请求次数 = %d, want 3", requests)
	}
}

func TestRousiAdapterFetchSiteUserDataSkipsMessagesWithoutToken(t *testing.T) {
	requests := make([]string, 0, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.Path)
		if r.URL.Path != "/api/v1/profile" {
			t.Fatalf("RousiPro 缺少 Token 时发生消息请求: %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer fixture-passkey" {
			t.Fatalf("RousiPro profile 未使用 Passkey: %#v", r.Header)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{
			"id": 77, "username": "rousi-user",
		}})
	}))
	defer server.Close()

	adapter, err := NewRousiAdapter(newIndexerHTTPClient(t, 1), 14, "RousiPro", server.URL, map[string]any{
		"apikey": "fixture-passkey",
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := adapter.FetchSiteUserData(t.Context())
	if err != nil {
		t.Fatalf("FetchSiteUserData() error = %v", err)
	}
	if got.UserID != int64(77) || got.Username == nil || *got.Username != "rousi-user" {
		t.Fatalf("RousiPro profile 映射错误: %#v", got)
	}
	if got.MessageUnread != 0 || len(got.MessageUnreadContents) != 0 {
		t.Fatalf("RousiPro 缺少 Token 时消息数据不为空: %#v", got)
	}
	if !reflect.DeepEqual(requests, []string{"GET /api/v1/profile"}) {
		t.Fatalf("RousiPro 缺少 Token 时请求序列错误: %#v", requests)
	}
}

func TestRousiAdapterFetchSiteUserDataSkipsMessagesWhenV2SettingDisabled(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path != "/api/v1/profile" {
			t.Fatalf("SITE_MESSAGE=false 时发生远端消息副作用: %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{
			"id": 77, "username": "rousi-user",
		}})
	}))
	defer server.Close()

	adapter, err := NewRousiAdapter(newIndexerHTTPClient(t, 1), 14, "RousiPro", server.URL, map[string]any{
		"apikey": "fixture-passkey", "token": "fixture-message-token",
	})
	if err != nil {
		t.Fatal(err)
	}
	coordinator, err := NewCoordinator(1)
	if err != nil {
		t.Fatal(err)
	}
	defer coordinator.Close()
	got, err := coordinator.FetchSiteUserData(t.Context(), adapter, SiteUserDataOptions{FetchMessages: false})
	if err != nil {
		t.Fatal(err)
	}
	if requests != 1 || got.UserID != int64(77) || got.MessageUnread != 0 || len(got.MessageUnreadContents) != 0 {
		t.Fatalf("requests=%d data=%#v", requests, got)
	}
}

func TestRousiAdapterFetchSiteUserDataPreservesBusinessErrorAndReadAll(t *testing.T) {
	readAllCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/profile":
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 401, "message": "Passkey 已失效"})
		case "/api/messages":
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{"messages": []any{}, "total_pages": 0}})
		case "/api/messages/read-all":
			readAllCalls++
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	adapter, err := NewRousiAdapter(newIndexerHTTPClient(t, 1), 1, "RousiPro", server.URL, map[string]any{
		"apikey": "fixture",
		"token":  "message-token",
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := adapter.FetchSiteUserData(t.Context())
	if err != nil {
		t.Fatalf("FetchSiteUserData() error = %v", err)
	}
	if got.ErrMsg == nil || *got.ErrMsg != "Passkey 已失效" || readAllCalls != 1 {
		t.Fatalf("RousiPro 业务错误或 read-all 副作用错误: data=%#v readAll=%d", got, readAllCalls)
	}
}

func TestRousiAdapterFetchSiteUserDataPreservesSnapshotWhenReadAllFails(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{name: "普通请求错误", err: errors.New("read-all failed")},
		{name: "上下文已取消", err: context.Canceled},
		{name: "上下文超过截止时间", err: context.DeadlineExceeded},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/profile":
					_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{
						"id": 77, "username": "rousi-user",
					}})
				case "/api/messages":
					_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{
						"messages": []any{map[string]any{
							"title": "站内信", "created_at": "2026-08-16T08:09:10+08:00", "content": "正文",
						}},
						"total_pages": 1,
					}})
				default:
					t.Fatalf("RousiPro 发生非预期请求: %s %s", r.Method, r.URL.Path)
				}
			}))
			defer server.Close()

			client := newIndexerHTTPClient(t, 1)
			transport := &rousiReadAllErrorTransport{base: client.client.Transport, err: test.err}
			client.client.Transport = transport
			adapter, err := NewRousiAdapter(client, 1, "RousiPro", server.URL, map[string]any{
				"apikey": "fixture-passkey",
				"token":  "fixture-message-token",
			})
			if err != nil {
				t.Fatal(err)
			}

			got, err := adapter.FetchSiteUserData(t.Context())
			if err != nil {
				t.Fatalf("FetchSiteUserData() error = %v", err)
			}
			wantMessages := [][]any{{"站内信", "2026-08-16 08:09:10", "正文"}}
			if got.UserID != int64(77) || got.Username == nil || *got.Username != "rousi-user" ||
				got.MessageUnread != 1 || !reflect.DeepEqual(got.MessageUnreadContents, wantMessages) {
				t.Fatalf("RousiPro read-all 失败后快照丢失: %#v", got)
			}
			if transport.readAllCalls != 1 {
				t.Fatalf("RousiPro read-all 调用次数 = %d, want 1", transport.readAllCalls)
			}
		})
	}
}

func TestRousiAdapterFetchSiteUserDataReturnsContextError(t *testing.T) {
	adapter, err := NewRousiAdapter(newIndexerHTTPClient(t, 1), 1, "RousiPro", "https://rousi.example", map[string]any{"apikey": "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = adapter.FetchSiteUserData(ctx)
	if !errors.Is(err, context.Canceled) || err != context.Canceled {
		t.Fatalf("FetchSiteUserData() error = %v, want context.Canceled", err)
	}
}

func TestRousiAdapterFetchSiteUserDataIgnoresNonContextRequestFailures(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "upstream unavailable", http.StatusBadGateway)
	}))
	defer server.Close()
	adapter, err := NewRousiAdapter(newIndexerHTTPClient(t, 1), 1, "RousiPro", server.URL, map[string]any{"apikey": "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := adapter.FetchSiteUserData(t.Context())
	if err != nil {
		t.Fatalf("FetchSiteUserData() error = %v", err)
	}
	if got.ErrMsg != nil || got.MessageUnread != 0 || len(got.MessageUnreadContents) != 0 {
		t.Fatalf("RousiPro 非上下文请求失败应保持空数据: %#v", got)
	}
}

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

func TestSunnyPTAdapterFetchSiteUserDataAndMessages(t *testing.T) {
	requests := make([]string, 0, 3)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatalf("SunnyPT 用户数据请求方法 = %s, want GET", r.Method)
		}
		if r.Header.Get("X-API-Key") != "fixture-key" || r.Header.Get("Accept") != "application/json" || r.Header.Get("User-Agent") != "MoviePilot-Test" {
			t.Fatalf("SunnyPT 用户数据请求头错误: %#v", r.Header)
		}
		requests = append(requests, r.URL.Path+"?"+r.URL.RawQuery)
		switch r.URL.Path {
		case "/api/v1/mp/profile":
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{
				"id": 1001, "username": "sunny", "level": "Power User",
				"registered_at": "2025-01-01T12:00:00+08:00",
				"uploaded":      1_099_511_627_776, "downloaded": 536_870_912_000,
				"ratio": 2.048, "bonus": 12345.6,
				"seeding_count": 30, "seeding_size": 2_147_483_648_000,
				"leeching_count": 1, "leeching_size": 10_737_418_240,
				"unread_messages": 2,
			}})
		case "/api/v1/mp/messages":
			if r.URL.Query().Get("unread_only") != "true" || r.URL.Query().Get("page_size") != "100" {
				t.Fatalf("SunnyPT 消息请求参数错误: %s", r.URL.RawQuery)
			}
			switch r.URL.Query().Get("page") {
			case "1":
				_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{
					"items": []any{
						map[string]any{"id": 9001, "title": "审核通过", "content": "种子已审核。", "created_at": "2026-07-21T16:30:00+08:00", "unread": true},
						map[string]any{"id": 9000, "title": "已读", "content": "忽略", "created_at": "2026-07-20T16:30:00+08:00", "unread": false},
					},
					"unread_count": 2, "has_more": true,
				}})
			case "2":
				_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{
					"items":        []any{map[string]any{"title": "新消息", "content": "第二页正文", "created_at": "2026-07-22 09:10:11", "unread": true}},
					"unread_count": 2, "has_more": false,
				}})
			default:
				t.Fatalf("SunnyPT 消息页码错误: %s", r.URL.RawQuery)
			}
		default:
			t.Fatalf("SunnyPT 发生非预期请求: %s", r.URL.Path)
		}
	}))
	defer server.Close()

	adapter, err := NewSunnyPTAdapter(newIndexerHTTPClient(t, 1), 13, "SunnyPT", server.URL, map[string]any{
		"api_url": server.URL + "/api/v1/mp", "apikey": "fixture-key", "ua": "MoviePilot-Test",
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := adapter.FetchSiteUserData(t.Context())
	if err != nil {
		t.Fatalf("FetchSiteUserData() error = %v", err)
	}
	if got.UserID != int64(1001) || got.Username == nil || *got.Username != "sunny" ||
		got.UserLevel == nil || *got.UserLevel != "Power User" || got.JoinAt == nil || *got.JoinAt != "2025-01-01 12:00:00" {
		t.Fatalf("SunnyPT 用户基础字段映射错误: %#v", got)
	}
	if got.Upload != 1_099_511_627_776 || got.Download != 536_870_912_000 || got.Ratio != 2.048 || got.Bonus != 12345.6 ||
		got.Seeding != 30 || got.SeedingSize != 2_147_483_648_000 || got.Leeching != 1 || got.LeechingSize != 10_737_418_240 {
		t.Fatalf("SunnyPT 用户统计字段映射错误: %#v", got)
	}
	wantMessages := [][]any{
		{"审核通过", "2026-07-21 16:30:00", "种子已审核。", "sunnypt-message:9001"},
		{"新消息", "2026-07-22 09:10:11", "第二页正文", nil},
	}
	if got.MessageUnread != 2 || !reflect.DeepEqual(got.MessageUnreadContents, wantMessages) {
		t.Fatalf("SunnyPT 未读消息映射错误: %#v", got)
	}
	if len(requests) != 3 {
		t.Fatalf("SunnyPT 请求数 = %d, want 3: %#v", len(requests), requests)
	}
}

func TestSunnyPTAdapterFetchSiteUserDataPreservesBusinessError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/mp/profile" {
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 401, "msg": "API Key 已失效"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{
			"items": []any{}, "unread_count": 0, "has_more": false,
		}})
	}))
	defer server.Close()
	adapter, err := NewSunnyPTAdapter(newIndexerHTTPClient(t, 1), 1, "SunnyPT", server.URL, map[string]any{
		"api_url": server.URL + "/api/v1/mp", "apikey": "fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := adapter.FetchSiteUserData(t.Context())
	if err != nil {
		t.Fatalf("FetchSiteUserData() error = %v", err)
	}
	if got.ErrMsg == nil || *got.ErrMsg != "API Key 已失效" {
		t.Fatalf("ErrMsg = %#v, want API Key 已失效", got.ErrMsg)
	}
}

func TestSunnyPTAdapterFetchSiteUserDataReturnsContextError(t *testing.T) {
	adapter, err := NewSunnyPTAdapter(newIndexerHTTPClient(t, 1), 1, "SunnyPT", "https://sunnypt.example", map[string]any{
		"api_url": "https://api.sunnypt.example/api/v1/mp", "apikey": "fixture",
	})
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

func TestSunnyPTAdapterFetchSiteUserDataMapsRequestFailureToErrMsg(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "upstream unavailable", http.StatusBadGateway)
	}))
	defer server.Close()
	adapter, err := NewSunnyPTAdapter(newIndexerHTTPClient(t, 1), 1, "SunnyPT", server.URL, map[string]any{
		"api_url": server.URL + "/api/v1/mp", "apikey": "fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := adapter.FetchSiteUserData(t.Context())
	if err != nil {
		t.Fatalf("FetchSiteUserData() error = %v", err)
	}
	if got.ErrMsg == nil || *got.ErrMsg != "获取未读消息失败，未收到 API 响应" {
		t.Fatalf("ErrMsg = %#v, want SunnyPT 消息请求失败", got.ErrMsg)
	}
}

func TestSunnyPTAdapterFetchSiteUserDataSkipsMessagesWhenV2SettingDisabled(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path != "/api/v1/mp/profile" {
			t.Fatalf("SITE_MESSAGE=false 时请求消息接口: %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{
			"id": 1, "unread_messages": 2,
		}})
	}))
	defer server.Close()
	adapter, err := NewSunnyPTAdapter(newIndexerHTTPClient(t, 1), 1, "SunnyPT", server.URL, map[string]any{
		"api_url": server.URL + "/api/v1/mp", "apikey": "fixture",
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
	if requests != 1 || got.MessageUnread != 2 || len(got.MessageUnreadContents) != 0 {
		t.Fatalf("requests=%d data=%#v", requests, got)
	}
}

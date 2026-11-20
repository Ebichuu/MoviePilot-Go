package indexer

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestYemaAdapterFetchSiteUserData(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/openApi/user/fetchBasicInfo.json" {
			t.Fatalf("YemaPT 用户数据请求 = %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "fixture-auth-key" ||
			r.Header.Get("Content-Type") != "application/json" ||
			r.Header.Get("Accept") != "application/json, text/plain, */*" ||
			r.Header.Get("User-Agent") != "MoviePilot-Test" {
			t.Fatalf("YemaPT 用户数据请求头错误: %#v", r.Header)
		}
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if len(request) != 0 {
			t.Fatalf("YemaPT 用户数据请求体 = %#v, want {}", request)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"data": map[string]any{
				"id": 10, "name": "yema-user", "level": 7,
				"registerTime":          "2024-05-01T00:00:00+08:00",
				"promotionUploadSize":   2_000_000,
				"promotionDownloadSize": 750_000,
				"bonus":                 "1000000.5",
			},
		})
	}))
	defer server.Close()

	adapter, err := NewYemaAdapter(newIndexerHTTPClient(t, 1), 7, "YemaPT", server.URL, map[string]any{
		"apikey": "fixture-auth-key", "ua": "MoviePilot-Test",
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := adapter.FetchSiteUserData(t.Context())
	if err != nil {
		t.Fatalf("FetchSiteUserData() error = %v", err)
	}
	if got.UserID != int64(10) || got.Username == nil || *got.Username != "yema-user" ||
		got.UserLevel == nil || *got.UserLevel != "7" || got.JoinAt == nil || *got.JoinAt != "2024-05-01 00:00:00" {
		t.Fatalf("YemaPT 用户基础字段映射错误: %#v", got)
	}
	if got.Upload != 2_000_000 || got.Download != 750_000 || got.Ratio != 2.67 || got.Bonus != 1_000_000.5 {
		t.Fatalf("YemaPT 用户流量字段映射错误: %#v", got)
	}
	if got.ErrMsg != nil || got.UpdatedDay == "" || got.SeedingInfo == nil || got.MessageUnreadContents == nil {
		t.Fatalf("YemaPT 用户数据默认字段错误: %#v", got)
	}
}

func TestYemaAdapterFetchSiteUserDataVisibleErrors(t *testing.T) {
	tests := []struct {
		name        string
		contentType string
		status      int
		body        string
		want        string
	}{
		{name: "business error", contentType: "application/json", status: http.StatusForbidden, body: `{"success":false,"errorMessage":"AuthKey 已失效"}`, want: "AuthKey 已失效"},
		{name: "business fallback", contentType: "application/json", body: `{"success":true}`, want: "获取用户信息失败"},
		{name: "invalid json", contentType: "application/json", body: `{`, want: "获取用户信息失败，开放 API 响应不是有效 JSON"},
		{name: "invalid structure", contentType: "application/json", body: `[]`, want: "获取用户信息失败，开放 API 响应结构无效"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", tt.contentType)
				if tt.status != 0 {
					w.WriteHeader(tt.status)
				}
				_, _ = w.Write([]byte(tt.body))
			}))
			defer server.Close()
			adapter, err := NewYemaAdapter(newIndexerHTTPClient(t, 1), 1, "Yema", server.URL, map[string]any{"apikey": "fixture"})
			if err != nil {
				t.Fatal(err)
			}
			got, err := adapter.FetchSiteUserData(t.Context())
			if err != nil {
				t.Fatalf("FetchSiteUserData() error = %v", err)
			}
			if got.ErrMsg == nil || *got.ErrMsg != tt.want {
				t.Fatalf("ErrMsg = %#v, want %q", got.ErrMsg, tt.want)
			}
		})
	}
}

func TestYemaAdapterFetchSiteUserDataReturnsContextError(t *testing.T) {
	adapter, err := NewYemaAdapter(newIndexerHTTPClient(t, 1), 1, "Yema", "https://example.test", map[string]any{"apikey": "fixture"})
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

func TestYemaAdapterFetchSiteUserDataMapsRequestFailureToErrMsg(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "upstream unavailable", http.StatusBadGateway)
	}))
	defer server.Close()
	adapter, err := NewYemaAdapter(newIndexerHTTPClient(t, 1), 1, "Yema", server.URL, map[string]any{"apikey": "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := adapter.FetchSiteUserData(t.Context())
	if err != nil {
		t.Fatalf("FetchSiteUserData() error = %v", err)
	}
	if got.ErrMsg == nil || *got.ErrMsg != "获取用户信息失败，未收到开放 API 响应" {
		t.Fatalf("ErrMsg = %#v, want YemaPT 可见请求失败", got.ErrMsg)
	}
}

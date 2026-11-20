package indexer

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTNodeFetchSiteUserDataMapsProfileAndSeeding(t *testing.T) {
	var csrfRequests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Cookie") != "session=fixture" || r.Header.Get("User-Agent") != "fixture-agent" {
			t.Fatalf("TNode 请求凭据错误: cookie=%q ua=%q", r.Header.Get("Cookie"), r.Header.Get("User-Agent"))
		}
		switch r.URL.Path {
		case "/":
			csrfRequests++
			_, _ = w.Write([]byte(`<meta name="x-csrf-token" content="fixture-token">`))
		case "/api/user/getMainInfo":
			assertTNodeUserDataCSRF(t, r)
			_ = json.NewEncoder(w).Encode(map[string]any{"status": 200, "data": map[string]any{
				"id": 77, "username": "fixture-user", "class": map[string]any{"name": "Power User"},
				"regTime": "2026-08-10 12:00:00", "upload": 3001, "download": 2000, "bonus": "12.5",
				"unreadAdmin": 1, "unreadInbox": 2, "unreadSystem": 3,
			}})
		case "/api/user/listTorrentActivity":
			assertTNodeUserDataCSRF(t, r)
			if got := r.URL.Query(); got.Get("id") != "" || got.Get("type") != "seeding" || got.Get("page") != "1" || got.Get("size") != "20000" {
				t.Fatalf("TNode 做种查询参数错误: %v", got)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"status": 200, "data": map[string]any{"torrents": []any{
				map[string]any{"size": 1024, "seeding": 5},
				"invalid",
				map[string]any{"size": "2048", "seeding": "7"},
			}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	adapter, err := NewTNodeAdapter(newIndexerHTTPClient(t, 2), 9, "TNode", server.URL, map[string]any{
		"cookie": "session=fixture", "ua": "fixture-agent",
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := adapter.FetchSiteUserData(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if result.UserID != int64(77) || result.Username == nil || *result.Username != "fixture-user" ||
		result.UserLevel == nil || *result.UserLevel != "Power User" || result.JoinAt == nil || *result.JoinAt != "2026-08-10 12:00:00" {
		t.Fatalf("TNode 用户字段映射错误: %#v", result)
	}
	if result.Upload != 3001 || result.Download != 2000 || result.Ratio != 1.501 || result.Bonus != 12.5 || result.MessageUnread != 6 {
		t.Fatalf("TNode 统计字段映射错误: %#v", result)
	}
	if result.Seeding != 2 || result.SeedingSize != 3072 || len(result.SeedingInfo) != 2 ||
		result.SeedingInfo[0][0] != 5 || result.SeedingInfo[1][1] != 2048 {
		t.Fatalf("TNode 做种字段映射错误: %#v", result)
	}
	if csrfRequests != 1 {
		t.Fatalf("TNode CSRF 首页请求次数 = %d, want 1", csrfRequests)
	}
}

func TestTNodeFetchSiteUserDataKeepsZeroRatioAndIgnoresPayloadErrors(t *testing.T) {
	tests := []struct {
		name        string
		profileBody string
		wantUser    bool
	}{
		{name: "zero download", profileBody: `{"status":200,"data":{"username":"zero","upload":100,"download":0}}`, wantUser: true},
		{name: "invalid profile json", profileBody: `{`, wantUser: false},
		{name: "failed profile status", profileBody: `{"status":403}`, wantUser: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/":
					_, _ = w.Write([]byte(`<meta name="x-csrf-token" content="fixture-token">`))
				case "/api/user/getMainInfo":
					_, _ = w.Write([]byte(tt.profileBody))
				case "/api/user/listTorrentActivity":
					_, _ = w.Write([]byte(`{"status":500}`))
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			adapter, err := NewTNodeAdapter(newIndexerHTTPClient(t, 1), 1, "TNode", server.URL, map[string]any{"cookie": "fixture"})
			if err != nil {
				t.Fatal(err)
			}
			result, err := adapter.FetchSiteUserData(t.Context())
			if err != nil {
				t.Fatalf("FetchSiteUserData() error = %v", err)
			}
			if result.Ratio != 0 || (result.Username != nil) != tt.wantUser || result.Seeding != 0 {
				t.Fatalf("TNode 异常载荷结果错误: %#v", result)
			}
		})
	}
}

func assertTNodeUserDataCSRF(t *testing.T, r *http.Request) {
	t.Helper()
	if r.Method != http.MethodGet || r.Header.Get("X-CSRF-TOKEN") != "fixture-token" {
		t.Fatalf("TNode userdata 请求错误: method=%s csrf=%q", r.Method, r.Header.Get("X-CSRF-TOKEN"))
	}
}

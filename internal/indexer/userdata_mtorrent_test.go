package indexer

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestMTorrentFetchSiteUserDataMapsProfileAndPaginatesSeeding(t *testing.T) {
	var pages []int
	var trackerRequests int
	var markReadRequests int
	requests := make([]string, 0, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "fixture-key" || r.Header.Get("User-Agent") != "fixture-agent" {
			t.Fatalf("mTorrent 请求凭据错误: api-key=%q ua=%q", r.Header.Get("x-api-key"), r.Header.Get("User-Agent"))
		}
		requests = append(requests, r.URL.Path)
		switch r.URL.Path {
		case "/api/member/profile":
			assertMTorrentUserDataForm(t, r, "")
			_ = json.NewEncoder(w).Encode(map[string]any{"code": "0", "data": map[string]any{
				"id": 42, "username": "fixture-user", "role": "13",
				"memberStatus": map[string]any{"createdDate": "2026-08-10 12:00:00"},
				"memberCount":  map[string]any{"uploaded": "3000", "downloaded": "2000", "shareRate": "1.5", "bonus": "8.25"},
			}})
		case "/api/msg/search":
			if err := r.ParseForm(); err != nil {
				t.Fatal(err)
			}
			if r.PostForm.Get("keyword") != "" || r.PostForm.Get("box") != "-2" ||
				r.PostForm.Get("type") != "pageNumber" || r.PostForm.Get("pageSize") != "100" {
				t.Fatalf("mTorrent 消息参数错误: %#v", r.PostForm)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"code": "0", "data": map[string]any{"data": []any{
				map[string]any{"id": 9, "unread": true, "title": "通知", "createdDate": "2026-08-16", "context": "正文"},
				map[string]any{"id": 10, "unread": false, "title": "已读", "createdDate": "2026-08-15", "context": "忽略"},
			}}})
		case "/api/msg/markRead":
			markReadRequests++
			if err := r.ParseForm(); err != nil {
				t.Fatal(err)
			}
			if r.PostForm.Get("msgId") != "9" {
				t.Fatalf("mTorrent markRead msgId = %q, want 9", r.PostForm.Get("msgId"))
			}
			http.Error(w, "ignored mark failure", http.StatusBadGateway)
		case "/api/member/getUserTorrentList":
			if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" {
				t.Fatalf("mTorrent 做种请求格式错误: method=%s content-type=%q", r.Method, r.Header.Get("Content-Type"))
			}
			var request struct {
				PageNumber int    `json:"pageNumber"`
				PageSize   int    `json:"pageSize"`
				Type       string `json:"type"`
				UserID     int64  `json:"userid"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			if request.PageSize != 200 || request.Type != "SEEDING" || request.UserID != 42 {
				t.Fatalf("mTorrent 做种参数错误: %#v", request)
			}
			pages = append(pages, request.PageNumber)
			items := []any{map[string]any{"torrent": map[string]any{"size": "1024", "source": "5"}}, "invalid", map[string]any{}}
			if request.PageNumber == 2 {
				items = []any{map[string]any{"torrent": map[string]any{"size": 2048, "source": 7}}}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"code": "0", "data": map[string]any{"data": items}})
		case "/api/tracker/myPeerStatus":
			trackerRequests++
			assertMTorrentUserDataForm(t, r, "42")
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"seeder": 2}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	adapter, err := NewMTorrentAdapter(newIndexerHTTPClient(t, 2), 12, "M-Team", "https://tracker.example.test", map[string]any{
		"api_url": server.URL, "apikey": "fixture-key", "ua": "fixture-agent",
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := adapter.FetchSiteUserData(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if result.Domain != "example.test" || result.UserID != int64(42) || result.Username == nil || *result.Username != "fixture-user" ||
		result.UserLevel == nil || *result.UserLevel != "Moderator" || result.JoinAt == nil || *result.JoinAt != "2026-08-10 12:00:00" {
		t.Fatalf("mTorrent 用户字段映射错误: %#v", result)
	}
	if result.Upload != 3000 || result.Download != 2000 || result.Ratio != 1.5 || result.Bonus != 8.25 {
		t.Fatalf("mTorrent 统计字段映射错误: %#v", result)
	}
	if result.Seeding != 2 || result.SeedingSize != 3072 || !reflect.DeepEqual(result.SeedingInfo, [][]int64{{5, 1024}, {7, 2048}}) {
		t.Fatalf("mTorrent 做种字段映射错误: %#v", result)
	}
	if !reflect.DeepEqual(pages, []int{1, 2}) || trackerRequests != 2 {
		t.Fatalf("mTorrent 分页错误: pages=%v trackerRequests=%d", pages, trackerRequests)
	}
	if result.MessageUnread != 0 || !reflect.DeepEqual(result.MessageUnreadContents, [][]any{{"通知", "2026-08-16", "正文"}}) ||
		markReadRequests != 1 {
		t.Fatalf("mTorrent V2 消息怪异语义错误: result=%#v markRead=%d", result, markReadRequests)
	}
	wantPrefix := []string{"/api/member/profile", "/api/msg/search", "/api/msg/markRead", "/api/member/getUserTorrentList"}
	if len(requests) < len(wantPrefix) || !reflect.DeepEqual(requests[:len(wantPrefix)], wantPrefix) {
		t.Fatalf("mTorrent 请求顺序 = %#v, want prefix %#v", requests, wantPrefix)
	}
}

func TestMTorrentFetchSiteUserDataTreatsBusinessFailuresAsSuccess(t *testing.T) {
	tests := []struct {
		name           string
		profileCode    string
		seedingCode    string
		wantUsername   bool
		wantSeedCalls  int
		wantTrackCalls int
	}{
		{name: "profile failure", profileCode: "1", seedingCode: "0"},
		{name: "seeding failure", profileCode: "0", seedingCode: "1", wantUsername: true, wantSeedCalls: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var seedCalls, trackerCalls int
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/member/profile":
					_ = json.NewEncoder(w).Encode(map[string]any{"code": tt.profileCode, "data": map[string]any{"id": 42, "username": "fixture"}})
				case "/api/member/getUserTorrentList":
					seedCalls++
					_ = json.NewEncoder(w).Encode(map[string]any{"code": tt.seedingCode})
				case "/api/tracker/myPeerStatus":
					trackerCalls++
					_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"seeder": 0}})
				case "/api/msg/search":
					_ = json.NewEncoder(w).Encode(map[string]any{"code": "0", "data": map[string]any{"data": []any{}}})
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			adapter, err := NewMTorrentAdapter(newIndexerHTTPClient(t, 1), 1, "M-Team", "https://tracker.example.test", map[string]any{
				"api_url": server.URL, "apikey": "fixture-key",
			})
			if err != nil {
				t.Fatal(err)
			}
			result, err := adapter.FetchSiteUserData(t.Context())
			if err != nil {
				t.Fatalf("FetchSiteUserData() error = %v", err)
			}
			if (result.Username != nil) != tt.wantUsername || seedCalls != tt.wantSeedCalls || trackerCalls != tt.wantTrackCalls {
				t.Fatalf("mTorrent 业务失败结果错误: result=%#v seedCalls=%d trackerCalls=%d", result, seedCalls, trackerCalls)
			}
		})
	}
}

func TestMTorrentFetchSiteUserDataContinuesPastInvalidPage(t *testing.T) {
	var pages []int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/member/profile":
			_ = json.NewEncoder(w).Encode(map[string]any{"code": "0", "data": map[string]any{"id": 42}})
		case "/api/member/getUserTorrentList":
			var request struct {
				PageNumber int `json:"pageNumber"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			pages = append(pages, request.PageNumber)
			items := []any{"invalid", map[string]any{}}
			if request.PageNumber == 2 {
				items = []any{map[string]any{"torrent": map[string]any{"size": 1024, "source": 3}}}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"code": "0", "data": map[string]any{"data": items}})
		case "/api/tracker/myPeerStatus":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"seeder": 1}})
		case "/api/msg/search":
			_ = json.NewEncoder(w).Encode(map[string]any{"code": "0", "data": map[string]any{"data": []any{}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	adapter, err := NewMTorrentAdapter(newIndexerHTTPClient(t, 1), 1, "M-Team", "https://tracker.example.test", map[string]any{
		"api_url": server.URL, "apikey": "fixture-key",
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := adapter.FetchSiteUserData(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if result.Seeding != 1 || !reflect.DeepEqual(pages, []int{1, 2}) {
		t.Fatalf("mTorrent 无效页后未继续分页: result=%#v pages=%v", result, pages)
	}
}

func TestMTorrentFetchSiteUserDataSkipsMessagesWhenV2SettingDisabled(t *testing.T) {
	requests := make([]string, 0, 3)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.URL.Path)
		switch r.URL.Path {
		case "/api/member/profile":
			_ = json.NewEncoder(w).Encode(map[string]any{"code": "0", "data": map[string]any{"id": 42}})
		case "/api/member/getUserTorrentList":
			_ = json.NewEncoder(w).Encode(map[string]any{"code": "0", "data": map[string]any{"data": []any{}}})
		case "/api/tracker/myPeerStatus":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"seeder": 0}})
		case "/api/msg/search", "/api/msg/markRead":
			t.Fatalf("SITE_MESSAGE=false 时发生远端消息副作用: %s", r.URL.Path)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	adapter, err := NewMTorrentAdapter(newIndexerHTTPClient(t, 1), 1, "M-Team", "https://tracker.example.test", map[string]any{
		"api_url": server.URL, "apikey": "fixture-key",
	})
	if err != nil {
		t.Fatal(err)
	}
	coordinator, err := NewCoordinator(1)
	if err != nil {
		t.Fatal(err)
	}
	defer coordinator.Close()
	if _, err := coordinator.FetchSiteUserData(t.Context(), adapter, SiteUserDataOptions{FetchMessages: false}); err != nil {
		t.Fatal(err)
	}
	want := []string{"/api/member/profile", "/api/member/getUserTorrentList", "/api/tracker/myPeerStatus"}
	if !reflect.DeepEqual(requests, want) {
		t.Fatalf("requests = %#v, want %#v", requests, want)
	}
}

func assertMTorrentUserDataForm(t *testing.T, r *http.Request, wantUID string) {
	t.Helper()
	if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/x-www-form-urlencoded; charset=UTF-8" {
		t.Fatalf("mTorrent form 请求格式错误: method=%s content-type=%q", r.Method, r.Header.Get("Content-Type"))
	}
	if err := r.ParseForm(); err != nil {
		t.Fatal(err)
	}
	if got := r.PostForm.Get("uid"); got != wantUID {
		t.Fatalf("mTorrent uid = %q, want %q", got, wantUID)
	}
}

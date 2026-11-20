package indexer

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTorrentLeechFetchSiteUserDataMatchesV2(t *testing.T) {
	requests := make(map[string]int)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests[r.URL.Path]++
		if r.Header.Get("Cookie") != "session=fixture" {
			t.Errorf("Cookie = %q", r.Header.Get("Cookie"))
		}
		switch r.URL.Path {
		case "/":
			_, _ = w.Write([]byte(`<a href="/logout">logout</a><span class="centerTopBar"><button onclick="go('/profile/42/view')">me</button></span>`))
		case "/profile/42/view":
			_, _ = w.Write([]byte(`<div class="profile-username">alice</div><div class="profile-uploaded"><span>1.5 GB</span></div><div class="profile-downloaded"><span>512 MB</span></div><div class="profile-ratio"><span>3.0</span></div><table class="profileViewTable"><tr><td>Class</td><td>Power User</td></tr><tr><td>Registration date</td><td>2020-01-02T03:04:05Z</td></tr></table><span class="total-TL-points">12.5</span>`))
		case "/profile/42/seeding":
			_, _ = w.Write([]byte(`<table><tbody><tr><td>x</td><td>2 GB</td><td>x</td><td>x</td><td>x</td><td>x</td><td>7</td></tr></tbody></table>`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	adapter, err := NewTorrentLeechAdapter(newIndexerHTTPClient(t, 2), 7, "TorrentLeech", server.URL, map[string]any{"cookie": "session=fixture"})
	if err != nil {
		t.Fatal(err)
	}
	data, err := adapter.FetchSiteUserData(t.Context())
	if err != nil {
		t.Fatalf("FetchSiteUserData() error = %v", err)
	}
	if data.UserID != "42" || data.Username == nil || *data.Username != "alice" || data.UserLevel == nil || *data.UserLevel != "Power User" {
		t.Fatalf("identity = %#v", data)
	}
	if data.Upload != 1610612736 || data.Download != 536870912 || data.Ratio != 3 || data.Bonus != 12.5 {
		t.Fatalf("traffic = %#v", data)
	}
	if data.JoinAt == nil || *data.JoinAt != "2020-01-02 03:04:05" || data.Seeding != 1 || data.SeedingSize != 2147483648 || len(data.SeedingInfo) != 1 || data.SeedingInfo[0][0] != 7 {
		t.Fatalf("detail = %#v", data)
	}
	for _, path := range []string{"/", "/profile/42/view", "/profile/42/seeding"} {
		if requests[path] != 1 {
			t.Fatalf("requests[%q] = %d", path, requests[path])
		}
	}
}

func TestTorrentLeechFetchSiteUserDataReturnsV2ParserErrors(t *testing.T) {
	tests := []struct {
		name string
		html string
		want string
	}{
		{name: "logged out", html: `<input type="password">`, want: "未检测到已登陆，请检查cookies是否过期"},
		{name: "missing id", html: `<a href="/logout">logout</a>`, want: "未获取到用户ID"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(tt.html)) }))
			defer server.Close()
			adapter, err := NewTorrentLeechAdapter(newIndexerHTTPClient(t, 1), 7, "TorrentLeech", server.URL, map[string]any{"cookie": "session=fixture"})
			if err != nil {
				t.Fatal(err)
			}
			data, err := adapter.FetchSiteUserData(t.Context())
			if err != nil || data.ErrMsg == nil || *data.ErrMsg != tt.want {
				t.Fatalf("FetchSiteUserData() = %#v, %v", data, err)
			}
		})
	}
}

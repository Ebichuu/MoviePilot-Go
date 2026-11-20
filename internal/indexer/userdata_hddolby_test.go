package indexer

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHDDolbyFetchSiteUserDataMatchesV2(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/user/data":
			if r.Header.Get("x-api-key") != "api-key" || r.Header.Get("Cookie") != "" {
				t.Errorf("user headers = %#v", r.Header)
			}
			_, _ = w.Write([]byte(`{"status":0,"data":[{"id":"42","username":"alice","class":"10","added":"2020-01-02 03:04:05","uploaded":"4096","downloaded":"1024","seedbonus":"12.5","unread_messages":"1"}]}`))
		case "/api/v1/user/peers":
			_, _ = w.Write([]byte(`{"status":0,"data":[{"size":8192,"seeders":0}]}`))
		case "/messages.php":
			if r.Header.Get("Cookie") != "session=fixture" || r.Header.Get("x-api-key") != "" {
				t.Errorf("message headers = %#v", r.Header)
			}
			if r.URL.Query().Get("action") == "viewmailbox" && r.URL.Query().Get("box") == "1" {
				_, _ = w.Write([]byte(`<table><tr><td><a href="/viewmessage.php?id=9">new</a></td></tr></table>`))
				return
			}
			_, _ = w.Write([]byte(`<table></table>`))
		case "/viewmessage.php":
			_, _ = w.Write([]byte(`<h1>Notice</h1><table><tr><td class="colhead">head</td></tr><tr><td>x</td><td>2026-08-16</td></tr><tr><td>Body</td></tr></table>`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	adapter, err := NewHDDolbyAdapter(newIndexerHTTPClient(t, 2), 7, "HDDolby", server.URL, map[string]any{
		"api_url": server.URL, "apikey": "api-key", "cookie": "session=fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	data, err := adapter.FetchSiteUserData(t.Context())
	if err != nil {
		t.Fatalf("FetchSiteUserData() error = %v", err)
	}
	if data.UserID != "42" || data.Username == nil || *data.Username != "alice" || data.UserLevel == nil || *data.UserLevel != "VIP" {
		t.Fatalf("identity = %#v", data)
	}
	if data.Upload != 4096 || data.Download != 1024 || data.Ratio != 4 || data.Bonus != 12.5 {
		t.Fatalf("traffic = %#v", data)
	}
	if data.Seeding != 1 || data.SeedingSize != 8192 || len(data.SeedingInfo) != 1 || data.SeedingInfo[0][0] != 1 {
		t.Fatalf("seeding = %#v", data)
	}
	if data.MessageUnread != 1 || len(data.MessageUnreadContents) != 1 || data.MessageUnreadContents[0][0] != "Notice" {
		t.Fatalf("messages = %#v", data.MessageUnreadContents)
	}
}

func TestHDDolbyFetchSiteUserDataInvalidJSONMatchesV2ModuleFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`not-json`)) }))
	defer server.Close()
	adapter, err := NewHDDolbyAdapter(newIndexerHTTPClient(t, 1), 7, "HDDolby", server.URL, map[string]any{
		"api_url": server.URL, "apikey": "api-key", "cookie": "session=fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.FetchSiteUserData(t.Context()); err == nil {
		t.Fatal("FetchSiteUserData() error = nil")
	}
}

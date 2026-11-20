package emby

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

func TestListUsersAndResolveUserIDV2Order(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/emby/Users" || len(r.URL.Query()) != 0 {
			t.Errorf("request=%s?%s", r.URL.Path, r.URL.RawQuery)
		}
		_, _ = io.WriteString(w, `[{"Id":"admin-first","Name":"Admin One","Policy":{"IsAdministrator":true}},`+
			`{"Id":"configured","Name":"Configured","Policy":{}},`+
			`{"Id":"requested","Name":"Requested","Policy":{}},`+
			`{"Id":"admin-second","Name":"Admin Two","Policy":{"IsAdministrator":true}}]`)
	}))
	t.Cleanup(server.Close)
	users, err := newTestClient(t, server.URL+"/emby").ListUsers(t.Context())
	if err != nil || len(users) != 4 {
		t.Fatalf("ListUsers()=(%#v,%v)", users, err)
	}
	for _, test := range []struct {
		name, requested, configured, want string
	}{
		{name: "requested first", requested: "Requested", configured: "Configured", want: "requested"},
		{name: "configured second", requested: "Missing", configured: "Configured", want: "configured"},
		{name: "exact case", requested: "requested", configured: "", want: "admin-first"},
		{name: "administrator response order", requested: "Missing", configured: "Absent", want: "admin-first"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := ResolveUserID(users, test.requested, test.configured); got != test.want {
				t.Fatalf("ResolveUserID()=%q want %q", got, test.want)
			}
		})
	}
}

func TestGetItemInfoContracts(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name      string
		status    int
		body      string
		wantFound bool
		wantID    string
		wantErr   ErrorCode
		wantCalls int32
	}{
		{name: "formatted object", body: `{"Id":"movie-1","Type":"Movie","Name":"Movie","ProviderIds":{"Tmdb":"1"}}`, wantFound: true, wantID: "movie-1", wantCalls: 1},
		{name: "V2 optional fields object", body: `{}`, wantFound: true, wantCalls: 1},
		{name: "not found", status: http.StatusNotFound, wantCalls: 1},
		{name: "null is not formatted object", body: `null`, wantErr: ErrorCodeInvalidResponse, wantCalls: 1},
		{name: "array is not formatted object", body: `[]`, wantErr: ErrorCodeInvalidResponse, wantCalls: 1},
		{name: "wrong item field shape", body: `{"Id":[]}`, wantErr: ErrorCodeInvalidResponse, wantCalls: 1},
		{name: "null provider ids fail V2 formatter", body: `{"ProviderIds":null}`, wantErr: ErrorCodeInvalidResponse, wantCalls: 1},
		{name: "invalid tmdb id fails V2 formatter", body: `{"ProviderIds":{"Tmdb":"bad"}}`, wantErr: ErrorCodeInvalidResponse, wantCalls: 1},
		{name: "truthy array user data fails V2 formatter", body: `{"UserData":[1]}`, wantErr: ErrorCodeInvalidResponse, wantCalls: 1},
		{name: "invalid playback position fails V2 formatter", body: `{"UserData":{"PlaybackPositionTicks":"bad"}}`, wantErr: ErrorCodeInvalidResponse, wantCalls: 1},
		{name: "array played state fails V2 formatter", body: `{"UserData":{"Played":[]}}`, wantErr: ErrorCodeInvalidResponse, wantCalls: 1},
		{name: "invalid play count fails V2 formatter", body: `{"UserData":{"PlayCount":"bad"}}`, wantErr: ErrorCodeInvalidResponse, wantCalls: 1},
		{name: "object played percentage fails V2 formatter", body: `{"UserData":{"PlayedPercentage":{}}}`, wantErr: ErrorCodeInvalidResponse, wantCalls: 1},
		{name: "invalid last played date fails V2 formatter", body: `{"UserData":{"LastPlayedDate":"bad"}}`, wantErr: ErrorCodeInvalidResponse, wantCalls: 1},
		{name: "valid last played date matches V2 formatter", body: `{"Id":"movie-2","UserData":{"LastPlayedDate":"2026-09-14T10:11:12.123Z"}}`, wantFound: true, wantID: "movie-2", wantCalls: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.URL.Path != "/emby/Users/user/unsafe/Items/item/unsafe" || r.URL.EscapedPath() != "/emby/Users/user%2Funsafe/Items/item%2Funsafe" {
					t.Errorf("path=%q escaped=%q", r.URL.Path, r.URL.EscapedPath())
				}
				if test.status != 0 {
					w.WriteHeader(test.status)
				}
				_, _ = io.WriteString(w, test.body)
			}))
			t.Cleanup(server.Close)
			item, found, err := newTestClient(t, server.URL+"/emby").GetItemInfo(t.Context(), "user/unsafe", "item/unsafe")
			if found != test.wantFound || item.ID != test.wantID || calls.Load() != test.wantCalls {
				t.Fatalf("GetItemInfo()=(%#v,%v,%v) calls=%d", item, found, err, calls.Load())
			}
			assertEmbyErrorCode(t, err, test.wantErr)
		})
	}
}

func TestGetItemInfoRequiresV2HTTP200(t *testing.T) {
	t.Parallel()
	for _, status := range []int{http.StatusCreated, http.StatusAccepted, http.StatusNoContent} {
		status := status
		t.Run(http.StatusText(status), func(t *testing.T) {
			t.Parallel()
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.WriteHeader(status)
				_, _ = io.WriteString(w, `{}`)
			}))
			t.Cleanup(server.Close)
			_, found, err := newTestClient(t, server.URL).GetItemInfo(t.Context(), "user", "item")
			if err != nil || found || calls.Load() != 1 {
				t.Fatalf("GetItemInfo() found=%v error=%v calls=%d", found, err, calls.Load())
			}
		})
	}
}

func TestListUserItemsUsesBoundedParentPage(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/emby/Users/user/unsafe/Items" || r.URL.EscapedPath() != "/emby/Users/user%2Funsafe/Items" {
			t.Errorf("path=%q escaped=%q", r.URL.Path, r.URL.EscapedPath())
		}
		want := map[string]string{
			"ParentId": "parent/unsafe", "Fields": v2LibraryItemFields,
			"StartIndex": "40", "Limit": "200",
		}
		if len(r.URL.Query()) != len(want) {
			t.Errorf("query=%v", r.URL.Query())
		}
		for key, value := range want {
			if got := r.URL.Query().Get(key); got != value {
				t.Errorf("query %s=%q want %q", key, got, value)
			}
		}
		_, _ = io.WriteString(w, `{"Items":[{"Id":"movie"}],"TotalRecordCount":41,"StartIndex":40}`)
	}))
	t.Cleanup(server.Close)
	result, err := newTestClient(t, server.URL+"/emby").ListUserItems(t.Context(), UserItemsOptions{
		UserID: "user/unsafe", ParentID: "parent/unsafe", StartIndex: 40,
	})
	if err != nil || result.StartIndex != 40 || result.TotalRecordCount != 41 || !reflect.DeepEqual(result.Items, []Item{{ID: "movie"}}) {
		t.Fatalf("ListUserItems()=(%#v,%v)", result, err)
	}
}

func TestR07MethodsRejectInvalidArgumentsWithoutRequest(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	t.Cleanup(server.Close)
	client := newTestClient(t, server.URL)
	invalidIDs := []string{"", " ", ".", "..", "bad\nvalue", strings.Repeat("x", maxEmbyPathIDBytes+1)}
	for _, value := range invalidIDs {
		_, _, err := client.GetItemInfo(t.Context(), value, "item")
		assertEmbyErrorCode(t, err, ErrorCodeInvalidArgument)
		_, _, err = client.GetItemInfo(t.Context(), "user", value)
		assertEmbyErrorCode(t, err, ErrorCodeInvalidArgument)
	}
	for _, options := range []UserItemsOptions{
		{}, {UserID: "user", ParentID: "parent", StartIndex: -1},
		{UserID: "user", ParentID: "parent", StartIndex: maxUserItemsStartIndex + 1},
		{UserID: "user", ParentID: "parent", Limit: -1},
		{UserID: "user", ParentID: "parent", Limit: maxUserItemsPageLimit + 1},
	} {
		_, err := client.ListUserItems(t.Context(), options)
		assertEmbyErrorCode(t, err, ErrorCodeInvalidArgument)
	}
	if calls.Load() != 0 {
		t.Fatalf("invalid arguments caused %d requests", calls.Load())
	}
}

func TestGetItemInfoPropagatesCancellation(t *testing.T) {
	t.Parallel()
	client := newTestClient(t, "http://127.0.0.1:1")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, _, err := client.GetItemInfo(ctx, "user", "item")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v", err)
	}
}

func assertEmbyErrorCode(t *testing.T, err error, want ErrorCode) {
	t.Helper()
	if want == "" {
		if err != nil {
			t.Fatalf("error=%v want nil", err)
		}
		return
	}
	var embyErr *Error
	if !errors.As(err, &embyErr) || embyErr.Code != want {
		t.Fatalf("error=%v want code %s", err, want)
	}
}

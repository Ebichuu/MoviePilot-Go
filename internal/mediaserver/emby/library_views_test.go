package emby

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"
)

func TestListUserViewsContracts(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name      string
		userID    string
		status    int
		body      string
		want      []UserView
		wantCalls int32
		wantErr   ErrorCode
	}{
		{
			name: "user scoped views", userID: "user-1",
			body: `{"Items":[{"Id":"lib-1","Name":"Movies","CollectionType":"movies","ServerId":"srv"},` +
				`{"Id":"lib-2","Name":"Shows","CollectionType":"tvshows"}]}`,
			want: []UserView{
				{ID: "lib-1", Name: "Movies", CollectionType: "movies", ServerID: "srv"},
				{ID: "lib-2", Name: "Shows", CollectionType: "tvshows"},
			},
			wantCalls: 1,
		},
		{name: "missing items is an empty library list", userID: "user-1", body: `{}`,
			want: []UserView{}, wantCalls: 1},
		{name: "invalid user id never requests", userID: " .. ", wantErr: ErrorCodeInvalidArgument},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.URL.Path != "/emby/Users/user-1/Views" || len(r.URL.Query()) != 0 {
					t.Errorf("request = %s?%s", r.URL.Path, r.URL.RawQuery)
				}
				if test.status != 0 {
					w.WriteHeader(test.status)
					return
				}
				_, _ = io.WriteString(w, test.body)
			}))
			t.Cleanup(server.Close)
			views, err := newTestClient(t, server.URL+"/emby").ListUserViews(t.Context(), test.userID)
			if test.wantErr != "" {
				var upstream *Error
				if !errors.As(err, &upstream) || upstream.Code != test.wantErr {
					t.Fatalf("ListUserViews() error = %v, want %v", err, test.wantErr)
				}
			} else if err != nil || !reflect.DeepEqual(views, test.want) {
				t.Fatalf("ListUserViews() = (%#v, %v), want %#v", views, err, test.want)
			}
			if calls.Load() != test.wantCalls {
				t.Fatalf("requests = %d, want %d", calls.Load(), test.wantCalls)
			}
		})
	}
}

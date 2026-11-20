package emby

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestClientRequestContracts(t *testing.T) {
	const mediaPath = `/media/Movies/A&B`
	const providerID = `tmdb.123/secret`
	tests := []struct {
		name        string
		method      string
		path        string
		escapedPath string
		query       map[string]string
		response    string
		status      int
		call        func(*Client) error
	}{
		{
			name: "system info", method: http.MethodGet, path: "/emby/System/Info",
			response: `{"Id":"server"}`,
			call:     func(client *Client) error { _, err := client.SystemInfo(t.Context()); return err },
		},
		{
			name: "list libraries", method: http.MethodGet, path: "/emby/Library/VirtualFolders",
			response: `[{"Name":"Movies"}]`,
			call:     func(client *Client) error { _, err := client.ListLibraries(t.Context()); return err },
		},
		{
			name: "find items", method: http.MethodGet, path: "/emby/Items",
			query: map[string]string{
				"Path": mediaPath, "AnyProviderIdEquals": providerID,
				"IncludeItemTypes": "Movie,Episode", "Recursive": "true",
				"Fields": "Path,ProviderIds", "Limit": "2",
			},
			response: `{"Items":[],"TotalRecordCount":0}`,
			call: func(client *Client) error {
				_, err := client.FindItems(t.Context(), FindItemsOptions{
					Path: mediaPath, AnyProviderIDEquals: providerID, Recursive: true,
					Fields: []string{"Path", "ProviderIds"}, Limit: 2,
				})
				return err
			},
		},
		{
			name: "find items page", method: http.MethodGet, path: "/emby/Items",
			query: map[string]string{
				"IncludeItemTypes": "Movie,Episode", "Recursive": "false",
				"StartIndex": "40", "Limit": "200",
			},
			response: `{"Items":[],"TotalRecordCount":40,"StartIndex":40}`,
			call: func(client *Client) error {
				_, err := client.FindItems(t.Context(), FindItemsOptions{StartIndex: 40, Limit: 200})
				return err
			},
		},
		{
			name: "search series", method: http.MethodGet, path: "/emby/Items",
			query: map[string]string{
				"IncludeItemTypes": "Series", "Fields": "ProductionYear,ProviderIds",
				"StartIndex": "0", "Recursive": "true", "SearchTerm": "Example Show",
				"Limit": "10", "IncludeSearchTypes": "false",
			},
			response: `{"Items":[],"TotalRecordCount":0,"StartIndex":0}`,
			call: func(client *Client) error {
				_, err := client.SearchSeries(t.Context(), SearchSeriesOptions{SearchTerm: " Example Show "})
				return err
			},
		},
		{
			name: "search movies", method: http.MethodGet, path: "/emby/Items",
			query: map[string]string{
				"IncludeItemTypes": "Movie",
				"Fields": "ProviderIds,OriginalTitle,ProductionYear,Path," +
					"UserDataPlayCount,UserDataLastPlayedDate,ParentId",
				"StartIndex": "0", "Recursive": "true", "SearchTerm": "Example Movie",
				"Limit": "10", "IncludeSearchTypes": "false",
			},
			response: `{"Items":[],"TotalRecordCount":0,"StartIndex":0}`,
			call: func(client *Client) error {
				_, err := client.SearchMovies(t.Context(), SearchMoviesOptions{SearchTerm: " Example Movie "})
				return err
			},
		},
		{
			name: "list episodes", method: http.MethodGet,
			path: "/emby/Shows/series/unsafe/Episodes", escapedPath: "/emby/Shows/series%2Funsafe/Episodes",
			query:    map[string]string{"IsMissing": "false", "StartIndex": "40", "Limit": "200"},
			response: `{"Items":[],"TotalRecordCount":40,"StartIndex":40}`,
			call: func(client *Client) error {
				_, err := client.ListEpisodes(t.Context(), ListEpisodesOptions{
					SeriesID: "series/unsafe", StartIndex: 40, Limit: 200,
				})
				return err
			},
		},
		{
			name: "refresh item", method: http.MethodPost,
			path: "/emby/Items/item/unsafe/Refresh", escapedPath: "/emby/Items/item%2Funsafe/Refresh",
			status: http.StatusNoContent,
			call:   func(client *Client) error { return client.RefreshItem(t.Context(), "item/unsafe") },
		},
		{
			name: "refresh all", method: http.MethodPost, path: "/emby/Library/Refresh",
			status: http.StatusNoContent,
			call:   func(client *Client) error { return client.RefreshAll(t.Context()) },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != tt.method || r.URL.Path != tt.path {
					t.Errorf("request = %s %s, want %s %s", r.Method, r.URL.Path, tt.method, tt.path)
				}
				if tt.escapedPath != "" && r.URL.EscapedPath() != tt.escapedPath {
					t.Errorf("escaped path = %q, want %q", r.URL.EscapedPath(), tt.escapedPath)
				}
				for key, want := range tt.query {
					if got := r.URL.Query().Get(key); got != want {
						t.Errorf("query %s = %q, want %q", key, got, want)
					}
				}
				if got := len(r.URL.Query()); got != len(tt.query) {
					t.Errorf("query = %v, want exactly %v", r.URL.Query(), tt.query)
				}
				if r.URL.Query().Has("api_key") {
					t.Error("request contains forbidden api_key")
				}
				if r.Header.Get("X-Emby-Token") != "fixture-token" {
					t.Error("missing X-Emby-Token")
				}
				if r.Header.Get("Authorization") != "MediaBrowser Client=MoviePilot-Go" {
					t.Error("missing Authorization")
				}
				if r.ContentLength > 0 {
					t.Errorf("unexpected request body length %d", r.ContentLength)
				}
				if tt.status != 0 {
					w.WriteHeader(tt.status)
				}
				if tt.response != "" {
					_, _ = w.Write([]byte(tt.response))
				}
			}))
			t.Cleanup(server.Close)

			if err := tt.call(newTestClient(t, server.URL+"/emby")); err != nil {
				t.Fatalf("call error = %v", err)
			}
		})
	}
}

func TestUnknownResponseFieldsAreIgnored(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/System/Info":
			_, _ = w.Write([]byte(`{"Id":"server","ServerName":"Fixture","Future":{"Nested":true}}`))
		case "/Items":
			_, _ = w.Write([]byte(
				`{"Items":[{"Id":"item","Name":"Movie","Type":"Movie",` +
					`"Path":"/media/movie.mkv","ProviderIds":{"Tmdb":"123"},"FutureField":[1,2]}],` +
					`"TotalRecordCount":1,"UnknownEnvelope":true}`,
			))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	client := newTestClient(t, server.URL)

	info, err := client.SystemInfo(t.Context())
	if err != nil || info.ID != "server" || info.ServerName != "Fixture" {
		t.Fatalf("SystemInfo() = (%#v, %v)", info, err)
	}
	items, err := client.FindItems(t.Context(), FindItemsOptions{Limit: 1})
	if err != nil || len(items.Items) != 1 {
		t.Fatalf("FindItems() = (%#v, %v)", items, err)
	}
	want := Item{
		ID: "item", Name: "Movie", Type: "Movie", Path: "/media/movie.mkv",
		ProviderIDs: map[string]string{"Tmdb": "123"},
	}
	if !reflect.DeepEqual(items.Items[0], want) {
		t.Fatalf("item = %#v, want %#v", items.Items[0], want)
	}
}

func TestRefreshAcceptsEmptySuccessResponse(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusNoContent} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(status)
			}))
			t.Cleanup(server.Close)
			client := newTestClient(t, server.URL)
			if err := client.RefreshAll(t.Context()); err != nil {
				t.Fatalf("RefreshAll() error = %v", err)
			}
		})
	}
}

func TestFindItemsRejectsInvalidPageWithoutRequest(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		_, _ = w.Write([]byte(`{"Items":[]}`))
	}))
	t.Cleanup(server.Close)
	client := newTestClient(t, server.URL)

	for _, options := range []FindItemsOptions{
		{Limit: -1},
		{Limit: maxFindItemsLimit + 1},
		{StartIndex: -1},
		{StartIndex: maxFindItemsStartIndex + 1},
	} {
		_, err := client.FindItems(t.Context(), options)
		var embyErr *Error
		if !errors.As(err, &embyErr) || embyErr.Code != ErrorCodeInvalidArgument {
			t.Fatalf("FindItems(%#v) error = %v, want invalid_argument", options, err)
		}
	}
	if requests != 0 {
		t.Fatalf("invalid limits caused %d requests", requests)
	}
}

func TestSubscriptionMediaRequestsRejectInvalidArguments(t *testing.T) {
	t.Parallel()
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		_, _ = w.Write([]byte(`{"Items":[]}`))
	}))
	t.Cleanup(server.Close)
	client := newTestClient(t, server.URL)

	tests := []struct {
		name string
		call func() error
	}{
		{name: "empty series term", call: func() error {
			_, err := client.SearchSeries(t.Context(), SearchSeriesOptions{})
			return err
		}},
		{name: "series limit exceeds bound", call: func() error {
			_, err := client.SearchSeries(t.Context(), SearchSeriesOptions{SearchTerm: "Show", Limit: maxSeriesSearchLimit + 1})
			return err
		}},
		{name: "empty movie term", call: func() error {
			_, err := client.SearchMovies(t.Context(), SearchMoviesOptions{})
			return err
		}},
		{name: "movie limit exceeds bound", call: func() error {
			_, err := client.SearchMovies(t.Context(), SearchMoviesOptions{SearchTerm: "Movie", Limit: maxMovieSearchLimit + 1})
			return err
		}},
		{name: "empty series id", call: func() error {
			_, err := client.ListEpisodes(t.Context(), ListEpisodesOptions{})
			return err
		}},
		{name: "negative episode page", call: func() error {
			_, err := client.ListEpisodes(t.Context(), ListEpisodesOptions{SeriesID: "series", StartIndex: -1})
			return err
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.call()
			var embyErr *Error
			if !errors.As(err, &embyErr) || embyErr.Code != ErrorCodeInvalidArgument {
				t.Fatalf("call error = %v, want invalid_argument", err)
			}
		})
	}
	if requests != 0 {
		t.Fatalf("invalid arguments caused %d requests", requests)
	}
}

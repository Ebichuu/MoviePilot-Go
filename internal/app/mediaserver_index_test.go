package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/moviepilot-go/moviepilot-go/internal/httpapi"
	"github.com/moviepilot-go/moviepilot-go/internal/mediaserver/emby"
)

func TestMediaServerRuntimeFindLocalMediaServerItem(t *testing.T) {
	tests := []struct {
		name         string
		tmdbID       int64
		responses    []localMediaIndexResponse
		wantID       string
		wantFound    bool
		wantErr      bool
		wantRequests []int32
	}{
		{
			name:         "zero TMDB ID does not query",
			responses:    []localMediaIndexResponse{{body: singleLocalMediaIndexItem("item-1", "123")}},
			wantRequests: []int32{0},
		},
		{
			name: "negative TMDB ID does not query", tmdbID: -1,
			responses:    []localMediaIndexResponse{{body: singleLocalMediaIndexItem("item-1", "123")}},
			wantRequests: []int32{0},
		},
		{
			name: "single validated item", tmdbID: 123,
			responses: []localMediaIndexResponse{{body: singleLocalMediaIndexItem("item-1", "123")}},
			wantID:    "item-1", wantFound: true, wantRequests: []int32{1},
		},
		{
			name: "provider key is case insensitive", tmdbID: 123,
			responses: []localMediaIndexResponse{{body: `{"Items":[{"Id":"item-1","ProviderIds":{"TMDB":"123"}}],"TotalRecordCount":1}`}},
			wantID:    "item-1", wantFound: true, wantRequests: []int32{1},
		},
		{
			name: "provider mismatch is ignored", tmdbID: 123,
			responses:    []localMediaIndexResponse{{body: singleLocalMediaIndexItem("item-1", "456")}},
			wantRequests: []int32{1},
		},
		{
			name: "missing provider is ignored", tmdbID: 123,
			responses:    []localMediaIndexResponse{{body: `{"Items":[{"Id":"item-1"}],"TotalRecordCount":1}`}},
			wantRequests: []int32{1},
		},
		{
			name: "empty item ID is ignored", tmdbID: 123,
			responses:    []localMediaIndexResponse{{body: singleLocalMediaIndexItem(" ", "123")}},
			wantRequests: []int32{1},
		},
		{
			name: "zero matches", tmdbID: 123,
			responses:    []localMediaIndexResponse{{body: `{"Items":[],"TotalRecordCount":0}`}},
			wantRequests: []int32{1},
		},
		{
			name: "duplicate copies return the first match", tmdbID: 123,
			responses: []localMediaIndexResponse{{body: `{"Items":[` +
				`{"Id":"item-1","ProviderIds":{"Tmdb":"123"}},` +
				`{"Id":"item-2","ProviderIds":{"Tmdb":"123"}}],"TotalRecordCount":2}`}},
			wantID: "item-1", wantFound: true, wantRequests: []int32{1},
		},
		{
			name: "total count greater than one still matches", tmdbID: 123,
			responses: []localMediaIndexResponse{{body: singleLocalMediaIndexItemWithCount("item-1", "123", 2)}},
			wantID:    "item-1", wantFound: true, wantRequests: []int32{1},
		},
		{
			name: "later server may provide unique match", tmdbID: 123,
			responses: []localMediaIndexResponse{
				{body: `{"Items":[],"TotalRecordCount":0}`},
				{body: singleLocalMediaIndexItem("item-2", "123")},
			},
			wantID: "item-2", wantFound: true, wantRequests: []int32{1, 1},
		},
		{
			name: "first server wins when two servers hold the item", tmdbID: 123,
			responses: []localMediaIndexResponse{
				{body: singleLocalMediaIndexItem("item-1", "123")},
				{body: singleLocalMediaIndexItem("item-2", "123")},
			},
			wantID: "item-1", wantFound: true, wantRequests: []int32{1, 0},
		},
		{
			name: "HTTP failure is sanitized", tmdbID: 123,
			responses: []localMediaIndexResponse{{status: http.StatusInternalServerError, body: "secret error page"}},
			wantErr:   true, wantRequests: []int32{1},
		},
		{
			name: "invalid JSON is sanitized", tmdbID: 123,
			responses: []localMediaIndexResponse{{body: `{"ProviderIds":"secret response"`}},
			wantErr:   true, wantRequests: []int32{1},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runtime, requests, serverURLs := newLocalMediaIndexTestRuntime(t, tt.tmdbID, tt.responses)
			itemID, found, err := runtime.FindLocalMediaServerItem(t.Context(), httpapi.LocalMediaExistsQuery{
				TMDBID: tt.tmdbID,
				Title:  "/private/media/secret-title.mkv",
			})
			if tt.wantErr {
				if !errors.Is(err, errLocalMediaServerLookup) {
					t.Fatalf("FindLocalMediaServerItem() error = %v, want stable lookup error", err)
				}
				assertLocalMediaIndexErrorSanitized(t, err, serverURLs...)
			} else if err != nil {
				t.Fatalf("FindLocalMediaServerItem() error = %v", err)
			}
			if itemID != tt.wantID || found != tt.wantFound {
				t.Fatalf(
					"FindLocalMediaServerItem() = (%q, %v), want (%q, %v)",
					itemID, found, tt.wantID, tt.wantFound,
				)
			}
			for index, want := range tt.wantRequests {
				if got := requests[index].Load(); got != want {
					t.Errorf("server %d requests = %d, want %d", index, got, want)
				}
			}
		})
	}
}

func TestMediaServerRuntimeFindLocalMediaServerItemPropagatesContext(t *testing.T) {
	t.Run("already canceled", func(t *testing.T) {
		var requests atomic.Int32
		runtime, _ := newLocalMediaIndexRuntime(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			requests.Add(1)
		}))
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		_, found, err := runtime.FindLocalMediaServerItem(ctx, httpapi.LocalMediaExistsQuery{TMDBID: 123})
		if found || !errors.Is(err, context.Canceled) {
			t.Fatalf("FindLocalMediaServerItem() = (found=%v, err=%v), want context.Canceled", found, err)
		}
		if got := requests.Load(); got != 0 {
			t.Fatalf("requests = %d, want 0", got)
		}
	})

	t.Run("canceled in request", func(t *testing.T) {
		started := make(chan struct{})
		runtime, _ := newLocalMediaIndexRuntime(t, http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
			close(started)
			<-request.Context().Done()
		}))
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			_, _, err := runtime.FindLocalMediaServerItem(ctx, httpapi.LocalMediaExistsQuery{TMDBID: 123})
			done <- err
		}()
		<-started
		cancel()

		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("FindLocalMediaServerItem() error = %v, want context.Canceled", err)
		}
	})
}

type localMediaIndexResponse struct {
	status int
	body   string
}

func newLocalMediaIndexTestRuntime(
	t *testing.T,
	tmdbID int64,
	responses []localMediaIndexResponse,
) (*mediaServerRuntime, []*atomic.Int32, []string) {
	t.Helper()
	handlers := make([]http.Handler, 0, len(responses))
	requests := make([]*atomic.Int32, 0, len(responses))
	for _, response := range responses {
		requestCount := &atomic.Int32{}
		requests = append(requests, requestCount)
		handlers = append(handlers, http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
			requestCount.Add(1)
			assertLocalMediaIndexRequest(t, request, tmdbID)
			status := response.status
			if status == 0 {
				status = http.StatusOK
			}
			w.WriteHeader(status)
			_, _ = w.Write([]byte(response.body))
		}))
	}
	runtime, serverURLs := newLocalMediaIndexRuntime(t, handlers...)
	return runtime, requests, serverURLs
}

func newLocalMediaIndexRuntime(
	t *testing.T,
	handlers ...http.Handler,
) (*mediaServerRuntime, []string) {
	t.Helper()
	entries := make([]*mediaServerEntry, 0, len(handlers))
	serverURLs := make([]string, 0, len(handlers))
	for _, handler := range handlers {
		server := httptest.NewServer(handler)
		t.Cleanup(server.Close)
		client, err := emby.NewClient(emby.Config{
			BaseURL: server.URL,
			Token:   "fixture-token",
			Timeout: time.Second,
		})
		if err != nil {
			t.Fatalf("emby.NewClient() error = %v", err)
		}
		t.Cleanup(client.CloseIdleConnections)
		entries = append(entries, &mediaServerEntry{client: client})
		serverURLs = append(serverURLs, server.URL)
	}
	return &mediaServerRuntime{entries: entries}, serverURLs
}

func assertLocalMediaIndexRequest(t *testing.T, request *http.Request, tmdbID int64) {
	t.Helper()
	if request.Method != http.MethodGet || request.URL.Path != "/Items" {
		t.Errorf("request = %s %s, want GET /Items", request.Method, request.URL.Path)
	}
	query := request.URL.Query()
	wantProvider := fmt.Sprintf("tmdb.%d", tmdbID)
	wants := map[string]string{
		"AnyProviderIdEquals": wantProvider,
		"IncludeItemTypes":    "Movie,Episode",
		"Recursive":           "true",
		"Fields":              "ProviderIds",
		"Limit":               "2",
	}
	for key, want := range wants {
		if got := query.Get(key); got != want {
			t.Errorf("query %s = %q, want %q", key, got, want)
		}
	}
	if len(query) != len(wants) || query.Has("api_key") {
		t.Errorf("unexpected query: %v", query)
	}
	if request.Header.Get("X-Emby-Token") != "fixture-token" {
		t.Errorf("X-Emby-Token = %q", request.Header.Get("X-Emby-Token"))
	}
}

func singleLocalMediaIndexItem(itemID, tmdbID string) string {
	return singleLocalMediaIndexItemWithCount(itemID, tmdbID, 1)
}

func singleLocalMediaIndexItemWithCount(itemID, tmdbID string, count int) string {
	return fmt.Sprintf(
		`{"Items":[{"Id":%q,"ProviderIds":{"Tmdb":%q}}],"TotalRecordCount":%d}`,
		itemID,
		tmdbID,
		count,
	)
}

func assertLocalMediaIndexErrorSanitized(t *testing.T, err error, serverURLs ...string) {
	t.Helper()
	secrets := append(serverURLs, "fixture-token", "tmdb.123", "/private/media/secret-title.mkv", "secret error page", "secret response")
	for _, secret := range secrets {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("error leaked %q: %q", secret, err)
		}
	}
}

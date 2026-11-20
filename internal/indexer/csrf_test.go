package indexer

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestDeclarativePOSTCoalescesConcurrentCSRFFetches(t *testing.T) {
	var tokenRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			tokenRequests.Add(1)
			time.Sleep(20 * time.Millisecond)
			_, _ = w.Write([]byte(`<meta name="x-csrf-token" content="fixture-token">`))
		case "/search":
			if r.Header.Get("X-CSRF-TOKEN") != "fixture-token" {
				http.Error(w, "missing token", http.StatusForbidden)
				return
			}
			_, _ = w.Write([]byte(`{"data":[]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	adapter, err := NewDeclarativeAdapter(newIndexerHTTPClient(t, 8), Definition{
		Name: "csrf-concurrent", BaseURL: server.URL, SearchPath: "/search", Format: "json", Method: http.MethodPost,
		ResultPath: "data", Body: map[string]any{"keyword": "${keyword}"},
		CSRFPath: "/token", CSRFRegex: `name="x-csrf-token" content="([^"]+)"`,
	})
	if err != nil {
		t.Fatal(err)
	}
	var group sync.WaitGroup
	errorsFound := make(chan error, 16)
	for index := range 16 {
		group.Add(1)
		go func() {
			defer group.Done()
			_, searchErr := adapter.Search(context.Background(), Query{Keyword: fmt.Sprint(index), Limit: 10})
			if searchErr != nil {
				errorsFound <- searchErr
			}
		}()
	}
	group.Wait()
	close(errorsFound)
	for searchErr := range errorsFound {
		t.Error(searchErr)
	}
	if got := tokenRequests.Load(); got != 1 {
		t.Fatalf("concurrent CSRF token fetches=%d want=1", got)
	}
}

func TestDeclarativePOSTRefreshesRejectedCSRFTokenOnce(t *testing.T) {
	var tokenRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			if r.Header.Get("X-CSRF-TOKEN") != "" {
				t.Error("stale CSRF token leaked into refresh request")
			}
			requestNumber := tokenRequests.Add(1)
			csrfMarker := "expired-token"
			if requestNumber > 1 {
				csrfMarker = "fresh-token"
			}
			_, _ = fmt.Fprintf(w, `<meta name="x-csrf-token" content="%s">`, csrfMarker)
		case "/search":
			if r.Header.Get("X-CSRF-TOKEN") == "expired-token" {
				http.Error(w, "expired", http.StatusForbidden)
				return
			}
			if r.Header.Get("X-CSRF-TOKEN") != "fresh-token" {
				http.Error(w, "invalid", http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte(`{"data":[]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	adapter, err := NewDeclarativeAdapter(newIndexerHTTPClient(t, 2), Definition{
		Name: "csrf-refresh", BaseURL: server.URL, SearchPath: "/search", Format: "json", Method: http.MethodPost,
		ResultPath: "data", Body: map[string]any{"keyword": "${keyword}"},
		CSRFPath: "/token", CSRFRegex: `name="x-csrf-token" content="([^"]+)"`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.Search(context.Background(), Query{Keyword: "fixture", Limit: 10}); err != nil {
		t.Fatal(err)
	}
	if got := tokenRequests.Load(); got != 2 {
		t.Fatalf("CSRF token fetches=%d want=2", got)
	}
}

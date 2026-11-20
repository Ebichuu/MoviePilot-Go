package emby

import (
	"crypto/tls"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestSameOriginRedirectLimit(t *testing.T) {
	tests := []struct {
		name      string
		redirects int
		wantCode  ErrorCode
		wantCalls int32
	}{
		{name: "two redirects allowed", redirects: 2, wantCalls: 3},
		{name: "third redirect rejected", redirects: 3, wantCode: ErrorCodeRedirect, wantCalls: 3},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				call := int(calls.Add(1))
				if r.Header.Get("X-Emby-Token") != "fixture-token" || r.Header.Get("Authorization") == "" {
					t.Error("redirect lost authentication headers")
				}
				if call <= tt.redirects {
					http.Redirect(w, r, "/redirect/"+string(rune('0'+call)), http.StatusTemporaryRedirect)
					return
				}
				_, _ = w.Write([]byte(`{"Id":"server"}`))
			}))
			t.Cleanup(server.Close)

			_, err := newTestClient(t, server.URL).SystemInfo(t.Context())
			if tt.wantCode == "" {
				if err != nil {
					t.Fatalf("SystemInfo() error = %v", err)
				}
			} else {
				assertErrorCode(t, err, tt.wantCode)
			}
			if got := calls.Load(); got != tt.wantCalls {
				t.Fatalf("handler calls = %d, want %d", got, tt.wantCalls)
			}
		})
	}
}

func TestCrossOriginRedirectIsRejectedBeforeCredentialForwarding(t *testing.T) {
	var targetCalls atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		targetCalls.Add(1)
	}))
	t.Cleanup(target.Close)
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/capture", http.StatusTemporaryRedirect)
	}))
	t.Cleanup(source.Close)

	_, err := newTestClient(t, source.URL).SystemInfo(t.Context())
	assertErrorCode(t, err, ErrorCodeRedirect)
	if got := targetCalls.Load(); got != 0 {
		t.Fatalf("cross-origin target received %d requests", got)
	}
}

func TestRedirectCannotIntroduceAPIKeyQuery(t *testing.T) {
	var finalCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/System/Info" {
			http.Redirect(w, r, "/final?api_key=forbidden", http.StatusTemporaryRedirect)
			return
		}
		finalCalls.Add(1)
	}))
	t.Cleanup(server.Close)

	_, err := newTestClient(t, server.URL).SystemInfo(t.Context())
	assertErrorCode(t, err, ErrorCodeRedirect)
	if got := finalCalls.Load(); got != 0 {
		t.Fatalf("redirect with api_key reached final handler %d times", got)
	}
}

func TestHTTPSDowngradeRedirectIsRejected(t *testing.T) {
	server := httptest.NewTLSServer(nil)
	server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		target := strings.Replace(server.URL, "https://", "http://", 1) + "/capture"
		http.Redirect(w, r, target, http.StatusTemporaryRedirect)
	})
	t.Cleanup(server.Close)

	client := newTestClient(t, server.URL)
	transport, ok := client.httpClient.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T", client.httpClient.Transport)
	}
	transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} // Test server certificate only.

	_, err := client.SystemInfo(t.Context())
	assertErrorCode(t, err, ErrorCodeRedirect)
}

func TestRefreshRejectsMethodChangingRedirectWithoutRetry(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Redirect(w, r, "/different", http.StatusFound)
	}))
	t.Cleanup(server.Close)

	err := newTestClient(t, server.URL).RefreshAll(t.Context())
	assertErrorCode(t, err, ErrorCodeRedirect)
	if errors.Is(err, ErrOutcomeUnknown) {
		t.Fatalf("redirect error incorrectly classified as outcome unknown: %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("handler calls = %d, want 1", got)
	}
}

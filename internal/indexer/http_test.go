package indexer

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBlockedIPRejectsSpecialPurposeNetworks(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{"0.1.2.3", "100.64.0.1", "100.127.255.254", "198.18.0.1", "198.19.255.254"} {
		if !blockedIP(net.ParseIP(raw)) {
			t.Errorf("special-purpose address was not blocked: %s", raw)
		}
	}
	for _, raw := range []string{"1.1.1.1", "8.8.8.8"} {
		if blockedIP(net.ParseIP(raw)) {
			t.Errorf("public address was unexpectedly blocked: %s", raw)
		}
	}
}

func TestHTTPClientGetLimitedUsesSmallerResponseBudget(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", 32)))
	}))
	defer server.Close()
	client := newIndexerHTTPClient(t, 1)
	if _, err := client.getLimited(context.Background(), server.URL, nil, 8); err == nil || !strings.Contains(err.Error(), "8 字节") {
		t.Fatalf("limited response budget was not enforced: %v", err)
	}
}

func TestHTTPClientRejectsOversizedResponseHeaders(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Oversized", strings.Repeat("x", maxSiteResponseHeaderBytes+1024))
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	client := newIndexerHTTPClient(t, 1)
	if _, err := client.get(context.Background(), server.URL, nil); err == nil {
		t.Fatal("oversized response headers were accepted")
	}
}

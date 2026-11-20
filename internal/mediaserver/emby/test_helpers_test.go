package emby

import (
	"net/http"
	"testing"
	"time"
)

func newTestClient(t *testing.T, baseURL string) *Client {
	t.Helper()
	client, err := NewClient(Config{
		BaseURL:       baseURL,
		Token:         "fixture-token",
		Authorization: "MediaBrowser Client=MoviePilot-Go",
		Timeout:       2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.CloseIdleConnections)
	return client
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (function roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

type temporaryError string

func (err temporaryError) Error() string { return string(err) }
func (temporaryError) Timeout() bool     { return false }
func (temporaryError) Temporary() bool   { return true }

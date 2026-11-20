package emby

import (
	"compress/gzip"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestCompressedSuccessBodyLimitAppliesAfterDecompression(t *testing.T) {
	tests := []struct {
		name string
		path string
		body string
		call func(*Client) error
	}{
		{
			name: "system info limit", path: "/System/Info",
			body: `{"ServerName":"` + strings.Repeat("x", int(maxSystemInfoBytes)) + `"}`,
			call: func(client *Client) error { _, err := client.SystemInfo(t.Context()); return err },
		},
		{
			name: "items limit", path: "/Items",
			body: `{"Items":[],"FuturePadding":"` + strings.Repeat("x", int(maxItemsBytes)) + `"}`,
			call: func(client *Client) error { _, err := client.FindItems(t.Context(), FindItemsOptions{}); return err },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != tt.path {
					t.Errorf("path = %q, want %q", r.URL.Path, tt.path)
				}
				w.Header().Set("Content-Encoding", "gzip")
				writer := gzip.NewWriter(w)
				if _, err := writer.Write([]byte(tt.body)); err != nil {
					t.Errorf("write gzip response: %v", err)
				}
				if err := writer.Close(); err != nil {
					t.Errorf("close gzip response: %v", err)
				}
			}))
			t.Cleanup(server.Close)

			err := tt.call(newTestClient(t, server.URL))
			assertErrorCode(t, err, ErrorCodeResponseTooLarge)
		})
	}
}

func TestErrorBodyReadIsCappedAt64KiB(t *testing.T) {
	reader := &countingReadCloser{reader: strings.NewReader(strings.Repeat("x", int(maxErrorBodyBytes*2)))}
	client := newTestClient(t, "http://emby.invalid")
	client.httpClient.Transport = roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusInternalServerError,
			Header:     make(http.Header),
			Body:       reader,
			Request:    request,
		}, nil
	})

	_, err := client.SystemInfo(t.Context())
	assertErrorCode(t, err, ErrorCodeHTTPStatus)
	if got, want := reader.read.Load(), maxErrorBodyBytes+1; got > want {
		t.Fatalf("error body bytes read = %d, want <= %d", got, want)
	}
}

func TestErrorsDoNotLeakSensitiveInputsOrResponses(t *testing.T) {
	var (
		token         = strings.Join([]string{"fixture", "token", "value"}, "-")
		authorization = strings.Join([]string{"MediaBrowser", "FixtureAuthorization"}, " ")
	)
	const (
		mediaPath  = "/private/media/secret-title.mkv"
		providerID = "tmdb.secret-provider-id"
		errorPage  = "<html>secret-error-page</html>"
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, errorPage, http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)
	client, err := NewClient(Config{
		BaseURL: server.URL, Token: token, Authorization: authorization,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.CloseIdleConnections)

	_, err = client.FindItems(t.Context(), FindItemsOptions{
		Path: mediaPath, AnyProviderIDEquals: providerID, Limit: 1,
	})
	assertErrorCode(t, err, ErrorCodeHTTPStatus)
	assertNoSecrets(t, err, token, authorization, mediaPath, providerID, errorPage, server.URL)
}

func TestTransportAndDecodeErrorsAreSanitized(t *testing.T) {
	tests := []struct {
		name           string
		body           string
		transportError error
		wantCode       ErrorCode
		secrets        []string
	}{
		{
			name: "transport error", transportError: errors.New("dial leaked http://emby.invalid/private?provider=secret"),
			wantCode: ErrorCodeTransport, secrets: []string{"emby.invalid", "provider=secret"},
		},
		{
			name: "invalid JSON", body: `{"Path":"/private/media/secret"`,
			wantCode: ErrorCodeInvalidResponse, secrets: []string{"/private/media/secret"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := newTestClient(t, "http://emby.invalid")
			client.httpClient.Transport = roundTripperFunc(func(request *http.Request) (*http.Response, error) {
				if tt.transportError != nil {
					return nil, tt.transportError
				}
				return jsonHTTPResponse(request, tt.body), nil
			})
			_, err := client.SystemInfo(t.Context())
			assertErrorCode(t, err, tt.wantCode)
			assertNoSecrets(t, err, tt.secrets...)
		})
	}
}

type countingReadCloser struct {
	reader io.Reader
	read   atomic.Int64
}

func (reader *countingReadCloser) Read(buffer []byte) (int, error) {
	count, err := reader.reader.Read(buffer)
	reader.read.Add(int64(count))
	return count, err
}

func (*countingReadCloser) Close() error { return nil }

func assertNoSecrets(t *testing.T, err error, secrets ...string) {
	t.Helper()
	for _, secret := range secrets {
		if secret != "" && strings.Contains(err.Error(), secret) {
			t.Fatalf("error leaked %q: %q", secret, err)
		}
	}
}

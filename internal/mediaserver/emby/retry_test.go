package emby

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestGETStatusRetryPolicy(t *testing.T) {
	tests := []struct {
		name      string
		statuses  []int
		wantCalls int32
		wantCode  ErrorCode
	}{
		{
			name:      "503 and 502 then success",
			statuses:  []int{http.StatusServiceUnavailable, http.StatusBadGateway, http.StatusOK},
			wantCalls: 3,
		},
		{name: "429 honors retry after", statuses: []int{http.StatusTooManyRequests, http.StatusOK}, wantCalls: 2},
		{
			name: "500 is not retried", statuses: []int{http.StatusInternalServerError},
			wantCalls: 1, wantCode: ErrorCodeHTTPStatus,
		},
		{
			name:      "retry budget is three attempts",
			statuses:  []int{http.StatusGatewayTimeout, http.StatusGatewayTimeout, http.StatusGatewayTimeout, http.StatusOK},
			wantCalls: 3, wantCode: ErrorCodeHTTPStatus,
		},
		{
			name:      "exhausted 429 remains rate limited",
			statuses:  []int{http.StatusTooManyRequests, http.StatusTooManyRequests, http.StatusTooManyRequests},
			wantCalls: 3, wantCode: ErrorCodeRateLimited,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				call := int(calls.Add(1)) - 1
				status := tt.statuses[min(call, len(tt.statuses)-1)]
				if status == http.StatusTooManyRequests {
					w.Header().Set("Retry-After", "0")
				}
				w.WriteHeader(status)
				if status == http.StatusOK {
					_, _ = w.Write([]byte(`{"Id":"server"}`))
				}
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
				t.Fatalf("request count = %d, want %d", got, tt.wantCalls)
			}
		})
	}
}

func TestGETTemporaryTransportRetryPolicy(t *testing.T) {
	tests := []struct {
		name      string
		failures  int32
		temporary bool
		wantCalls int32
		wantCode  ErrorCode
	}{
		{name: "temporary failures recover", failures: 2, temporary: true, wantCalls: 3},
		{name: "temporary failures stop at three", failures: 4, temporary: true, wantCalls: 3, wantCode: ErrorCodeTransport},
		{name: "permanent failure is not retried", failures: 1, wantCalls: 1, wantCode: ErrorCodeTransport},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := newTestClient(t, "http://emby.invalid")
			var calls atomic.Int32
			client.httpClient.Transport = roundTripperFunc(func(request *http.Request) (*http.Response, error) {
				call := calls.Add(1)
				if request.URL.Query().Has("api_key") {
					t.Error("request contains forbidden api_key")
				}
				if call <= tt.failures {
					if tt.temporary {
						return nil, temporaryError("transport secret: " + request.URL.String())
					}
					return nil, errors.New("permanent transport secret: " + request.URL.String())
				}
				return jsonHTTPResponse(request, `{"Id":"server"}`), nil
			})

			_, err := client.SystemInfo(t.Context())
			if tt.wantCode == "" {
				if err != nil {
					t.Fatalf("SystemInfo() error = %v", err)
				}
			} else {
				assertErrorCode(t, err, tt.wantCode)
			}
			if got := calls.Load(); got != tt.wantCalls {
				t.Fatalf("transport calls = %d, want %d", got, tt.wantCalls)
			}
			if err != nil && strings.Contains(err.Error(), "emby.invalid") {
				t.Fatalf("error leaked URL: %q", err)
			}
		})
	}
}

func TestHTTPStatusCodeIsPreserved(t *testing.T) {
	for _, status := range []int{
		http.StatusUnauthorized,
		http.StatusForbidden,
		http.StatusNotFound,
		http.StatusInternalServerError,
	} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(status)
			}))
			t.Cleanup(server.Close)

			_, err := newTestClient(t, server.URL).SystemInfo(t.Context())
			var embyErr *Error
			if !errors.As(err, &embyErr) || embyErr.Code != ErrorCodeHTTPStatus || embyErr.StatusCode != status {
				t.Fatalf("error = %#v, want http_status with status %d", err, status)
			}
		})
	}
}

func TestRefreshPOSTIsNotRetried(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		http.Error(w, "temporary backend error", http.StatusServiceUnavailable)
	}))
	t.Cleanup(server.Close)

	err := newTestClient(t, server.URL).RefreshAll(t.Context())
	assertErrorCode(t, err, ErrorCodeHTTPStatus)
	if got := calls.Load(); got != 1 {
		t.Fatalf("RefreshAll() requests = %d, want 1", got)
	}
}

func TestRefreshTransportFailureIsOutcomeUnknown(t *testing.T) {
	client := newTestClient(t, "http://emby.invalid")
	var calls atomic.Int32
	client.httpClient.Transport = roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, temporaryError("lost response for " + request.URL.String())
	})

	err := client.RefreshItem(t.Context(), "sensitive-item-id")
	assertErrorCode(t, err, ErrorCodeOutcomeUnknown)
	if !errors.Is(err, ErrOutcomeUnknown) {
		t.Fatalf("RefreshItem() error = %v, want ErrOutcomeUnknown", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("RefreshItem() calls = %d, want 1", got)
	}
	for _, secret := range []string{"emby.invalid", "sensitive-item-id"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("error leaked %q: %q", secret, err)
		}
	}
}

func TestRefreshResponseReadFailureIsOutcomeUnknown(t *testing.T) {
	client := newTestClient(t, "http://emby.invalid")
	client.httpClient.Transport = roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       errorReadCloser{err: temporaryError("secret response read failure")},
			Request:    request,
		}, nil
	})

	err := client.RefreshAll(t.Context())
	assertErrorCode(t, err, ErrorCodeOutcomeUnknown)
	if !errors.Is(err, ErrOutcomeUnknown) {
		t.Fatalf("RefreshAll() error = %v, want ErrOutcomeUnknown", err)
	}
	if strings.Contains(err.Error(), "secret response") {
		t.Fatalf("RefreshAll() leaked body error: %q", err)
	}
}

func TestRefreshCancellationAfterStartIsOutcomeUnknown(t *testing.T) {
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	}))
	t.Cleanup(server.Close)
	client := newTestClient(t, server.URL)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- client.RefreshAll(ctx) }()
	<-started
	cancel()

	err := <-done
	assertErrorCode(t, err, ErrorCodeOutcomeUnknown)
	if !errors.Is(err, ErrOutcomeUnknown) || !errors.Is(err, context.Canceled) {
		t.Fatalf("RefreshAll() error = %v, want outcome unknown and context canceled", err)
	}
}

func TestCancellationStopsRetryWait(t *testing.T) {
	responded := make(chan struct{})
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusServiceUnavailable)
		close(responded)
	}))
	t.Cleanup(server.Close)
	client := newTestClient(t, server.URL)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := client.SystemInfo(ctx)
		done <- err
	}()
	<-responded
	time.Sleep(10 * time.Millisecond)
	cancel()

	err := <-done
	assertErrorCode(t, err, ErrorCodeCanceled)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("SystemInfo() error = %v, want context.Canceled", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("request count = %d, want 1", got)
	}
}

func TestRetryAfterDelayIsBounded(t *testing.T) {
	if got := retryDelay(0, "9223372036854775807"); got != maximumRetryDelay {
		t.Fatalf("retryDelay() = %s, want %s", got, maximumRetryDelay)
	}
}

func TestClientConcurrencyLimitAndAdmissionCancellation(t *testing.T) {
	var active atomic.Int32
	var maximum atomic.Int32
	var once sync.Once
	fourStarted := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		current := active.Add(1)
		defer active.Add(-1)
		updateMaximum(&maximum, current)
		if current == maxConcurrentRequests {
			once.Do(func() { close(fourStarted) })
		}
		<-release
		_, _ = w.Write([]byte(`{"Id":"server"}`))
	}))
	t.Cleanup(server.Close)
	client := newTestClient(t, server.URL)

	const callers = 8
	done := make(chan error, callers)
	for range callers {
		go func() {
			_, err := client.SystemInfo(context.Background())
			done <- err
		}()
	}
	<-fourStarted

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := client.SystemInfo(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("waiting SystemInfo() error = %v, want context.Canceled", err)
	}
	time.Sleep(25 * time.Millisecond)
	if got := maximum.Load(); got > maxConcurrentRequests {
		t.Fatalf("maximum concurrent requests = %d, want <= %d", got, maxConcurrentRequests)
	}
	close(release)
	for range callers {
		if err := <-done; err != nil {
			t.Fatalf("SystemInfo() error = %v", err)
		}
	}
}

func jsonHTTPResponse(request *http.Request, body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    request,
	}
}

func assertErrorCode(t *testing.T, err error, code ErrorCode) {
	t.Helper()
	var embyErr *Error
	if !errors.As(err, &embyErr) || embyErr.Code != code {
		t.Fatalf("error = %v, want code %s", err, code)
	}
}

func updateMaximum(maximum *atomic.Int32, value int32) {
	for {
		current := maximum.Load()
		if value <= current || maximum.CompareAndSwap(current, value) {
			return
		}
	}
}

type errorReadCloser struct {
	err error
}

func (reader errorReadCloser) Read([]byte) (int, error) { return 0, reader.err }
func (errorReadCloser) Close() error                    { return nil }

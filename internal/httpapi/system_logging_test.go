package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/moviepilot-go/moviepilot-go/internal/auth"
	"github.com/moviepilot-go/moviepilot-go/internal/config"
	"github.com/moviepilot-go/moviepilot-go/internal/logbuf"
)

func TestSystemLoggingPlainAndSSEAndDownload(t *testing.T) {
	t.Parallel()

	buf := logbuf.New(100)
	buf.Append("2026-11-21 01:00:00 [INFO] line-1")
	buf.Append("2026-11-21 01:00:01 [WARN] line-2")

	authSvc := auth.NewService(nil, "log-test-jwt-secret", "log-test-resource-secret", time.Hour)
	admin := auth.User{ID: 1, Username: "admin", SuperUser: true}
	accessToken, err := authSvc.AccessToken(admin)
	if err != nil {
		t.Fatalf("AccessToken() error = %v", err)
	}
	resToken, err := authSvc.ResourceToken(admin, time.Hour)
	if err != nil {
		t.Fatalf("ResourceToken() error = %v", err)
	}

	router := NewRouter(Dependencies{
		Config:    config.Config{MaxRequestBody: 1 << 20, FrontendDir: t.TempDir()},
		Auth:      authSvc,
		Users:     fakeUserLookup{user: admin},
		LogBuffer: buf,
	})

	t.Run("plain text length=-1 with Bearer", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/system/logging?length=-1", nil)
		req.Header.Set("Authorization", "Bearer "+accessToken)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Header().Get("Content-Type"), "text/plain") {
			t.Fatalf("Content-Type = %q", rec.Header().Get("Content-Type"))
		}
		if !strings.Contains(rec.Body.String(), "line-1") || !strings.Contains(rec.Body.String(), "line-2") {
			t.Fatalf("body = %q", rec.Body.String())
		}
	})

	t.Run("SSE with resource cookie streams history and live", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		req := httptest.NewRequest(http.MethodGet, "/api/v1/system/logging?length=10", nil).WithContext(ctx)
		req.AddCookie(&http.Cookie{Name: resourceCookieName, Value: resToken})
		rec := httptest.NewRecorder()

		done := make(chan struct{})
		go func() {
			router.ServeHTTP(rec, req)
			close(done)
		}()

		time.Sleep(40 * time.Millisecond)
		buf.Append("2026-11-21 01:00:02 [ERROR] live-line-3")
		time.Sleep(40 * time.Millisecond)
		cancel()
		<-done

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		if !strings.Contains(rec.Header().Get("Content-Type"), "text/event-stream") {
			t.Fatalf("Content-Type = %q", rec.Header().Get("Content-Type"))
		}
		body := rec.Body.String()
		if !strings.Contains(body, "data: 2026-11-21 01:00:00 [INFO] line-1") || !strings.Contains(body, "data: 2026-11-21 01:00:02 [ERROR] live-line-3") {
			t.Fatalf("SSE body missing lines:\n%s", body)
		}
	})

	t.Run("download log attachment", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/system/logging/download/moviepilot.log", nil)
		req.Header.Set("Authorization", "Bearer "+accessToken)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		if !strings.Contains(rec.Header().Get("Content-Disposition"), "moviepilot.log") {
			t.Fatalf("Content-Disposition = %q", rec.Header().Get("Content-Disposition"))
		}
		if !strings.Contains(rec.Body.String(), "line-2") {
			t.Fatalf("download body = %q", rec.Body.String())
		}
	})
}

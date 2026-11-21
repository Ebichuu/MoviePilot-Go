package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/moviepilot-go/moviepilot-go/internal/config"
)

type fakeWebhookNotifier struct {
	events []WebhookEvent
}

func (f *fakeWebhookNotifier) NotifyWebhook(_ context.Context, event WebhookEvent) error {
	f.events = append(f.events, event)
	return nil
}

func TestWebhookReceivesEmbyJellyfinAndPlex(t *testing.T) {
	t.Parallel()

	notifier := &fakeWebhookNotifier{}
	router := NewRouter(Dependencies{
		Config: config.Config{
			APIToken:       "secret-api-token",
			MaxRequestBody: 1 << 20,
			FrontendDir:    t.TempDir(),
		},
		WebhookNotifier: notifier,
	})

	t.Run("rejects missing token", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/webhook/", strings.NewReader(`{"Event":"playback.start"}`))
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", rec.Code)
		}
	})

	t.Run("Emby playback.start JSON", func(t *testing.T) {
		body := `{"Event":"playback.start","Server":{"Name":"HomeEmby"},"User":{"Name":"alice"},"Session":{"Client":"Emby Web","DeviceName":"Chrome"},"Item":{"Type":"Episode","SeriesName":"Frieren","Name":"Journey's End","ParentIndexNumber":1,"IndexNumber":5}}`
		req := httptest.NewRequest(http.MethodPost, "/api/v1/webhook/?token=secret-api-token", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		var resp Response
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || !resp.Success {
			t.Fatalf("response = %s", rec.Body.String())
		}
	})

	t.Run("Jellyfin ItemAdded JSON", func(t *testing.T) {
		body := `{"NotificationType":"ItemAdded","ServerName":"HomeJF","Name":"Dune: Part Two","ItemType":"Movie","NotificationUsername":"bob"}`
		req := httptest.NewRequest(http.MethodPost, "/api/v1/webhook/?token=secret-api-token", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
	})

	t.Run("Plex multipart payload", func(t *testing.T) {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		_ = mw.WriteField("payload", `{"event":"media.play","Account":{"title":"carol"},"Server":{"title":"HomePlex"},"Player":{"title":"Apple TV"},"Metadata":{"type":"movie","title":"Oppenheimer"}}`)
		_ = mw.Close()

		req := httptest.NewRequest(http.MethodPost, "/api/v1/webhook/?token=secret-api-token", &buf)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
	})

	if len(notifier.events) != 3 {
		t.Fatalf("notified events = %d, want 3: %+v", len(notifier.events), notifier.events)
	}
	if notifier.events[0].Source != "emby" || !strings.Contains(notifier.events[0].Title, "Frieren S01E05") {
		t.Fatalf("emby event = %+v", notifier.events[0])
	}
	if notifier.events[1].Source != "jellyfin" || !strings.Contains(notifier.events[1].Title, "新入库") {
		t.Fatalf("jellyfin event = %+v", notifier.events[1])
	}
	if notifier.events[2].Source != "plex" || !strings.Contains(notifier.events[2].Title, "Oppenheimer") {
		t.Fatalf("plex event = %+v", notifier.events[2])
	}
}

func TestSeerrSubscribeAuthAndValidation(t *testing.T) {
	t.Parallel()

	router := NewRouter(Dependencies{
		Config: config.Config{
			APIToken:       "seerr-token",
			MaxRequestBody: 1 << 20,
			FrontendDir:    t.TempDir(),
		},
	})

	// 未携带 Authorization 返回 400（对齐 V2）。
	req := httptest.NewRequest(http.MethodPost, "/api/v1/subscribe/seerr", strings.NewReader(`{"notification_type":"TEST_NOTIFICATION"}`))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unauthorized status = %d, want 400", rec.Code)
	}

	// 携带正确的 API_TOKEN 且测试通知返回 success=false（无 Store）或有 Store 时 success=true。
	req = httptest.NewRequest(http.MethodPost, "/api/v1/subscribe/seerr", strings.NewReader(`{"notification_type":"TEST_NOTIFICATION"}`))
	req.Header.Set("Authorization", "seerr-token")
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if seasons := parseSeerrSeasons([]struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	}{{Name: "Requested Seasons", Value: "1, 2, 5"}}); len(seasons) != 3 || seasons[2] != 5 {
		t.Fatalf("parseSeerrSeasons = %v", seasons)
	}
}

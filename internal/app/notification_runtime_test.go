package app

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/moviepilot-go/moviepilot-go/internal/notification"
)

type appNotificationRuntimeTestStore struct {
	mu          sync.Mutex
	raw         json.RawMessage
	found       bool
	err         error
	active      int
	maxActive   int
	loadStarted chan struct{}
	loadRelease chan struct{}
}

func (s *appNotificationRuntimeTestStore) LoadNotificationSettings(context.Context) (json.RawMessage, bool, error) {
	s.mu.Lock()
	s.active++
	if s.active > s.maxActive {
		s.maxActive = s.active
	}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.active--
		s.mu.Unlock()
	}()
	if s.loadStarted != nil {
		select {
		case s.loadStarted <- struct{}{}:
		default:
		}
	}
	if s.loadRelease != nil {
		<-s.loadRelease
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return append(json.RawMessage(nil), s.raw...), s.found, s.err
}

func notificationRuntimeTargetNames(registry *notification.Registry, message notification.Message) []string {
	targets := registry.Targets(message)
	names := make([]string, 0, len(targets))
	for _, target := range targets {
		names = append(names, target.Source)
	}
	return names
}

func TestLoadNotificationRuntimeDatabaseValueIsAuthoritative(t *testing.T) {
	t.Setenv("MOVIEPILOT_TELEGRAM_BOT_TOKEN", "legacy-token")
	t.Setenv("MOVIEPILOT_TELEGRAM_CHAT_ID", "legacy-chat")
	store := &appNotificationRuntimeTestStore{found: true, raw: json.RawMessage(`null`)}
	runtime, err := loadNotificationRuntime(t.Context(), store, slog.Default())
	if err != nil {
		t.Fatalf("loadNotificationRuntime() error = %v", err)
	}
	if runtime == nil || runtime.Registry() == nil {
		t.Fatal("loadNotificationRuntime() returned nil runtime/registry")
	}
	if got := notificationRuntimeTargetNames(runtime.Registry(), notification.Message{Type: notification.TypeOrganize}); len(got) != 0 {
		t.Fatalf("database null unexpectedly used environment fallback: %v", got)
	}
}

func TestLoadNotificationRuntimeFallsBackOnlyWhenDatabaseSettingMissing(t *testing.T) {
	t.Setenv("MOVIEPILOT_TELEGRAM_BOT_TOKEN", "legacy-token")
	t.Setenv("MOVIEPILOT_TELEGRAM_CHAT_ID", "legacy-chat")
	store := &appNotificationRuntimeTestStore{}
	runtime, err := loadNotificationRuntime(t.Context(), store, slog.Default())
	if err != nil {
		t.Fatalf("loadNotificationRuntime() error = %v", err)
	}
	want := []string{"Telegram"}
	if got := notificationRuntimeTargetNames(runtime.Registry(), notification.Message{Type: notification.TypeOrganize}); len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("environment fallback targets = %v, want %v", got, want)
	}
}

func TestNotificationRuntimeReloadFailureKeepsPreviousSnapshot(t *testing.T) {
	store := &appNotificationRuntimeTestStore{found: true, raw: json.RawMessage(`[{"name":"Primary","type":"telegram","config":{"TELEGRAM_TOKEN":"token","TELEGRAM_CHAT_ID":"1"},"switchs":["整理入库"],"enabled":true}]`)}
	runtime, err := loadNotificationRuntime(t.Context(), store, slog.Default())
	if err != nil {
		t.Fatalf("initial load error = %v", err)
	}
	store.mu.Lock()
	store.raw = json.RawMessage(`{"unterminated":`)
	store.mu.Unlock()
	if err := runtime.Reload(t.Context()); err == nil {
		t.Fatal("Reload() error = nil for malformed authoritative setting")
	}
	got := notificationRuntimeTargetNames(runtime.Registry(), notification.Message{Type: notification.TypeOrganize})
	if len(got) != 1 || got[0] != "Primary" {
		t.Fatalf("failed reload replaced previous snapshot: %v", got)
	}
}

func TestNotificationRuntimeReloadsAreSerialized(t *testing.T) {
	store := &appNotificationRuntimeTestStore{
		found:       true,
		raw:         json.RawMessage(`null`),
		loadStarted: make(chan struct{}, 2),
		loadRelease: make(chan struct{}),
	}
	runtime := &notificationRuntime{store: store, registry: notification.NewRegistry(slog.Default())}
	firstDone := make(chan error, 1)
	secondDone := make(chan error, 1)
	go func() { firstDone <- runtime.Reload(context.Background()) }()
	select {
	case <-store.loadStarted:
	case <-time.After(time.Second):
		t.Fatal("first reload did not reach store")
	}
	go func() { secondDone <- runtime.Reload(context.Background()) }()
	select {
	case <-store.loadStarted:
		t.Fatal("second reload entered store before first completed")
	case <-time.After(30 * time.Millisecond):
	}
	close(store.loadRelease)
	if err := <-firstDone; err != nil {
		t.Fatalf("first reload error = %v", err)
	}
	if err := <-secondDone; err != nil {
		t.Fatalf("second reload error = %v", err)
	}
	store.mu.Lock()
	maxActive := store.maxActive
	store.mu.Unlock()
	if maxActive != 1 {
		t.Fatalf("store observed %d concurrent loads, want 1", maxActive)
	}
}

func TestNotificationRuntimeNilSafety(t *testing.T) {
	var runtime *notificationRuntime
	if runtime.Registry() != nil {
		t.Fatal("nil runtime Registry() is non-nil")
	}
	if err := runtime.Reload(context.Background()); err == nil {
		t.Fatal("nil runtime Reload() error = nil")
	}
}

package app

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/moviepilot-go/moviepilot-go/internal/config"
	"github.com/moviepilot-go/moviepilot-go/internal/storage"
)

type memorySettingStore struct {
	data map[string][]byte
}

func (m *memorySettingStore) GetSetting(_ context.Context, key string, target any) (bool, error) {
	raw, ok := m.data[key]
	if !ok {
		return false, nil
	}
	if err := json.Unmarshal(raw, target); err != nil {
		return false, err
	}
	return true, nil
}

func (m *memorySettingStore) SetSetting(_ context.Context, key string, value any, _ bool) error {
	if m.data == nil {
		m.data = map[string][]byte{}
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	m.data[key] = raw
	return nil
}

func TestBuildStorageProviderRegistersSMBAlipanU115AndPersists(t *testing.T) {
	store := &memorySettingStore{}
	provider := buildStorageProvider(context.Background(), config.Config{
		StorageLocalPath: t.TempDir(),
	}, store, nil)

	reg, ok := provider.(*storage.ProviderRegistry)
	if !ok {
		t.Fatalf("expected *storage.ProviderRegistry, got %T", provider)
	}
	names := reg.Names()
	want := map[string]bool{"local": false, "smb": false, "alipan": false, "u115": false}
	for _, n := range names {
		if _, exists := want[n]; exists {
			want[n] = true
		}
	}
	for name, found := range want {
		if !found {
			t.Fatalf("missing registered storage %q in %v", name, names)
		}
	}

	// Saving SMB config persists into Storages setting
	if err := reg.SaveConfig(context.Background(), "smb", map[string]any{
		"host": "192.168.1.99", "share": "media", "port": 445,
	}); err != nil {
		t.Fatalf("SaveConfig smb: %v", err)
	}
	var entries []persistedStorageEntry
	found, err := store.GetSetting(context.Background(), storagesSettingKey, &entries)
	if err != nil || !found || len(entries) != 1 || entries[0].Type != "smb" || entries[0].Config["host"] != "192.168.1.99" {
		t.Fatalf("unexpected persisted entries: found=%v entries=%+v err=%v", found, entries, err)
	}
}

func TestBuildSubtitleCoordinator(t *testing.T) {
	empty := buildSubtitleCoordinator(config.Config{}, nil)
	if empty.Enabled() {
		t.Fatalf("expected disabled coordinator when no keys configured")
	}

	coord := buildSubtitleCoordinator(config.Config{
		OpenSubtitlesAPIKey: "os-key",
		AssrtToken:          "assrt-token",
		SubtitleLanguages:   []string{"zh-cn", "en"},
	}, nil)
	if !coord.Enabled() || coord.ProviderCount() != 2 {
		t.Fatalf("expected 2 subtitle providers, got enabled=%v count=%d", coord.Enabled(), coord.ProviderCount())
	}
}

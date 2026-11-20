//go:build integration

package database

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/moviepilot-go/moviepilot-go/internal/mediaconfig"
)

func TestMediaServersSettingPostgresPersistence(t *testing.T) {
	tests := []struct {
		name string
		run  func(*testing.T, *Store)
	}{
		{name: "加密保存并读取规范化快照", run: testMediaServersEncryptedRoundTrip},
		{name: "读取时升级旧明文和缺失 ID", run: testMediaServersLegacyUpgrade},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			baseStore := newSiteStatisticsIntegrationStore(t)
			store := NewStore(baseStore.db, newMediaServersSettingTestBox(t))
			tt.run(t, store)
		})
	}
}

func testMediaServersEncryptedRoundTrip(t *testing.T, store *Store) {
	t.Helper()
	servers := []mediaconfig.Server{{
		ID: "11111111-1111-4111-8111-111111111111", Name: " Home ", Type: "EMBY", Enabled: true,
		SyncInterval: 15, SyncLibraries: []string{" movies ", "movies"},
		PathMapping: []mediaconfig.PathMapping{{LocalRoot: "/mnt/media/", EmbyRoot: "/library/"}},
		Config: mediaconfig.Config{
			Host: " https://Emby.Example/emby/ ", Username: " admin ", APIKey: "apikey-secret-value",
		},
	}}

	saved, err := store.SaveMediaServers(t.Context(), servers)
	if err != nil {
		t.Fatalf("SaveMediaServers() error = %v", err)
	}
	assertMediaServersEncryptedRow(t, store)

	loaded, found, err := store.LoadMediaServers(t.Context())
	if err != nil {
		t.Fatalf("LoadMediaServers() error = %v", err)
	}
	if !found || loaded.RequiredID != saved.RequiredID {
		t.Fatalf("LoadMediaServers() found=%v snapshot=%#v, want required=%q", found, loaded, saved.RequiredID)
	}
	savedJSON, err := json.Marshal(saved.Servers)
	if err != nil {
		t.Fatalf("编码保存快照: %v", err)
	}
	loadedJSON, err := json.Marshal(loaded.Servers)
	if err != nil {
		t.Fatalf("编码读取快照: %v", err)
	}
	if !bytes.Equal(loadedJSON, savedJSON) {
		t.Fatalf("读取快照 = %s, want %s", loadedJSON, savedJSON)
	}
}

func testMediaServersLegacyUpgrade(t *testing.T, store *Store) {
	t.Helper()
	legacyValue := json.RawMessage(`[{"name":" Legacy ","type":"EMBY","enabled":true,"sync_interval":5,"config":{"host":" https://emby.example/emby/ ","apikey":"apikey-secret-value"}}]`)
	if _, err := store.db.Pool.Exec(t.Context(), `
		INSERT INTO settings (key, value, encrypted)
		VALUES ($1, $2::jsonb, FALSE)`, MediaServersSettingKey, legacyValue); err != nil {
		t.Fatalf("写入旧媒体服务器明文设置: %v", err)
	}

	loaded, found, err := store.LoadMediaServers(t.Context())
	if err != nil {
		t.Fatalf("LoadMediaServers() error = %v", err)
	}
	if !found || len(loaded.Servers) != 1 || loaded.Servers[0].ID == "" || loaded.RequiredID != loaded.Servers[0].ID {
		t.Fatalf("旧设置升级快照 = found=%v %#v", found, loaded)
	}
	firstID := loaded.Servers[0].ID
	assertMediaServersEncryptedRow(t, store)

	reloaded, found, err := store.LoadMediaServers(t.Context())
	if err != nil || !found || reloaded.Servers[0].ID != firstID {
		t.Fatalf("升级后稳定 ID 变化: found=%v err=%v snapshot=%#v", found, err, reloaded)
	}
}

func assertMediaServersEncryptedRow(t *testing.T, store *Store) {
	t.Helper()
	var storedValue []byte
	var encrypted bool
	if err := store.db.Pool.QueryRow(t.Context(), `
		SELECT value, encrypted
		FROM settings
		WHERE key=$1`, MediaServersSettingKey).Scan(&storedValue, &encrypted); err != nil {
		t.Fatalf("读取数据库媒体服务器设置: %v", err)
	}
	if !encrypted {
		t.Fatal("settings.encrypted = false, want true")
	}
	for _, plainText := range []string{"apikey-secret-value", "https://emby.example/emby", "admin"} {
		if bytes.Contains(storedValue, []byte(plainText)) {
			t.Fatalf("数据库值泄漏明文 %q: %s", plainText, storedValue)
		}
	}
}

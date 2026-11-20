//go:build integration

package database

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/moviepilot-go/moviepilot-go/internal/downloadconfig"
)

func TestDownloadersSettingPostgresEncryption(t *testing.T) {
	baseStore := newSiteStatisticsIntegrationStore(t)
	store := NewStore(baseStore.db, newDownloadersSettingTestBox(t))
	plainValue := json.RawMessage(`[{"id":"b7088afb-12ab-4a76-82b7-2f478428ec85","name":"qb","type":"qbittorrent","default":false,"enabled":false,"config":{"password":"password-secret","apikey":"apikey-secret"}}]`)

	settings, err := downloadconfig.Decode(plainValue)
	if err != nil {
		t.Fatalf("downloadconfig.Decode() error = %v", err)
	}
	if err := store.SaveDownloaders(t.Context(), settings); err != nil {
		t.Fatalf("SaveDownloaders() error = %v", err)
	}

	var storedValue []byte
	var encrypted bool
	if err := store.db.Pool.QueryRow(t.Context(), `
		SELECT value, encrypted
		FROM settings
		WHERE key=$1`, DownloadersSettingKey).Scan(&storedValue, &encrypted); err != nil {
		t.Fatalf("读取数据库下载器设置: %v", err)
	}
	if !encrypted {
		t.Fatal("settings.encrypted = false, want true")
	}
	for _, secret := range []string{"password-secret", "apikey-secret"} {
		if bytes.Contains(storedValue, []byte(secret)) {
			t.Fatalf("数据库值泄漏秘密 %q: %s", secret, storedValue)
		}
	}

	loaded, found, err := store.LoadDownloaders(t.Context())
	if err != nil {
		t.Fatalf("LoadDownloaders() error = %v", err)
	}
	if !found {
		t.Fatal("LoadDownloaders() found=false")
	}
	if !reflect.DeepEqual(loaded, settings) {
		t.Fatalf("LoadDownloaders() = %#v, want %#v", loaded, settings)
	}
}

func TestLoadDownloadersPostgresUpgradesLegacyPlaintext(t *testing.T) {
	baseStore := newSiteStatisticsIntegrationStore(t)
	store := NewStore(baseStore.db, newDownloadersSettingTestBox(t))
	legacyValue := json.RawMessage(`[{"name":" qb ","type":"QBITTORRENT","default":false,"enabled":true,"config":{"host":" https://qb.example/ ","username":"admin","password":"password-secret","apikey":"apikey-secret"}}]`)
	if _, err := store.db.Pool.Exec(t.Context(), `
		INSERT INTO settings (key, value, encrypted)
		VALUES ($1, $2::jsonb, FALSE)`, DownloadersSettingKey, legacyValue); err != nil {
		t.Fatalf("写入旧明文下载器设置: %v", err)
	}

	settings, found, err := store.LoadDownloaders(t.Context())
	if err != nil {
		t.Fatalf("LoadDownloaders() error = %v", err)
	}
	if !found || len(settings) != 1 {
		t.Fatalf("LoadDownloaders() found=%v len=%d", found, len(settings))
	}
	runtime, err := settings[0].Runtime()
	if err != nil {
		t.Fatalf("Downloader.Runtime() error = %v", err)
	}
	if runtime.ID == "" || runtime.Name != "qb" || runtime.Type != "qbittorrent" || runtime.Host != "https://qb.example/" || !runtime.Enabled || !runtime.Default {
		t.Fatalf("运行时投影字段错误: id=%q name=%q type=%q host=%q enabled=%v default=%v",
			runtime.ID, runtime.Name, runtime.Type, runtime.Host, runtime.Enabled, runtime.Default)
	}
	credentials := runtime.Authentication()
	if credentials.Method() != downloadconfig.AuthAPIKey || credentials.APIKey() != "apikey-secret" {
		t.Fatalf("运行时认证投影错误: method=%q", credentials.Method())
	}

	var storedValue []byte
	var encrypted bool
	if err := store.db.Pool.QueryRow(t.Context(), `
		SELECT value, encrypted
		FROM settings
		WHERE key=$1`, DownloadersSettingKey).Scan(&storedValue, &encrypted); err != nil {
		t.Fatalf("读取升级后的数据库值: %v", err)
	}
	if !encrypted {
		t.Fatal("旧明文记录未升级为 encrypted=true")
	}
	for _, secret := range []string{"password-secret", "apikey-secret"} {
		if bytes.Contains(storedValue, []byte(secret)) {
			t.Fatalf("升级后的数据库值泄漏秘密 %q: %s", secret, storedValue)
		}
	}
}

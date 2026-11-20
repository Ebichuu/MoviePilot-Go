//go:build integration

package database

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
)

func TestLoadNotificationSettingsPostgresUpgradesLegacyPlaintext(t *testing.T) {
	baseStore := newSiteStatisticsIntegrationStore(t)
	store := NewStore(baseStore.db, newNotificationSettingsTestBox(t))
	legacyValue := json.RawMessage(`{"channel":"legacy","token":"legacy-token-secret"}`)

	if _, err := store.db.Pool.Exec(t.Context(), `
		INSERT INTO settings (key, value, encrypted)
		VALUES ($1, $2::jsonb, FALSE)`, NotificationSettingsKey, legacyValue); err != nil {
		t.Fatalf("写入旧明文通知设置: %v", err)
	}

	loaded, found, err := store.LoadNotificationSettings(t.Context())
	if err != nil || !found {
		t.Fatalf("读取旧明文通知设置: found=%v err=%v", found, err)
	}
	assertNotificationSettingsJSONEqual(t, loaded, legacyValue)

	var storedValue []byte
	var encrypted bool
	if err := store.db.Pool.QueryRow(t.Context(), `
		SELECT value, encrypted
		FROM settings
		WHERE key=$1`, NotificationSettingsKey).Scan(&storedValue, &encrypted); err != nil {
		t.Fatalf("读取升级后的通知设置: %v", err)
	}
	if !encrypted {
		t.Fatal("旧明文通知设置未升级为密文")
	}
	if bytes.Contains(storedValue, []byte("legacy-token-secret")) {
		t.Fatal("升级后的数据库值泄漏通知配置明文")
	}
}

func TestNotificationSettingsLegacyUpgradeCASPreservesConcurrentSave(t *testing.T) {
	baseStore := newSiteStatisticsIntegrationStore(t)
	store := NewStore(baseStore.db, newNotificationSettingsTestBox(t))
	legacyValue := json.RawMessage(`{"channel":"legacy","token":"legacy-token-secret"}`)
	concurrentValue := json.RawMessage(`{"channel":"concurrent","token":"concurrent-token-secret"}`)

	if _, err := store.db.Pool.Exec(t.Context(), `
		INSERT INTO settings (key, value, encrypted)
		VALUES ($1, $2::jsonb, FALSE)`, NotificationSettingsKey, legacyValue); err != nil {
		t.Fatalf("写入旧明文通知设置: %v", err)
	}

	loadedLegacy, encrypted, found, err := store.loadNotificationSetting(t.Context())
	if err != nil || !found || encrypted {
		t.Fatalf("读取旧明文通知设置结果异常: found=%v encrypted=%v err=%v", found, encrypted, err)
	}
	assertNotificationSettingsJSONEqual(t, loadedLegacy, legacyValue)
	if err := store.SaveNotificationSettings(t.Context(), concurrentValue); err != nil {
		t.Fatalf("并发保存新通知设置: %v", err)
	}

	upgraded, err := store.upgradeLegacyNotificationSetting(t.Context(), loadedLegacy)
	if err != nil {
		t.Fatalf("尝试升级已过期旧明文: %v", err)
	}
	if upgraded {
		t.Fatal("已过期旧明文升级命中，覆盖了并发保存")
	}

	loaded, found, err := store.LoadNotificationSettings(t.Context())
	if err != nil || !found {
		t.Fatalf("读取并发保存后的通知设置: found=%v err=%v", found, err)
	}
	assertNotificationSettingsJSONEqual(t, loaded, concurrentValue)

	var storedValue []byte
	var storedEncrypted bool
	if err := store.db.Pool.QueryRow(t.Context(), `
		SELECT value, encrypted
		FROM settings
		WHERE key=$1`, NotificationSettingsKey).Scan(&storedValue, &storedEncrypted); err != nil {
		t.Fatalf("读取最终通知设置: %v", err)
	}
	if !storedEncrypted {
		t.Fatal("并发保存后的通知设置不是密文")
	}
	for _, secret := range [][]byte{[]byte("legacy-token-secret"), []byte("concurrent-token-secret")} {
		if bytes.Contains(storedValue, secret) {
			t.Fatal("数据库最终值泄漏通知配置明文")
		}
	}
}

func assertNotificationSettingsJSONEqual(t *testing.T, got, want json.RawMessage) {
	t.Helper()

	var gotValue any
	if err := json.Unmarshal(got, &gotValue); err != nil {
		t.Fatalf("解析实际通知设置: %v", err)
	}
	var wantValue any
	if err := json.Unmarshal(want, &wantValue); err != nil {
		t.Fatalf("解析预期通知设置: %v", err)
	}
	if !reflect.DeepEqual(gotValue, wantValue) {
		t.Fatal("通知设置 JSON 不匹配")
	}
}

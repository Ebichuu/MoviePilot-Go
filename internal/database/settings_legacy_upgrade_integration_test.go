//go:build integration

package database

import (
	"context"
	"testing"
)

// 本文件用真实 Postgres 覆盖「旧明文设置升级」在缺少加密器时的失败返回，
// 以及媒体服务器设置里已存值不是数组时的解码拒绝。

func writeLegacySettingRaw(t *testing.T, store *Store, key, value string) {
	t.Helper()
	if _, err := store.db.Pool.Exec(t.Context(), `
		INSERT INTO settings (key, value, encrypted)
		VALUES ($1, $2::jsonb, FALSE)
		ON CONFLICT (key) DO UPDATE
		SET value=EXCLUDED.value, encrypted=FALSE`, key, value); err != nil {
		t.Fatalf("写入旧明文设置 %s: %v", key, err)
	}
}

func TestLegacySettingsUpgradeWithoutSecretBox(t *testing.T) {
	baseStore := newSiteStatisticsIntegrationStore(t)
	store := NewStore(baseStore.db, nil)
	ctx := t.Context()
	t.Cleanup(func() {
		for _, key := range []string{MediaServersSettingKey, NotificationSettingsKey} {
			_, _ = store.db.Pool.Exec(context.Background(), `DELETE FROM settings WHERE key=$1`, key)
		}
	})

	// 已存值不是数组：媒体服务器设置解码直接拒绝。
	writeLegacySettingRaw(t, store, MediaServersSettingKey, `"not-an-array"`)
	if _, _, err := store.LoadMediaServers(ctx); err == nil {
		t.Fatal("非数组媒体服务器设置应报错")
	}

	// 合法明文记录需要升级为密文，但缺少加密器会失败。
	writeLegacySettingRaw(t, store, MediaServersSettingKey, `[{"name":"legacy-emby","enabled":false}]`)
	if _, _, err := store.LoadMediaServers(ctx); err == nil {
		t.Fatal("缺少加密器时升级媒体服务器设置应报错")
	}

	writeLegacySettingRaw(t, store, NotificationSettingsKey, `{"channel":"legacy"}`)
	if _, _, err := store.LoadNotificationSettings(ctx); err == nil {
		t.Fatal("缺少加密器时升级通知设置应报错")
	}
}

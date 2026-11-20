//go:build integration

package database

import (
	"context"
	"strings"
	"testing"

	"github.com/moviepilot-go/moviepilot-go/internal/auth"
)

// 本文件用真实 Postgres 覆盖下载器设置读取时对「已存值」的三种拒绝：
// 非数组、重名、以及敏感字段类型不合法。

func newDownloadersSettingBranchStore(t *testing.T) *Store {
	t.Helper()
	base := newSiteStatisticsIntegrationStore(t)
	secretBox, err := auth.NewSecretBox("downloaders-setting-branch-key")
	if err != nil {
		t.Fatalf("创建下载器设置测试 SecretBox: %v", err)
	}
	return NewStore(base.db, secretBox)
}

func writeDownloadersSettingRaw(t *testing.T, store *Store, value string) {
	t.Helper()
	if _, err := store.db.Pool.Exec(t.Context(), `
		INSERT INTO settings (key, value, encrypted, updated_at)
		VALUES ($1, $2::jsonb, FALSE, now())
		ON CONFLICT (key) DO UPDATE
		SET value=EXCLUDED.value, encrypted=FALSE, updated_at=now()`,
		DownloadersSettingKey, value); err != nil {
		t.Fatalf("写入下载器设置 %s: %v", value, err)
	}
}

func TestLoadDownloadersRejectsStoredValueBranches(t *testing.T) {
	store := newDownloadersSettingBranchStore(t)
	ctx := t.Context()
	t.Cleanup(func() {
		_, _ = store.db.Pool.Exec(context.Background(), `DELETE FROM settings WHERE key=$1`, DownloadersSettingKey)
	})

	writeDownloadersSettingRaw(t, store, `123`)
	if _, _, err := store.LoadDownloaders(ctx); err == nil || !strings.Contains(err.Error(), "数组") {
		t.Fatalf("非数组设置 error = %v", err)
	}

	writeDownloadersSettingRaw(t, store, `[{"name":"dup"},{"name":"dup"}]`)
	if _, _, err := store.LoadDownloaders(ctx); err == nil {
		t.Fatal("重名下载器应报错")
	}

	writeDownloadersSettingRaw(t, store, `[{"name":"qb","type":"qbittorrent","config":{"password":123}}]`)
	if _, _, err := store.LoadDownloaders(ctx); err == nil {
		t.Fatal("非字符串密码应报错")
	}

}

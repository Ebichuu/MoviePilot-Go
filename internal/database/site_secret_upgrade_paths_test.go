package database

import (
	"context"
	"testing"

	"github.com/moviepilot-go/moviepilot-go/internal/auth"
)

// 本文件覆盖站点凭据升级链路的闸门与已存值拒绝：
// 未初始化/缺加密器/事务无法开启，以及单行升级时公开配置或
// 已存密文不可用的三种失败。

func TestUpgradeLegacySiteSecretsGuards(t *testing.T) {
	ctx := context.Background()
	if err := (*Store)(nil).UpgradeLegacySiteSecrets(ctx); err == nil {
		t.Fatal("未初始化 store 应报错")
	}

	base := settingsGuardStore(t)
	if err := (&Store{db: base.db}).UpgradeLegacySiteSecrets(ctx); err == nil {
		t.Fatal("缺少加密器应报错")
	}
	// 加密器齐备但连接不可用：事务无法开启。
	secretBox, err := auth.NewSecretBox("site-secret-upgrade-guard-key")
	if err != nil {
		t.Fatalf("创建 SecretBox: %v", err)
	}
	if err := NewStore(base.db, secretBox).UpgradeLegacySiteSecrets(ctx); err == nil {
		t.Fatal("连接不可用应报错")
	}
}

func TestUpgradeLegacySiteCredentialRowRejectsStoredValues(t *testing.T) {
	secretBox, err := auth.NewSecretBox("site-secret-upgrade-master-key")
	if err != nil {
		t.Fatalf("创建 SecretBox: %v", err)
	}
	store := &Store{secretBox: secretBox}
	ctx := context.Background()

	if err := store.upgradeLegacySiteCredentialRow(ctx, nil, legacySiteCredentialRow{
		id: 1, rawConfig: []byte("{"),
	}); err == nil {
		t.Fatal("非法公开配置应报错")
	}

	if err := store.upgradeLegacySiteCredentialRow(ctx, nil, legacySiteCredentialRow{
		id: 2, rawConfig: []byte(`{"apikey":"live"}`), encryptedSecrets: "not-a-ciphertext",
	}); err == nil {
		t.Fatal("不可解密的已存凭据应报错")
	}

	encrypted, err := secretBox.Encrypt("not-json")
	if err != nil {
		t.Fatalf("加密测试载荷: %v", err)
	}
	if err := store.upgradeLegacySiteCredentialRow(ctx, nil, legacySiteCredentialRow{
		id: 3, rawConfig: []byte(`{"apikey":"live"}`), encryptedSecrets: encrypted,
	}); err == nil {
		t.Fatal("非 JSON 已存凭据应报错")
	}
}

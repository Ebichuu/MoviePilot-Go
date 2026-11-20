//go:build integration

package database

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/moviepilot-go/moviepilot-go/internal/auth"
)

const siteSecretUpgradeTestMasterKey = "site-secret-upgrade-integration-master-key"

func TestUpgradeLegacySiteSecretsPostgresRoundTripAndIdempotence(t *testing.T) {
	baseStore := newSiteStatisticsIntegrationStore(t)
	secretBox := newSiteSecretUpgradeTestBox(t, siteSecretUpgradeTestMasterKey)
	store := NewStore(baseStore.db, secretBox)

	existingEncrypted := encryptSiteSecretUpgradeCredentials(t, secretBox, map[string]any{
		"cookie": "current-cookie",
		"apikey": "current-api-key",
		"custom": "current-custom-secret",
	})
	existingID := insertLegacySiteSecretUpgradeFixture(t, store, "legacy-existing", map[string]any{
		"format":  "rss",
		"cookie":  "legacy-cookie",
		"api_key": "legacy-api-key",
		"token":   "legacy-token",
	}, existingEncrypted)
	legacyOnlyID := insertLegacySiteSecretUpgradeFixture(t, store, "legacy-only", map[string]any{
		"format":        "json",
		"cookie":        "legacy-only-cookie",
		"api_key":       "legacy-only-api-key",
		"passkey":       "legacy-only-passkey",
		"authorization": "legacy-only-authorization",
	}, "")

	if err := store.UpgradeLegacySiteSecrets(t.Context()); err != nil {
		t.Fatalf("首次升级历史站点凭据: %v", err)
	}

	existingPublic, existingCiphertext := readSiteSecretUpgradeState(t, store, existingID)
	assertSiteSecretUpgradePublicConfig(t, existingPublic, "rss")
	existingCredentials := decryptSiteSecretUpgradeCredentials(t, secretBox, existingCiphertext)
	assertSiteSecretUpgradeCredential(t, existingCredentials, "cookie", "current-cookie")
	assertSiteSecretUpgradeCredential(t, existingCredentials, "apikey", "current-api-key")
	assertSiteSecretUpgradeCredential(t, existingCredentials, "token", "legacy-token")
	assertSiteSecretUpgradeCredential(t, existingCredentials, "custom", "current-custom-secret")
	if _, exists := existingCredentials["api_key"]; exists {
		t.Fatal("历史 API Key 别名覆盖了已有凭据")
	}

	legacyOnlyPublic, legacyOnlyCiphertext := readSiteSecretUpgradeState(t, store, legacyOnlyID)
	assertSiteSecretUpgradePublicConfig(t, legacyOnlyPublic, "json")
	legacyOnlyCredentials := decryptSiteSecretUpgradeCredentials(t, secretBox, legacyOnlyCiphertext)
	assertSiteSecretUpgradeCredential(t, legacyOnlyCredentials, "cookie", "legacy-only-cookie")
	assertSiteSecretUpgradeCredential(t, legacyOnlyCredentials, "api_key", "legacy-only-api-key")
	assertSiteSecretUpgradeCredential(t, legacyOnlyCredentials, "passkey", "legacy-only-passkey")
	assertSiteSecretUpgradeCredential(t, legacyOnlyCredentials, "authorization", "legacy-only-authorization")

	if err := store.UpgradeLegacySiteSecrets(t.Context()); err != nil {
		t.Fatalf("重复升级历史站点凭据: %v", err)
	}
	_, existingCiphertextAfterRetry := readSiteSecretUpgradeState(t, store, existingID)
	_, legacyOnlyCiphertextAfterRetry := readSiteSecretUpgradeState(t, store, legacyOnlyID)
	if existingCiphertextAfterRetry != existingCiphertext || legacyOnlyCiphertextAfterRetry != legacyOnlyCiphertext {
		t.Fatal("重复执行升级改变了已迁移密文")
	}

	restartedStore := NewStore(baseStore.db, newSiteSecretUpgradeTestBox(t, siteSecretUpgradeTestMasterKey))
	sites, err := restartedStore.ListEnabledSites(t.Context())
	if err != nil {
		t.Fatalf("使用同一主密钥重启后加载站点: %v", err)
	}
	restartedExisting := findSiteSecretUpgradeFixture(t, sites, existingID)
	assertSiteSecretUpgradeCredential(t, restartedExisting.Config, "cookie", "current-cookie")
	assertSiteSecretUpgradeCredential(t, restartedExisting.Config, "token", "legacy-token")
	restartedLegacyOnly := findSiteSecretUpgradeFixture(t, sites, legacyOnlyID)
	assertSiteSecretUpgradeCredential(t, restartedLegacyOnly.Config, "cookie", "legacy-only-cookie")
	assertSiteSecretUpgradeCredential(t, restartedLegacyOnly.Config, "api_key", "legacy-only-api-key")
}

func TestUpgradeLegacySiteSecretsPostgresRollsBackAllRows(t *testing.T) {
	baseStore := newSiteStatisticsIntegrationStore(t)
	secretBox := newSiteSecretUpgradeTestBox(t, siteSecretUpgradeTestMasterKey)
	store := NewStore(baseStore.db, secretBox)

	firstID := insertLegacySiteSecretUpgradeFixture(t, store, "rollback-first", map[string]any{
		"format": "rss",
		"cookie": "rollback-first-cookie",
	}, "")
	secondID := insertLegacySiteSecretUpgradeFixture(t, store, "rollback-second", map[string]any{
		"format": "rss",
		"cookie": "rollback-second-cookie",
	}, "")

	if _, err := store.db.Pool.Exec(t.Context(), `
		CREATE FUNCTION fail_site_secret_upgrade_fixture() RETURNS trigger
		LANGUAGE plpgsql AS $body$
		BEGIN
			IF NEW.name = 'rollback-second' THEN
				RAISE EXCEPTION 'forced site credential upgrade failure';
			END IF;
			RETURN NEW;
		END
		$body$`); err != nil {
		t.Fatalf("创建回滚测试函数: %v", err)
	}
	if _, err := store.db.Pool.Exec(t.Context(), `
		CREATE TRIGGER fail_site_secret_upgrade_fixture
		BEFORE UPDATE ON sites
		FOR EACH ROW EXECUTE FUNCTION fail_site_secret_upgrade_fixture()`); err != nil {
		t.Fatalf("创建回滚测试触发器: %v", err)
	}

	if err := store.UpgradeLegacySiteSecrets(t.Context()); err == nil {
		t.Fatal("触发器拒绝第二行时升级意外成功")
	}
	assertSiteSecretUpgradeRolledBack(t, store, firstID)
	assertSiteSecretUpgradeRolledBack(t, store, secondID)

	if _, err := store.db.Pool.Exec(t.Context(), `DROP TRIGGER fail_site_secret_upgrade_fixture ON sites`); err != nil {
		t.Fatalf("移除回滚测试触发器: %v", err)
	}
	if _, err := store.db.Pool.Exec(t.Context(), `DROP FUNCTION fail_site_secret_upgrade_fixture()`); err != nil {
		t.Fatalf("移除回滚测试函数: %v", err)
	}
	if err := store.UpgradeLegacySiteSecrets(t.Context()); err != nil {
		t.Fatalf("移除故障后重新升级: %v", err)
	}
	firstPublic, firstCiphertext := readSiteSecretUpgradeState(t, store, firstID)
	secondPublic, secondCiphertext := readSiteSecretUpgradeState(t, store, secondID)
	assertSiteSecretUpgradePublicConfig(t, firstPublic, "rss")
	assertSiteSecretUpgradePublicConfig(t, secondPublic, "rss")
	if firstCiphertext == "" || secondCiphertext == "" {
		t.Fatal("故障恢复后凭据未写入加密列")
	}
}

func TestUpgradeLegacySiteSecretsPostgresDoesNotExposeCredentialsOnFailure(t *testing.T) {
	baseStore := newSiteStatisticsIntegrationStore(t)
	store := NewStore(baseStore.db, newSiteSecretUpgradeTestBox(t, siteSecretUpgradeTestMasterKey))

	const (
		plainCredential   = "plaintext-must-not-appear"
		invalidCiphertext = "ciphertext-must-not-appear"
	)
	siteID := insertLegacySiteSecretUpgradeFixture(t, store, "no-error-leak", map[string]any{
		"format": "rss",
		"cookie": plainCredential,
	}, invalidCiphertext)

	err := store.UpgradeLegacySiteSecrets(t.Context())
	if err == nil {
		t.Fatal("损坏的已有密文未阻止升级")
	}
	for _, secret := range []string{plainCredential, invalidCiphertext} {
		if strings.Contains(err.Error(), secret) {
			t.Fatal("升级错误泄露了站点凭据或密文")
		}
	}
	config, encryptedSecrets := readSiteSecretUpgradeState(t, store, siteID)
	if encryptedSecrets != invalidCiphertext {
		t.Fatal("解密失败后已有密文被修改")
	}
	if config["cookie"] != plainCredential {
		t.Fatal("解密失败后历史明文未随事务保留")
	}
}

func newSiteSecretUpgradeTestBox(t *testing.T, masterKey string) *auth.SecretBox {
	t.Helper()
	secretBox, err := auth.NewSecretBox(masterKey)
	if err != nil {
		t.Fatalf("创建测试加密器: %v", err)
	}
	return secretBox
}

func encryptSiteSecretUpgradeCredentials(t *testing.T, secretBox *auth.SecretBox, credentials map[string]any) string {
	t.Helper()
	encoded, err := json.Marshal(credentials)
	if err != nil {
		t.Fatal("编码测试凭据失败")
	}
	encrypted, err := secretBox.Encrypt(string(encoded))
	if err != nil {
		t.Fatalf("加密测试凭据: %v", err)
	}
	return encrypted
}

func decryptSiteSecretUpgradeCredentials(t *testing.T, secretBox *auth.SecretBox, encrypted string) map[string]any {
	t.Helper()
	if encrypted == "" {
		t.Fatal("加密列为空")
	}
	plainText, err := secretBox.Decrypt(encrypted)
	if err != nil {
		t.Fatalf("解密测试凭据: %v", err)
	}
	var credentials map[string]any
	if err := json.Unmarshal([]byte(plainText), &credentials); err != nil {
		t.Fatal("解析测试凭据失败")
	}
	return credentials
}

func insertLegacySiteSecretUpgradeFixture(
	t *testing.T,
	store *Store,
	name string,
	config map[string]any,
	encryptedSecrets string,
) int {
	t.Helper()
	encodedConfig, err := json.Marshal(config)
	if err != nil {
		t.Fatal("编码历史站点测试配置失败")
	}
	var id int
	if err := store.db.Pool.QueryRow(t.Context(), `
		INSERT INTO sites (name, domain, url, adapter, enabled, config, secrets)
		VALUES ($1, $2, $3, 'declarative', TRUE, $4::jsonb, $5)
		RETURNING id`, name, name+".example", "https://"+name+".example/", encodedConfig, encryptedSecrets).Scan(&id); err != nil {
		t.Fatalf("插入历史站点测试数据: %v", err)
	}
	return id
}

func readSiteSecretUpgradeState(t *testing.T, store *Store, id int) (map[string]any, string) {
	t.Helper()
	var rawConfig []byte
	var encryptedSecrets string
	if err := store.db.Pool.QueryRow(t.Context(), `
		SELECT config, secrets FROM sites WHERE id=$1`, id).Scan(&rawConfig, &encryptedSecrets); err != nil {
		t.Fatalf("读取站点升级状态: %v", err)
	}
	var config map[string]any
	if err := json.Unmarshal(rawConfig, &config); err != nil {
		t.Fatal("解析站点公开配置失败")
	}
	return config, encryptedSecrets
}

func assertSiteSecretUpgradePublicConfig(t *testing.T, config map[string]any, wantFormat string) {
	t.Helper()
	if config["format"] != wantFormat {
		t.Fatal("站点公开配置未保留")
	}
	for key := range config {
		if sensitiveSiteKey(key) {
			t.Fatalf("站点公开配置仍包含敏感字段 %q", key)
		}
	}
}

func assertSiteSecretUpgradeCredential(t *testing.T, credentials map[string]any, key, want string) {
	t.Helper()
	value, ok := credentials[key].(string)
	if !ok || value != want {
		t.Fatalf("站点凭据字段 %q 不符合预期", key)
	}
}

func assertSiteSecretUpgradeRolledBack(t *testing.T, store *Store, id int) {
	t.Helper()
	config, encryptedSecrets := readSiteSecretUpgradeState(t, store, id)
	if encryptedSecrets != "" {
		t.Fatal("失败事务留下了加密列更新")
	}
	if _, exists := config["cookie"]; !exists {
		t.Fatal("失败事务移除了历史公开配置中的凭据")
	}
}

func findSiteSecretUpgradeFixture(t *testing.T, sites []Site, id int) Site {
	t.Helper()
	for _, site := range sites {
		if site.ID == id {
			return site
		}
	}
	t.Fatalf("重启后未找到站点 id=%d", id)
	return Site{}
}

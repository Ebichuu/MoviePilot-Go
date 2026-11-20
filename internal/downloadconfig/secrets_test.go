package downloadconfig

import (
	"encoding/json"
	"testing"
)

func TestMergeSecretsKeepsStoredSecretWhenBlank(t *testing.T) {
	existing := []Downloader{{
		ID: "1", Name: "qb", hadID: true,
		Config: map[string]any{"password": "old-pass", "apikey": "old-key"},
	}}
	incoming := []Downloader{{
		ID: "1", Name: "qb", hadID: true,
		Config: map[string]any{"password": "", "apikey": ""},
	}}
	if err := MergeSecrets(incoming, existing); err != nil {
		t.Fatalf("合并失败: %v", err)
	}
	// 前端不回传密码时必须保留已存凭据，否则用户每次改配置都要重输密码。
	if incoming[0].Config["password"] != "old-pass" {
		t.Fatalf("password = %v", incoming[0].Config["password"])
	}
	if incoming[0].Config["apikey"] != "old-key" {
		t.Fatalf("apikey = %v", incoming[0].Config["apikey"])
	}
	// 未提交的字段同样继承已存值（不是写空串）。
	absent := []Downloader{{ID: "1", Name: "qb", hadID: true, Config: map[string]any{"host": "http://qb"}}}
	if err := MergeSecrets(absent, existing); err != nil {
		t.Fatalf("合并失败: %v", err)
	}
	if absent[0].Config["password"] != "old-pass" {
		t.Fatalf("缺失字段应继承旧值，实际 %v", absent[0].Config["password"])
	}
}

func TestMergeSecretsAppliesUpdateSemantics(t *testing.T) {
	existing := []Downloader{{ID: "1", Name: "qb", hadID: true, Config: map[string]any{"password": "old"}}}

	// 非空值直接替换。
	replaced := []Downloader{{ID: "1", Name: "qb", hadID: true, Config: map[string]any{"password": "new"}}}
	if err := MergeSecrets(replaced, existing); err != nil {
		t.Fatalf("合并失败: %v", err)
	}
	if replaced[0].Config["password"] != "new" {
		t.Fatalf("password = %v", replaced[0].Config["password"])
	}

	// 显式 clear_* 优先于「空值保留」。
	cleared := []Downloader{{ID: "1", Name: "qb", hadID: true, Config: map[string]any{
		"password": "", "clear_password": true,
	}}}
	if err := MergeSecrets(cleared, existing); err != nil {
		t.Fatalf("合并失败: %v", err)
	}
	if cleared[0].Config["password"] != "" {
		t.Fatalf("clear_password 应清空，实际 %v", cleared[0].Config["password"])
	}
	// 即使同时提交了值，显式清除仍然生效（用户意图是删除）。
	clearedWithValue := []Downloader{{ID: "1", Name: "qb", hadID: true, Config: map[string]any{
		"password": "ignored", "clear_password": true,
	}}}
	if err := MergeSecrets(clearedWithValue, existing); err != nil {
		t.Fatalf("合并失败: %v", err)
	}
	if clearedWithValue[0].Config["password"] != "" {
		t.Fatalf("clear_password 应优先，实际 %v", clearedWithValue[0].Config["password"])
	}

	// clear_* 为假时按普通空值处理：保留旧值。
	notCleared := []Downloader{{ID: "1", Name: "qb", hadID: true, Config: map[string]any{
		"password": "", "clear_password": false,
	}}}
	if err := MergeSecrets(notCleared, existing); err != nil {
		t.Fatalf("合并失败: %v", err)
	}
	if notCleared[0].Config["password"] != "old" {
		t.Fatalf("password = %v", notCleared[0].Config["password"])
	}

	// 元数据字段必须被消费掉，不能原样落库。
	for _, key := range []string{"has_password", "clear_password", "has_apikey", "clear_apikey"} {
		if _, ok := notCleared[0].Config[key]; ok {
			t.Fatalf("%s 不应残留", key)
		}
	}
}

func TestMergeSecretsMatchesLegacyEntryByName(t *testing.T) {
	// 历史配置没有 ID（hadID=false）：新提交带 ID 时必须回落到按名字匹配，
	// 否则老用户升级后第一次保存就会丢密码。
	legacy := []Downloader{{Name: "QB", Config: map[string]any{"password": "legacy-pass"}}}
	incoming := []Downloader{{ID: "new-id", Name: "qb", hadID: true, Config: map[string]any{"password": ""}}}
	if err := MergeSecrets(incoming, legacy); err != nil {
		t.Fatalf("合并失败: %v", err)
	}
	if incoming[0].Config["password"] != "legacy-pass" {
		t.Fatalf("password = %v", incoming[0].Config["password"])
	}
}

func TestMergeSecretsAdoptsExistingIdentity(t *testing.T) {
	existing := []Downloader{{ID: "42", Name: "qb", hadID: true, Config: map[string]any{"password": "old"}}}
	// 新提交没有 ID，但有同名配置：必须沿用已有 ID，避免插入重复下载器。
	incoming := []Downloader{{Name: "QB", Config: map[string]any{"password": ""}}}
	if err := MergeSecrets(incoming, existing); err != nil {
		t.Fatalf("合并失败: %v", err)
	}
	if incoming[0].ID != "42" {
		t.Fatalf("ID = %q，期望沿用 42", incoming[0].ID)
	}
	if !incoming[0].hadID {
		t.Fatal("沿用 ID 后应标记 hadID")
	}
	if incoming[0].Config["password"] != "old" {
		t.Fatalf("password = %v", incoming[0].Config["password"])
	}
}

func TestMergeSecretsWithoutPreviousEntry(t *testing.T) {
	incoming := []Downloader{{Name: "tr", Config: map[string]any{"password": ""}}}
	if err := MergeSecrets(incoming, nil); err != nil {
		t.Fatalf("合并失败: %v", err)
	}
	// 没有历史记录时就是空串，不报错也不编造凭据。
	if incoming[0].Config["password"] != "" {
		t.Fatalf("password = %v", incoming[0].Config["password"])
	}
}

func TestMergeSecretsRejectsWrongTypes(t *testing.T) {
	existing := []Downloader{{ID: "1", Name: "qb", hadID: true, Config: map[string]any{"password": "old"}}}
	badPassword := []Downloader{{ID: "1", Name: "qb", hadID: true, Config: map[string]any{"password": 123}}}
	if err := MergeSecrets(badPassword, existing); err == nil {
		t.Fatal("非字符串密码应报错")
	}
	badClear := []Downloader{{ID: "1", Name: "qb", hadID: true, Config: map[string]any{"password": "", "clear_password": "yes"}}}
	if err := MergeSecrets(badClear, existing); err == nil {
		t.Fatal("非布尔 clear_password 应报错")
	}
}

func TestSanitizeStoredSecrets(t *testing.T) {
	settings := []Downloader{
		{ID: "1", Config: map[string]any{
			"password": nil, "has_password": true, "clear_apikey": false, "apikey": "key",
		}},
		{ID: "2", Config: map[string]any{"password": "pass"}},
	}
	changed, err := SanitizeStoredSecrets(settings)
	if err != nil {
		t.Fatalf("清理失败: %v", err)
	}
	if !changed {
		t.Fatal("有改动时应报告 changed")
	}
	// nil 密码必须落成空串，便于统一加密/校验路径。
	if settings[0].Config["password"] != "" {
		t.Fatalf("password = %v", settings[0].Config["password"])
	}
	for _, key := range []string{"has_password", "clear_apikey"} {
		if _, ok := settings[0].Config[key]; ok {
			t.Fatalf("%s 应被删除", key)
		}
	}
	// 真实密码保持不动。
	if settings[1].Config["password"] != "pass" {
		t.Fatalf("password = %v", settings[1].Config["password"])
	}

	// 已经干净的配置不需要再标记改动。
	clean := []Downloader{{ID: "3", Config: map[string]any{"password": "pass", "apikey": "key"}}}
	if changed, err := SanitizeStoredSecrets(clean); err != nil || changed {
		t.Fatalf("干净配置 = %v %v", changed, err)
	}

	bad := []Downloader{{ID: "4", Config: map[string]any{"password": 1}}}
	if _, err := SanitizeStoredSecrets(bad); err == nil {
		t.Fatal("非字符串密码应报错")
	}
}

func TestRedactSecretsBlanksAndFlags(t *testing.T) {
	original := []Downloader{{
		ID: "1", Name: "qb",
		Config:      map[string]any{"host": "http://qb", "password": "secret", "apikey": "", "clear_password": true},
		PathMapping: json.RawMessage(`[{"from":"/a","to":"/b"}]`),
	}}
	redacted, err := RedactSecrets(original)
	if err != nil {
		t.Fatalf("脱敏失败: %v", err)
	}
	// 响应里绝不能出现密码明文。
	for _, field := range []string{"password", "apikey"} {
		if value, _ := redacted[0].Config[field].(string); value != "" {
			t.Fatalf("%s 未脱敏: %q", field, value)
		}
	}
	// has_* 告诉前端「有这个凭据」，clear_* 不回显。
	if redacted[0].Config["has_password"] != true {
		t.Fatalf("has_password = %v", redacted[0].Config["has_password"])
	}
	if redacted[0].Config["has_apikey"] != false {
		t.Fatalf("has_apikey = %v", redacted[0].Config["has_apikey"])
	}
	if _, ok := redacted[0].Config["clear_password"]; ok {
		t.Fatal("clear_password 不应回显")
	}
	if redacted[0].Config["host"] != "http://qb" {
		t.Fatalf("非秘密字段应保留: %v", redacted[0].Config["host"])
	}

	// 必须是副本：改脱敏结果不能影响库里的真实配置。
	if original[0].Config["password"] != "secret" {
		t.Fatalf("原始配置被改写: %v", original[0].Config["password"])
	}
	redacted[0].Config["host"] = "mutated"
	if original[0].Config["host"] != "http://qb" {
		t.Fatal("Config 必须是副本")
	}
	redacted[0].PathMapping[0] = 'X'
	if original[0].PathMapping[0] != '[' {
		t.Fatal("PathMapping 必须是副本")
	}
}

func TestRedactSecretsHandlesMissingConfigAndBadTypes(t *testing.T) {
	redacted, err := RedactSecrets([]Downloader{{ID: "1"}})
	if err != nil {
		t.Fatalf("脱敏失败: %v", err)
	}
	// 没有 config 时也要给出 has_* 标记，前端才能画对表单。
	if redacted[0].Config["has_password"] != false || redacted[0].Config["has_apikey"] != false {
		t.Fatalf("Config = %v", redacted[0].Config)
	}
	if _, err := RedactSecrets([]Downloader{{ID: "1", Config: map[string]any{"apikey": 5}}}); err == nil {
		t.Fatal("非字符串 apikey 应报错")
	}
}

func TestMergeSecretsIsStableAcrossRepeatedSaves(t *testing.T) {
	// 连续两次保存（前端每次都不回传密码）必须保持同一凭据，不能逐渐变空。
	existing := []Downloader{{ID: "1", Name: "qb", hadID: true, Config: map[string]any{"password": "pass-1"}}}
	for round := 0; round < 3; round++ {
		incoming := []Downloader{{ID: "1", Name: "qb", hadID: true, Config: map[string]any{"password": ""}}}
		if err := MergeSecrets(incoming, existing); err != nil {
			t.Fatalf("第 %d 次合并失败: %v", round, err)
		}
		if incoming[0].Config["password"] != "pass-1" {
			t.Fatalf("第 %d 次保存后 password = %v", round, incoming[0].Config["password"])
		}
		existing = incoming
	}
	if password, ok := existing[0].Config["password"].(string); !ok || password == "" {
		t.Fatal("凭据在重复保存中丢失")
	}
}

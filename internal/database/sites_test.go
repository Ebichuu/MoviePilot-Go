package database

import "testing"

func TestSplitSiteConfigSeparatesCredentials(t *testing.T) {
	publicConfig, secrets := splitSiteConfig(map[string]any{
		"search_path": "/api/search",
		"cookie":      "session=fixture",
		"apikey":      "fixture-key",
		"headers":     map[string]any{"Authorization": "Bearer fixture"},
	})
	if publicConfig["search_path"] != "/api/search" || len(publicConfig) != 1 {
		t.Fatalf("站点公开配置拆分错误: %#v", publicConfig)
	}
	if secrets["cookie"] == nil || secrets["apikey"] == nil || secrets["headers"] == nil {
		t.Fatalf("站点敏感配置拆分错误: %#v", secrets)
	}
	if containsSensitiveSiteConfig(publicConfig) || !containsSensitiveSiteConfig(secrets) {
		t.Fatal("站点敏感配置检测错误")
	}
}

func TestSplitSiteConfigDropsExplicitlyClearedCredentials(t *testing.T) {
	_, secrets := splitSiteConfig(map[string]any{
		"cookie":  "",
		"apikey":  nil,
		"token":   "active-token",
		"passkey": " ",
	})
	if _, exists := secrets["cookie"]; exists {
		t.Fatalf("empty cookie should be removed: %#v", secrets)
	}
	if _, exists := secrets["apikey"]; exists {
		t.Fatalf("null API key should be removed: %#v", secrets)
	}
	if secrets["token"] != "active-token" || secrets["passkey"] != " " {
		t.Fatalf("non-empty credentials were changed: %#v", secrets)
	}
}

func TestMergeSiteConfigRestoresInMemoryDefinition(t *testing.T) {
	config := map[string]any{"format": "json"}
	mergeSiteConfig(config, map[string]any{"apikey": "fixture-key"})
	if config["format"] != "json" || config["apikey"] != "fixture-key" {
		t.Fatalf("站点内存配置合并错误: %#v", config)
	}
}

func TestSitePriorityUsesStableDefaults(t *testing.T) {
	if sitePriority(Site{Config: map[string]any{"pri": float64(4)}}) != 4 {
		t.Fatal("numeric priority was not read")
	}
	if sitePriority(Site{Config: map[string]any{"pri": "invalid"}}) != 0 {
		t.Fatal("invalid priority should use default")
	}
	if sitePriority(Site{}) != 0 {
		t.Fatal("missing priority should use default")
	}
}

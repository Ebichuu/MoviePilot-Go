package config

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadDefaultsAndPrecedence(t *testing.T) {
	dir := t.TempDir()
	envFile := filepath.Join(dir, "app.env")
	content := "APP_DOMAIN=https://from-file.example\nMOVIEPILOT_HTTP_ADDR=:4000\nSUBSCRIBE_RSS_INTERVAL=15\n"
	if err := os.WriteFile(envFile, []byte(content), 0o600); err != nil {
		t.Fatalf("write app.env: %v", err)
	}

	t.Setenv("CONFIG_DIR", dir)
	t.Setenv("MOVIEPILOT_ENV", "development")
	t.Setenv("MOVIEPILOT_HTTP_ADDR", ":5000")
	// 空值应视为未设置，不遮盖 app.env 中的 APP_DOMAIN。
	t.Setenv("APP_DOMAIN", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.HTTPAddr != ":5000" {
		t.Fatalf("HTTPAddr = %q, want :5000", cfg.HTTPAddr)
	}
	if cfg.AppDomain != "https://from-file.example" {
		t.Fatalf("AppDomain = %q, want https://from-file.example", cfg.AppDomain)
	}
	if cfg.SubscribeRSSInterval != 15*time.Minute {
		t.Fatalf("SubscribeRSSInterval = %v, want 15m", cfg.SubscribeRSSInterval)
	}
	if cfg.Location == nil || cfg.Location.String() != "Asia/Shanghai" {
		t.Fatalf("Location = %v, want Asia/Shanghai", cfg.Location)
	}
	if cfg.JWTSecret == "" || cfg.ResourceSecret == "" || len(cfg.MasterKey) < 16 {
		t.Fatal("development defaults must populate runtime secrets")
	}
}

func TestLoadProductionRequiresSecrets(t *testing.T) {
	t.Setenv("CONFIG_DIR", t.TempDir())
	t.Setenv("MOVIEPILOT_ENV", "production")
	t.Setenv("MOVIEPILOT_DATABASE_URL", "")
	t.Setenv("MOVIEPILOT_JWT_SECRET", "")
	t.Setenv("MOVIEPILOT_RESOURCE_SECRET", "")
	t.Setenv("MOVIEPILOT_MASTER_KEY", "")
	t.Setenv("MOVIEPILOT_BOOTSTRAP_PASSWORD", "")

	if _, err := Load(); err == nil {
		t.Fatal("expected production Load() without secrets to fail")
	}
}

func TestEnvironmentSettingsUpdateAndPersist(t *testing.T) {
	dir := t.TempDir()
	var gotRSS time.Duration
	settings := NewEnvironmentSettings(Config{
		ConfigDir:            dir,
		Location:             time.UTC,
		SubscribeRSSInterval: 30 * time.Minute,
	}, func(_ context.Context, interval time.Duration) error {
		gotRSS = interval
		return nil
	})

	results, err := settings.Update(context.Background(), map[string]any{
		"APP_DOMAIN":             "https://mp.example",
		"SUBSCRIBE_RSS_INTERVAL": json.Number("10"),
		"SITE_MESSAGE":           true,
		"MOVIEPILOT_MASTER_KEY":  "forbidden",
	})
	if err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if !results["APP_DOMAIN"].Changed || !results["SUBSCRIBE_RSS_INTERVAL"].Changed || !results["SITE_MESSAGE"].Changed {
		t.Fatalf("unexpected results = %#v", results)
	}
	if !results["MOVIEPILOT_MASTER_KEY"].Failed {
		t.Fatalf("expected protected key to fail, got %#v", results["MOVIEPILOT_MASTER_KEY"])
	}
	if gotRSS != 10*time.Minute {
		t.Fatalf("rss callback = %v, want 10m", gotRSS)
	}
	if !settings.SiteMessagesEnabled() {
		t.Fatal("SiteMessagesEnabled() = false, want true")
	}

	persisted, err := loadEnvFile(filepath.Join(dir, "app.env"))
	if err != nil {
		t.Fatalf("loadEnvFile() error = %v", err)
	}
	if persisted["APP_DOMAIN"] != "https://mp.example" {
		t.Fatalf("persisted APP_DOMAIN = %q", persisted["APP_DOMAIN"])
	}
	if _, leaked := persisted["MOVIEPILOT_MASTER_KEY"]; leaked {
		t.Fatal("protected key must not be persisted to app.env")
	}
}

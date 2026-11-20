package downloadconfig

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func TestDownloaderRuntimeForceResume(t *testing.T) {
	tests := []struct {
		name    string
		config  map[string]any
		want    bool
		wantErr bool
	}{
		{name: "absent defaults to false", config: map[string]any{"host": "http://qb:8080"}},
		{name: "true enables force resume", config: map[string]any{"host": "http://qb:8080", "force_resume": true}, want: true},
		{name: "false stays normal start", config: map[string]any{"host": "http://qb:8080", "force_resume": false}},
		{name: "null uses the V2 default", config: map[string]any{"host": "http://qb:8080", "force_resume": nil}},
		{name: "invalid type rejected", config: map[string]any{"host": "http://qb:8080", "force_resume": "yes"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runtime, err := Downloader{ID: "downloader-1", Name: "qb", Type: "qbittorrent", Config: tt.config}.Runtime()
			if tt.wantErr {
				if err == nil {
					t.Fatal("Runtime() error = nil, want invalid force_resume type error")
				}
				return
			}
			if err != nil {
				t.Fatalf("Runtime() error = %v", err)
			}
			if runtime.ForceResume() != tt.want {
				t.Fatalf("ForceResume() = %v, want %v", runtime.ForceResume(), tt.want)
			}
		})
	}
}

func TestDownloaderRuntimeAuthentication(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		config       map[string]any
		wantMethod   AuthMethod
		wantUsername string
		wantPassword string
		wantAPIKey   string
		wantErr      bool
	}{
		{
			name: "API Key 优先",
			config: map[string]any{
				"host": "https://qb.example/", "username": "admin", "password": "password-secret", "apikey": "apikey-secret",
			},
			wantMethod: AuthAPIKey,
			wantAPIKey: "apikey-secret",
		},
		{
			name: "用户名密码",
			config: map[string]any{
				"host": "http://qb.example:8080", "username": "admin", "password": "password-secret",
			},
			wantMethod: AuthUsernamePassword, wantUsername: "admin", wantPassword: "password-secret",
		},
		{name: "无认证", config: map[string]any{"host": "http://qb.example"}, wantMethod: AuthNone},
		{name: "拒绝错误秘密类型", config: map[string]any{"host": "http://qb.example", "apikey": 123}, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			downloader := Downloader{
				ID: "b7088afb-12ab-4a76-82b7-2f478428ec85", Name: "qb", Type: "qbittorrent",
				Enabled: true, Default: true, Config: tt.config,
			}
			runtime, err := downloader.Runtime()
			if tt.wantErr {
				if err == nil {
					t.Fatal("Runtime() error = nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("Runtime() error = %v", err)
			}
			if runtime.ID != downloader.ID || runtime.Host != tt.config["host"] || !runtime.Enabled || !runtime.Default {
				t.Fatalf("Runtime() 基础字段错误: %#v", runtime)
			}
			authentication := runtime.Authentication()
			if authentication.Method() != tt.wantMethod || authentication.Username() != tt.wantUsername || authentication.Password() != tt.wantPassword || authentication.APIKey() != tt.wantAPIKey {
				t.Fatalf("Authentication() = method=%q username=%q password=%q apikey=%q",
					authentication.Method(), authentication.Username(), authentication.Password(), authentication.APIKey())
			}
		})
	}
}

func TestDownloaderStructuredLoggingRedactsSecrets(t *testing.T) {
	t.Parallel()

	downloader := Downloader{
		ID: "b7088afb-12ab-4a76-82b7-2f478428ec85", Name: "qb", Type: "qbittorrent",
		Enabled: true, Default: true,
		Config: map[string]any{
			"host":     "https://user:host-secret@qb.example/private?token=query-secret",
			"username": "admin", "password": "password-secret", "apikey": "apikey-secret",
		},
	}
	runtime, err := downloader.Runtime()
	if err != nil {
		t.Fatalf("Runtime() error = %v", err)
	}

	var output bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&output, nil))
	logger.Info("downloader", "config", downloader, "runtime", runtime, "authentication", runtime.Authentication())
	logged := output.String()
	for _, secret := range []string{"host-secret", "query-secret", "password-secret", "apikey-secret"} {
		if strings.Contains(logged, secret) {
			t.Fatalf("结构化日志泄漏秘密 %q: %s", secret, logged)
		}
	}
	if !strings.Contains(logged, "https://qb.example") || !strings.Contains(logged, string(AuthAPIKey)) {
		t.Fatalf("结构化日志缺少安全诊断字段: %s", logged)
	}
}

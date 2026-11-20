package database

import (
	"maps"
	"reflect"
	"testing"
)

func TestSplitLegacySiteCredentials(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		config          map[string]any
		wantPublic      map[string]any
		wantCredentials map[string]any
		wantChanged     bool
	}{
		{
			name: "分离全部已知敏感字段",
			config: map[string]any{
				"format":        "rss",
				"ua":            "fixture-agent",
				"cookie":        "legacy-cookie",
				"apikey":        "legacy-api-key",
				"api_key":       "legacy-api-key-alias",
				"token":         "legacy-token",
				"passkey":       "legacy-passkey",
				"authorization": "legacy-authorization",
				"headers":       map[string]any{"X-Fixture": "legacy-header"},
				"secret":        "legacy-secret",
			},
			wantPublic: map[string]any{
				"format": "rss",
				"ua":     "fixture-agent",
			},
			wantCredentials: map[string]any{
				"cookie":        "legacy-cookie",
				"apikey":        "legacy-api-key",
				"api_key":       "legacy-api-key-alias",
				"token":         "legacy-token",
				"passkey":       "legacy-passkey",
				"authorization": "legacy-authorization",
				"headers":       map[string]any{"X-Fixture": "legacy-header"},
				"secret":        "legacy-secret",
			},
			wantChanged: true,
		},
		{
			name: "大小写键和空凭据也从公开配置移除",
			config: map[string]any{
				"format":          "json",
				" Cookie ":        "",
				"API_KEY":         nil,
				" Authorization ": "legacy-authorization",
			},
			wantPublic: map[string]any{"format": "json"},
			wantCredentials: map[string]any{
				" Authorization ": "legacy-authorization",
			},
			wantChanged: true,
		},
		{
			name:            "没有敏感字段时保持公开配置",
			config:          map[string]any{"format": "rss", "timeout": 15},
			wantPublic:      map[string]any{"format": "rss", "timeout": 15},
			wantCredentials: map[string]any{},
		},
		{
			name:            "空配置",
			config:          nil,
			wantPublic:      map[string]any{},
			wantCredentials: map[string]any{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			original := maps.Clone(tt.config)

			publicConfig, credentials, changed := splitLegacySiteCredentials(tt.config)

			if changed != tt.wantChanged {
				t.Fatalf("changed = %v, want %v", changed, tt.wantChanged)
			}
			if !reflect.DeepEqual(publicConfig, tt.wantPublic) {
				t.Fatal("公开配置分离结果不符合预期")
			}
			if !reflect.DeepEqual(credentials, tt.wantCredentials) {
				t.Fatal("敏感配置分离结果不符合预期")
			}
			if !reflect.DeepEqual(tt.config, original) {
				t.Fatal("分离逻辑修改了输入配置")
			}
		})
	}
}

func TestMergeLegacySiteCredentials(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		existing map[string]any
		legacy   map[string]any
		want     map[string]any
		wantAdd  bool
	}{
		{
			name: "已有凭据优先且 API Key 别名视为同一字段",
			existing: map[string]any{
				"cookie": "current-cookie",
				"apikey": "current-api-key",
				"custom": "current-custom-secret",
			},
			legacy: map[string]any{
				"cookie":  "legacy-cookie",
				"api_key": "legacy-api-key",
				"token":   "legacy-token",
			},
			want: map[string]any{
				"cookie": "current-cookie",
				"apikey": "current-api-key",
				"custom": "current-custom-secret",
				"token":  "legacy-token",
			},
			wantAdd: true,
		},
		{
			name:     "已有 API Key 别名保护规范键",
			existing: map[string]any{"api_key": "current-api-key"},
			legacy:   map[string]any{"apikey": "legacy-api-key"},
			want:     map[string]any{"api_key": "current-api-key"},
		},
		{
			name:    "没有已有凭据时完整迁移",
			legacy:  map[string]any{"cookie": "legacy-cookie", "token": "legacy-token"},
			want:    map[string]any{"cookie": "legacy-cookie", "token": "legacy-token"},
			wantAdd: true,
		},
		{
			name:     "没有历史凭据时保持已有凭据",
			existing: map[string]any{"cookie": "current-cookie"},
			want:     map[string]any{"cookie": "current-cookie"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			existingBefore := maps.Clone(tt.existing)
			legacyBefore := maps.Clone(tt.legacy)

			merged, added := mergeLegacySiteCredentials(tt.existing, tt.legacy)

			if added != tt.wantAdd {
				t.Fatalf("added = %v, want %v", added, tt.wantAdd)
			}
			if !reflect.DeepEqual(merged, tt.want) {
				t.Fatal("敏感配置合并结果不符合预期")
			}
			if !reflect.DeepEqual(tt.existing, existingBefore) {
				t.Fatal("合并逻辑修改了已有凭据")
			}
			if !reflect.DeepEqual(tt.legacy, legacyBefore) {
				t.Fatal("合并逻辑修改了历史凭据")
			}
		})
	}
}

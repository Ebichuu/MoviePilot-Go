package database

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/moviepilot-go/moviepilot-go/internal/auth"
	"github.com/moviepilot-go/moviepilot-go/internal/mediaconfig"
)

const mediaServersSettingTestMasterKey = "media-servers-setting-test-master-key"

func TestMediaServersSettingCodecRoundTrip(t *testing.T) {
	t.Parallel()

	secretBox := newMediaServersSettingTestBox(t)
	plainValue := json.RawMessage(`[{"id":"11111111-1111-4111-8111-111111111111","name":"Emby","type":"emby","enabled":true,"sync_interval":30,"sync_libraries":["movies"],"path_mapping":[],"config":{"host":"https://emby.example/emby","play_host":"","username":"admin","apikey":"apikey-secret-value"}}]`)
	storedValue, err := encodeMediaServersSetting(secretBox, plainValue)
	if err != nil {
		t.Fatalf("encodeMediaServersSetting() error = %v", err)
	}
	for _, secret := range []string{"apikey-secret-value", "admin"} {
		if bytes.Contains(storedValue, []byte(secret)) {
			t.Fatalf("数据库值泄漏敏感字段 %q: %s", secret, storedValue)
		}
	}
	var cipherText string
	if err := json.Unmarshal(storedValue, &cipherText); err != nil {
		t.Fatalf("密文不是 JSON 字符串: %v", err)
	}
	if !strings.HasPrefix(cipherText, "v1:") {
		t.Fatalf("密文版本 = %q, want v1", cipherText)
	}

	decoded, err := decodeMediaServersSetting(secretBox, storedValue, true)
	if err != nil {
		t.Fatalf("decodeMediaServersSetting() error = %v", err)
	}
	if !bytes.Equal(decoded, plainValue) {
		t.Fatalf("解密值 = %s, want %s", decoded, plainValue)
	}
}

func TestDecodeMediaServersSetting(t *testing.T) {
	t.Parallel()

	secretBox := newMediaServersSettingTestBox(t)
	plainValue := json.RawMessage(`[]`)
	encryptedValue, err := encodeMediaServersSetting(secretBox, plainValue)
	if err != nil {
		t.Fatalf("准备密文: %v", err)
	}
	invalidPlainTextCipher, err := secretBox.Encrypt("apikey-secret-value-not-json")
	if err != nil {
		t.Fatalf("准备非法明文密文: %v", err)
	}
	invalidPlainTextStored, err := json.Marshal(invalidPlainTextCipher)
	if err != nil {
		t.Fatalf("编码非法明文密文: %v", err)
	}

	tests := []struct {
		name      string
		secretBox *auth.SecretBox
		stored    json.RawMessage
		encrypted bool
		want      json.RawMessage
		wantErr   bool
	}{
		{name: "兼容旧明文", stored: plainValue, want: plainValue},
		{name: "解密有效记录", secretBox: secretBox, stored: encryptedValue, encrypted: true, want: plainValue},
		{name: "拒绝无解密器", stored: encryptedValue, encrypted: true, wantErr: true},
		{name: "拒绝非字符串密文", secretBox: secretBox, stored: json.RawMessage(`{"ciphertext":"secret"}`), encrypted: true, wantErr: true},
		{name: "拒绝损坏密文", secretBox: secretBox, stored: json.RawMessage(`"v1:c2VjcmV0"`), encrypted: true, wantErr: true},
		{name: "拒绝解密后的非 JSON", secretBox: secretBox, stored: invalidPlainTextStored, encrypted: true, wantErr: true},
		{name: "拒绝非法旧明文", secretBox: secretBox, stored: json.RawMessage(`apikey-secret-value`), wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, decodeErr := decodeMediaServersSetting(tt.secretBox, tt.stored, tt.encrypted)
			if tt.wantErr {
				if decodeErr == nil {
					t.Fatal("decodeMediaServersSetting() error = nil")
				}
				if strings.Contains(decodeErr.Error(), "apikey-secret-value") {
					t.Fatalf("错误信息泄漏 API Key: %v", decodeErr)
				}
				return
			}
			if decodeErr != nil {
				t.Fatalf("decodeMediaServersSetting() error = %v", decodeErr)
			}
			if !bytes.Equal(got, tt.want) {
				t.Fatalf("decodeMediaServersSetting() = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestPrepareMediaServersSetting(t *testing.T) {
	t.Parallel()

	valid := func() []mediaconfig.Server {
		return []mediaconfig.Server{{
			ID: "11111111-1111-4111-8111-111111111111", Name: "Emby", Type: "emby", Enabled: true,
			Config: mediaconfig.Config{Host: "https://emby.example/emby", APIKey: "apikey-secret-value"},
		}}
	}
	tests := []struct {
		name      string
		secretBox *auth.SecretBox
		servers   func() []mediaconfig.Server
		wantID    string
		wantErr   bool
	}{
		{name: "保存完整设置", secretBox: newMediaServersSettingTestBox(t), servers: valid, wantID: "11111111-1111-4111-8111-111111111111"},
		{
			name:      "保存时补齐旧无 ID",
			secretBox: newMediaServersSettingTestBox(t),
			servers: func() []mediaconfig.Server {
				value := valid()
				value[0].ID = ""
				return value
			},
			wantID: "",
		},
		{
			name:      "启用设置缺少 API Key",
			secretBox: newMediaServersSettingTestBox(t),
			servers: func() []mediaconfig.Server {
				value := valid()
				value[0].Config.APIKey = ""
				return value
			},
			wantErr: true,
		},
		{name: "缺少加密器", servers: valid, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			snapshot, stored, err := prepareMediaServersSetting(tt.secretBox, tt.servers())
			if tt.wantErr {
				if err == nil {
					t.Fatal("prepareMediaServersSetting() error = nil")
				}
				if strings.Contains(err.Error(), "apikey-secret-value") {
					t.Fatalf("错误信息泄漏 API Key: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("prepareMediaServersSetting() error = %v", err)
			}
			if len(stored) == 0 || !bytes.Contains(stored, []byte(`"v1:`)) {
				t.Fatalf("保存值不是带版本密文: %s", stored)
			}
			if tt.wantID != "" && snapshot.RequiredID != tt.wantID {
				t.Fatalf("snapshot.RequiredID = %q, want %q", snapshot.RequiredID, tt.wantID)
			}
			if tt.wantID == "" && snapshot.RequiredID == "" {
				if len(snapshot.Servers) != 1 || snapshot.Servers[0].ID == "" {
					t.Fatalf("未补齐稳定 ID: %#v", snapshot)
				}
			}
			plain, err := decodeMediaServersSetting(tt.secretBox, stored, true)
			if err != nil {
				t.Fatalf("读取保存值: %v", err)
			}
			loaded, _, err := decodeMediaServersSnapshot(plain)
			if err != nil {
				t.Fatalf("规范化读取快照: %v", err)
			}
			if loaded.RequiredID != snapshot.RequiredID || loaded.Servers[0].ID != snapshot.Servers[0].ID {
				t.Fatalf("保存读取后稳定标识变化: saved=%#v loaded=%#v", snapshot, loaded)
			}
		})
	}
}

func TestDecodeMediaServersSnapshot(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		value        json.RawMessage
		wantChanged  bool
		wantRequired string
		wantServerID string
		wantErr      bool
	}{
		{
			name:         "规范化快照",
			value:        json.RawMessage(`[{"id":"11111111-1111-4111-8111-111111111111","name":"Emby","type":"emby","enabled":true,"sync_interval":5,"sync_libraries":[],"path_mapping":[],"config":{"host":"https://emby.example/emby","play_host":"","username":"","apikey":"apikey-secret-value"}}]`),
			wantRequired: "11111111-1111-4111-8111-111111111111", wantServerID: "11111111-1111-4111-8111-111111111111",
		},
		{
			name:        "旧记录补 ID",
			value:       json.RawMessage(`[{"name":"Emby","type":"emby","enabled":true,"config":{"host":"https://emby.example/emby","apikey":"apikey-secret-value"}}]`),
			wantChanged: true, wantRequired: "any", wantServerID: "any",
		},
		{name: "启用记录缺少密钥", value: json.RawMessage(`[{"name":"Emby","type":"emby","enabled":true,"config":{"host":"https://emby.example/emby"}}]`), wantErr: true},
		{name: "非法类型", value: json.RawMessage(`[{"name":"Plex","type":"plex"}]`), wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			snapshot, changed, err := decodeMediaServersSnapshot(tt.value)
			if tt.wantErr {
				if err == nil {
					t.Fatal("decodeMediaServersSnapshot() error = nil")
				}
				if strings.Contains(err.Error(), "apikey-secret-value") {
					t.Fatalf("错误信息泄漏 API Key: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("decodeMediaServersSnapshot() error = %v", err)
			}
			if changed != tt.wantChanged {
				t.Fatalf("decodeMediaServersSnapshot() = changed=%v snapshot=%#v", changed, snapshot)
			}
			if tt.wantRequired == "any" && snapshot.RequiredID == "" {
				t.Fatalf("required ID 未补齐: %#v", snapshot)
			}
			if tt.wantRequired != "any" && snapshot.RequiredID != tt.wantRequired {
				t.Fatalf("required ID = %q, want %q", snapshot.RequiredID, tt.wantRequired)
			}
			if tt.wantServerID != "" && tt.wantServerID != "any" && snapshot.Servers[0].ID != tt.wantServerID {
				t.Fatalf("server ID = %q, want %q", snapshot.Servers[0].ID, tt.wantServerID)
			}
			if tt.wantServerID == "any" && snapshot.Servers[0].ID == "" {
				t.Fatal("旧记录未生成稳定 ID")
			}
		})
	}
}

func newMediaServersSettingTestBox(t *testing.T) *auth.SecretBox {
	t.Helper()
	secretBox, err := auth.NewSecretBox(mediaServersSettingTestMasterKey)
	if err != nil {
		t.Fatalf("auth.NewSecretBox() error = %v", err)
	}
	return secretBox
}

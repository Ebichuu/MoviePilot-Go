package database

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/moviepilot-go/moviepilot-go/internal/auth"
)

const notificationSettingsTestMasterKey = "notification-settings-test-master-key"

func TestNotificationSettingsCodecRoundTrip(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		value json.RawMessage
	}{
		{name: "对象", value: json.RawMessage(`{"TELEGRAM_TOKEN":"telegram-token-secret"}`)},
		{name: "数组", value: json.RawMessage(`[{"type":"telegram","enabled":true}]`)},
		{name: "字符串", value: json.RawMessage(`"telegram-token-secret"`)},
		{name: "布尔值", value: json.RawMessage(`true`)},
		{name: "数字", value: json.RawMessage(`42`)},
		{name: "空值", value: json.RawMessage(`null`)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			secretBox := newNotificationSettingsTestBox(t)
			storedValue, err := encodeNotificationSetting(secretBox, tt.value)
			if err != nil {
				t.Fatalf("encodeNotificationSetting() error = %v", err)
			}
			if bytes.Contains(storedValue, []byte("telegram-token-secret")) {
				t.Fatalf("数据库值泄漏通知配置: %s", storedValue)
			}
			var cipherText string
			if err := json.Unmarshal(storedValue, &cipherText); err != nil {
				t.Fatalf("密文不是 JSON 字符串: %v", err)
			}
			if !strings.HasPrefix(cipherText, "v1:") {
				t.Fatalf("密文版本 = %q, want v1", cipherText)
			}

			decoded, err := decodeNotificationSetting(secretBox, storedValue, true)
			if err != nil {
				t.Fatalf("decodeNotificationSetting() error = %v", err)
			}
			if !bytes.Equal(decoded, tt.value) {
				t.Fatalf("解密值 = %s, want %s", decoded, tt.value)
			}
		})
	}
}

func TestDecodeNotificationSetting(t *testing.T) {
	t.Parallel()

	secretBox := newNotificationSettingsTestBox(t)
	plainValue := json.RawMessage(`[{"config":{"TELEGRAM_TOKEN":"telegram-token-secret"}}]`)
	encryptedValue, err := encodeNotificationSetting(secretBox, plainValue)
	if err != nil {
		t.Fatalf("准备密文: %v", err)
	}
	invalidJSONCipher, err := secretBox.Encrypt("telegram-token-secret-not-json")
	if err != nil {
		t.Fatalf("准备非法 JSON 密文: %v", err)
	}
	invalidJSONStored, err := json.Marshal(invalidJSONCipher)
	if err != nil {
		t.Fatalf("编码非法 JSON 密文: %v", err)
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
		{name: "拒绝非字符串密文", secretBox: secretBox, stored: json.RawMessage(`{"TELEGRAM_TOKEN":"telegram-token-secret"}`), encrypted: true, wantErr: true},
		{name: "拒绝空密文", secretBox: secretBox, stored: json.RawMessage(`""`), encrypted: true, wantErr: true},
		{name: "拒绝损坏密文", secretBox: secretBox, stored: json.RawMessage(`"v1:dGVsZWdyYW0tdG9rZW4tc2VjcmV0"`), encrypted: true, wantErr: true},
		{name: "拒绝解密后的非法 JSON", secretBox: secretBox, stored: invalidJSONStored, encrypted: true, wantErr: true},
		{name: "拒绝非法旧明文", secretBox: secretBox, stored: json.RawMessage(`telegram-token-secret`), wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, decodeErr := decodeNotificationSetting(tt.secretBox, tt.stored, tt.encrypted)
			if tt.wantErr {
				if decodeErr == nil {
					t.Fatal("decodeNotificationSetting() error = nil")
				}
				if strings.Contains(decodeErr.Error(), "telegram-token-secret") || strings.Contains(decodeErr.Error(), "v1:") {
					t.Fatalf("错误信息泄漏通知配置或密文: %v", decodeErr)
				}
				return
			}
			if decodeErr != nil {
				t.Fatalf("decodeNotificationSetting() error = %v", decodeErr)
			}
			if !bytes.Equal(got, tt.want) {
				t.Fatalf("decodeNotificationSetting() = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestEncodeNotificationSettingRejectsInvalidInput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		secretBox *auth.SecretBox
		value     json.RawMessage
	}{
		{name: "缺少加密器", value: json.RawMessage(`null`)},
		{name: "非法 JSON", secretBox: newNotificationSettingsTestBox(t), value: json.RawMessage(`telegram-token-secret`)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := encodeNotificationSetting(tt.secretBox, tt.value)
			if err == nil {
				t.Fatal("encodeNotificationSetting() error = nil")
			}
			if strings.Contains(err.Error(), "telegram-token-secret") {
				t.Fatalf("错误信息泄漏通知配置: %v", err)
			}
		})
	}
}

func TestNotificationSettingsNilStoreIsSafe(t *testing.T) {
	t.Parallel()

	var nilStore *Store
	value, found, err := nilStore.LoadNotificationSettings(t.Context())
	if err == nil || found || value != nil {
		t.Fatalf("nil Store LoadNotificationSettings() = (%s, %v, %v)", value, found, err)
	}
	if err := nilStore.SaveNotificationSettings(t.Context(), json.RawMessage(`"telegram-token-secret"`)); err == nil {
		t.Fatal("nil Store SaveNotificationSettings() error = nil")
	} else if strings.Contains(err.Error(), "telegram-token-secret") {
		t.Fatalf("错误信息泄漏通知配置: %v", err)
	}

	storeWithoutDB := NewStore(nil, newNotificationSettingsTestBox(t))
	if _, _, err := storeWithoutDB.LoadNotificationSettings(t.Context()); err == nil {
		t.Fatal("无数据库 LoadNotificationSettings() error = nil")
	}
	if err := storeWithoutDB.SaveNotificationSettings(t.Context(), json.RawMessage(`null`)); err == nil {
		t.Fatal("无数据库 SaveNotificationSettings() error = nil")
	}
}

func TestUpgradeLegacyNotificationSettingRejectsUnavailableStore(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		store *Store
	}{
		{name: "空 Store"},
		{name: "缺少数据库", store: NewStore(nil, newNotificationSettingsTestBox(t))},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			upgraded, err := tt.store.upgradeLegacyNotificationSetting(
				t.Context(),
				json.RawMessage(`{"token":"telegram-token-secret"}`),
			)
			if err == nil || upgraded {
				t.Fatalf("upgradeLegacyNotificationSetting() = (%v, %v), want (false, error)", upgraded, err)
			}
			if strings.Contains(err.Error(), "telegram-token-secret") {
				t.Fatalf("错误信息泄漏通知配置: %v", err)
			}
		})
	}
}

func TestNotificationSettingsKey(t *testing.T) {
	t.Parallel()
	if NotificationSettingsKey != "Notifications" {
		t.Fatalf("NotificationSettingsKey = %q, want Notifications", NotificationSettingsKey)
	}
}

func newNotificationSettingsTestBox(t *testing.T) *auth.SecretBox {
	t.Helper()
	secretBox, err := auth.NewSecretBox(notificationSettingsTestMasterKey)
	if err != nil {
		t.Fatalf("auth.NewSecretBox() error = %v", err)
	}
	return secretBox
}

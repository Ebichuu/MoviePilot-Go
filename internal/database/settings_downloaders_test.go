package database

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/moviepilot-go/moviepilot-go/internal/auth"
)

const downloadersSettingTestMasterKey = "downloaders-setting-test-master-key"

func TestDownloadersSettingCodecRoundTrip(t *testing.T) {
	t.Parallel()

	secretBox := newDownloadersSettingTestBox(t)
	plainValue := json.RawMessage(`[{"id":"b7088afb-12ab-4a76-82b7-2f478428ec85","config":{"password":"password-secret","apikey":"apikey-secret"}}]`)
	storedValue, err := encodeDownloadersSetting(secretBox, plainValue)
	if err != nil {
		t.Fatalf("encodeDownloadersSetting() error = %v", err)
	}
	for _, secret := range []string{"password-secret", "apikey-secret"} {
		if bytes.Contains(storedValue, []byte(secret)) {
			t.Fatalf("数据库值泄漏秘密 %q: %s", secret, storedValue)
		}
	}
	var cipherText string
	if err := json.Unmarshal(storedValue, &cipherText); err != nil {
		t.Fatalf("密文不是 JSON 字符串: %v", err)
	}
	if !strings.HasPrefix(cipherText, "v1:") {
		t.Fatalf("密文版本 = %q, want v1", cipherText)
	}

	decoded, err := decodeDownloadersSetting(secretBox, storedValue, true)
	if err != nil {
		t.Fatalf("decodeDownloadersSetting() error = %v", err)
	}
	if !bytes.Equal(decoded, plainValue) {
		t.Fatalf("解密值 = %s, want %s", decoded, plainValue)
	}
}

func TestDecodeDownloadersSetting(t *testing.T) {
	t.Parallel()

	secretBox := newDownloadersSettingTestBox(t)
	plainValue := json.RawMessage(`[{"name":"qb"}]`)
	encryptedValue, err := encodeDownloadersSetting(secretBox, plainValue)
	if err != nil {
		t.Fatalf("准备密文: %v", err)
	}
	invalidPlainTextCipher, err := secretBox.Encrypt("password-secret-not-json")
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
		{name: "兼容旧明文记录", secretBox: secretBox, stored: plainValue, want: plainValue},
		{name: "解密有效记录", secretBox: secretBox, stored: encryptedValue, encrypted: true, want: plainValue},
		{name: "拒绝无解密器", stored: encryptedValue, encrypted: true, wantErr: true},
		{name: "拒绝非字符串密文", secretBox: secretBox, stored: json.RawMessage(`{"ciphertext":"password-secret"}`), encrypted: true, wantErr: true},
		{name: "拒绝损坏密文", secretBox: secretBox, stored: json.RawMessage(`"v1:cGFzc3dvcmQtc2VjcmV0"`), encrypted: true, wantErr: true},
		{name: "拒绝解密后的非法 JSON", secretBox: secretBox, stored: invalidPlainTextStored, encrypted: true, wantErr: true},
		{name: "拒绝非法旧明文", secretBox: secretBox, stored: json.RawMessage(`password-secret`), wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, decodeErr := decodeDownloadersSetting(tt.secretBox, tt.stored, tt.encrypted)
			if tt.wantErr {
				if decodeErr == nil {
					t.Fatalf("decodeDownloadersSetting() error = nil")
				}
				if strings.Contains(decodeErr.Error(), "password-secret") {
					t.Fatalf("错误信息泄漏秘密: %v", decodeErr)
				}
				return
			}
			if decodeErr != nil {
				t.Fatalf("decodeDownloadersSetting() error = %v", decodeErr)
			}
			if !bytes.Equal(got, tt.want) {
				t.Fatalf("decodeDownloadersSetting() = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestEncodeDownloadersSettingRejectsInvalidInput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		secretBox *auth.SecretBox
		value     json.RawMessage
	}{
		{name: "缺少加密器", value: json.RawMessage(`[]`)},
		{name: "非法 JSON", secretBox: newDownloadersSettingTestBox(t), value: json.RawMessage(`password-secret`)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := encodeDownloadersSetting(tt.secretBox, tt.value)
			if err == nil {
				t.Fatal("encodeDownloadersSetting() error = nil")
			}
			if strings.Contains(err.Error(), "password-secret") {
				t.Fatalf("错误信息泄漏秘密: %v", err)
			}
		})
	}
}

func newDownloadersSettingTestBox(t *testing.T) *auth.SecretBox {
	t.Helper()
	secretBox, err := auth.NewSecretBox(downloadersSettingTestMasterKey)
	if err != nil {
		t.Fatalf("auth.NewSecretBox() error = %v", err)
	}
	return secretBox
}

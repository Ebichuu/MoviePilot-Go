package auth

import (
	"bytes"
	"encoding/base64"
	"strings"
	"sync"
	"testing"
)

const (
	secretBoxTestMasterKey      = "primary-master-key-fixture-2026"
	secretBoxTestWrongMasterKey = "different-master-key-fixture-2026"
	secretBoxTestPlaintext      = "plaintext-credential-fixture-2026"
)

func TestNewSecretBoxRejectsShortMasterKey(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		masterKey string
	}{
		{name: "空主密钥", masterKey: ""},
		{name: "十五字节主密钥", masterKey: "short-key-value"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			box, err := NewSecretBox(tt.masterKey)
			if err == nil || box != nil {
				t.Fatal("无效主密钥未被拒绝")
			}
			assertSecretBoxErrorRedacted(t, err, tt.masterKey)
		})
	}
}

func TestSecretBoxEncryptUsesDifferentNonces(t *testing.T) {
	t.Parallel()

	box := mustNewSecretBox(t, secretBoxTestMasterKey)
	first := mustEncryptSecretBox(t, box, secretBoxTestPlaintext)
	second := mustEncryptSecretBox(t, box, secretBoxTestPlaintext)
	if first == second {
		t.Fatal("相同明文重复加密产生了相同密文")
	}

	for _, encrypted := range []string{first, second} {
		decrypted, err := box.Decrypt(encrypted)
		if err != nil {
			t.Fatal("随机 nonce 密文解密失败")
		}
		if decrypted != secretBoxTestPlaintext {
			t.Fatal("随机 nonce 密文往返结果不一致")
		}
	}
}

func TestSecretBoxRejectsInvalidCiphertext(t *testing.T) {
	t.Parallel()

	box := mustNewSecretBox(t, secretBoxTestMasterKey)
	wrongKeyBox := mustNewSecretBox(t, secretBoxTestWrongMasterKey)
	encrypted := mustEncryptSecretBox(t, box, secretBoxTestPlaintext)
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(encrypted, "v1:"))
	if err != nil {
		t.Fatal("测试密文解码失败")
	}

	tamperedPayload := append([]byte(nil), payload...)
	tamperedPayload[len(tamperedPayload)-1] ^= 0x01
	truncatedPayload := append([]byte(nil), payload[:len(payload)-1]...)

	tests := []struct {
		name      string
		box       *SecretBox
		encrypted string
	}{
		{
			name:      "错误主密钥",
			box:       wrongKeyBox,
			encrypted: encrypted,
		},
		{
			name:      "单字节篡改",
			box:       box,
			encrypted: "v1:" + base64.RawURLEncoding.EncodeToString(tamperedPayload),
		},
		{
			name:      "截断密文",
			box:       box,
			encrypted: "v1:" + base64.RawURLEncoding.EncodeToString(truncatedPayload),
		},
		{
			name:      "非法 base64",
			box:       box,
			encrypted: "v1:not*base64",
		},
		{
			name:      "错误版本",
			box:       box,
			encrypted: "v2:" + strings.TrimPrefix(encrypted, "v1:"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			plainText, decryptErr := tt.box.Decrypt(tt.encrypted)
			if decryptErr == nil {
				t.Fatal("无效密文未被拒绝")
			}
			if plainText != "" {
				t.Fatal("解密失败仍返回了明文")
			}
			assertSecretBoxErrorRedacted(t, decryptErr,
				secretBoxTestPlaintext,
				secretBoxTestMasterKey,
				secretBoxTestWrongMasterKey,
				encrypted,
				strings.TrimPrefix(encrypted, "v1:"),
				tt.encrypted,
				strings.TrimPrefix(tt.encrypted, "v1:"),
				strings.TrimPrefix(tt.encrypted, "v2:"),
			)
		})
	}
}

func TestSecretBoxConcurrentEncryptDecrypt(t *testing.T) {
	t.Parallel()

	const (
		workers    = 32
		iterations = 64
	)
	box := mustNewSecretBox(t, secretBoxTestMasterKey)
	failures := make(chan string, workers)
	var waitGroup sync.WaitGroup
	waitGroup.Add(workers)
	for range workers {
		go func() {
			defer waitGroup.Done()
			for range iterations {
				encrypted, err := box.Encrypt(secretBoxTestPlaintext)
				if err != nil {
					failures <- "加密"
					return
				}
				decrypted, err := box.Decrypt(encrypted)
				if err != nil {
					failures <- "解密"
					return
				}
				if decrypted != secretBoxTestPlaintext {
					failures <- "往返"
					return
				}
			}
		}()
	}
	waitGroup.Wait()
	close(failures)

	for stage := range failures {
		t.Fatalf("并发 %s 阶段失败", stage)
	}
}

func FuzzSecretBoxRoundTrip(f *testing.F) {
	box := mustNewSecretBox(f, secretBoxTestMasterKey)
	for _, seed := range [][]byte{
		{},
		[]byte("fixture"),
		[]byte("包含 Unicode 的凭据"),
		{0x00, 0xff, 0x01, 0xfe},
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, plainText []byte) {
		encrypted, err := box.Encrypt(string(plainText))
		if err != nil {
			t.Fatal("模糊测试加密失败")
		}
		decrypted, err := box.Decrypt(encrypted)
		if err != nil {
			t.Fatal("模糊测试解密失败")
		}
		if !bytes.Equal([]byte(decrypted), plainText) {
			t.Fatal("模糊测试往返结果不一致")
		}
	})
}

func FuzzSecretBoxRejectsSingleByteTamper(f *testing.F) {
	box := mustNewSecretBox(f, secretBoxTestMasterKey)
	f.Add([]byte("fixture"), uint64(0), byte(0x01))
	f.Add([]byte{}, uint64(12), byte(0xff))
	f.Add([]byte("包含 Unicode 的凭据"), ^uint64(0), byte(0x00))

	f.Fuzz(func(t *testing.T, plainText []byte, mutationIndex uint64, mask byte) {
		encrypted, err := box.Encrypt(string(plainText))
		if err != nil {
			t.Fatal("篡改模糊测试加密失败")
		}
		payload, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(encrypted, "v1:"))
		if err != nil || len(payload) == 0 {
			t.Fatal("篡改模糊测试密文解码失败")
		}
		if mask == 0 {
			mask = 0x01
		}
		payload[mutationIndex%uint64(len(payload))] ^= mask
		tampered := "v1:" + base64.RawURLEncoding.EncodeToString(payload)
		if _, err := box.Decrypt(tampered); err == nil {
			t.Fatal("单字节篡改未被拒绝")
		}
	})
}

func mustNewSecretBox(t testing.TB, masterKey string) *SecretBox {
	t.Helper()
	box, err := NewSecretBox(masterKey)
	if err != nil {
		t.Fatal("创建测试加密器失败")
	}
	return box
}

func mustEncryptSecretBox(t testing.TB, box *SecretBox, plainText string) string {
	t.Helper()
	encrypted, err := box.Encrypt(plainText)
	if err != nil {
		t.Fatal("生成测试密文失败")
	}
	return encrypted
}

func assertSecretBoxErrorRedacted(t testing.TB, err error, sensitiveValues ...string) {
	t.Helper()
	message := err.Error()
	for _, sensitiveValue := range sensitiveValues {
		if sensitiveValue != "" && strings.Contains(message, sensitiveValue) {
			t.Fatal("错误文本泄露了敏感输入")
		}
	}
}

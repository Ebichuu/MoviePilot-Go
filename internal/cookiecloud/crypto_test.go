package cookiecloud

import (
	"bytes"
	"testing"
)

func TestEncryptDecryptRoundTrip(t *testing.T) {
	key := CryptKey("moviepilot", "secret")
	message := []byte(`{"cookies":"a=1; b=2","domain":"example.org"}`)
	encrypted, err := Encrypt(message, key)
	if err != nil {
		t.Fatalf("加密失败: %v", err)
	}
	if bytes.Contains([]byte(encrypted), message) {
		t.Fatal("密文里不应出现明文")
	}
	decrypted, err := Decrypt(encrypted, key)
	if err != nil {
		t.Fatalf("解密失败: %v", err)
	}
	if !bytes.Equal(decrypted, message) {
		t.Fatalf("往返不一致: %q", decrypted)
	}
}

func TestEncryptProducesDifferentCiphertexts(t *testing.T) {
	key := CryptKey("moviepilot", "secret")
	first, err := Encrypt([]byte("same"), key)
	if err != nil {
		t.Fatalf("加密失败: %v", err)
	}
	second, err := Encrypt([]byte("same"), key)
	if err != nil {
		t.Fatalf("加密失败: %v", err)
	}
	if first == second {
		t.Fatal("同一明文两次加密应因随机盐而不同")
	}
}

func TestDecryptRejectsWrongKey(t *testing.T) {
	encrypted, err := Encrypt([]byte("payload"), CryptKey("moviepilot", "right"))
	if err != nil {
		t.Fatalf("加密失败: %v", err)
	}
	if _, err := Decrypt(encrypted, CryptKey("moviepilot", "wrong")); err == nil {
		t.Fatal("错误口令应当解密失败")
	}
}

func TestDecryptRejectsGarbage(t *testing.T) {
	if _, err := Decrypt("not-base64!!", CryptKey("moviepilot", "secret")); err == nil {
		t.Fatal("非法密文应当报错")
	}
}

func TestCryptKeyIsDeterministic(t *testing.T) {
	first := CryptKey("moviepilot", "secret")
	second := CryptKey("moviepilot", "secret")
	if !bytes.Equal(first, second) {
		t.Fatal("相同输入应当得到相同的密钥")
	}
	if len(first) == 0 {
		t.Fatal("密钥不应为空")
	}
	if bytes.Equal(first, CryptKey("moviepilot", "other")) {
		t.Fatal("不同口令应得到不同密钥")
	}
}

func TestPadUnpadPKCS7(t *testing.T) {
	padded := padPKCS7([]byte("abc"), 8)
	if len(padded)%8 != 0 {
		t.Fatalf("补齐后长度 %d 不是块大小倍数", len(padded))
	}
	restored, err := unpadPKCS7(padded, 8)
	if err != nil {
		t.Fatalf("去补齐失败: %v", err)
	}
	if string(restored) != "abc" {
		t.Fatalf("去补齐结果 = %q", restored)
	}
}

func TestUnpadPKCS7RejectsInvalidPadding(t *testing.T) {
	if _, err := unpadPKCS7([]byte("abcdefgh"), 8); err == nil {
		t.Fatal("非法补位应当报错")
	}
}

func TestURLDomain(t *testing.T) {
	// 子域会被归一到主域，与 V2 `get_domain` 的行为一致（同一站点的多域名归一到主域）。
	cases := map[string]string{
		"https://www.example.org:443/path?x=1": "example.org",
		"http://pt.example.org":                "example.org",
		"https://a.b.example.org":              "a.b.example.org",
		"example.org/path":                     "example.org",
		"":                                     "",
	}
	for raw, want := range cases {
		if got := URLDomain(raw); got != want {
			t.Fatalf("URLDomain(%q) = %q，期望 %q", raw, got, want)
		}
	}
}

func TestSiteBaseURLKeepsSchemeAndHost(t *testing.T) {
	// 已经是完整 URL 时原样保留（只用于判断站点是否可达，不做路径裁剪）。
	if got := siteBaseURL("https://pt.example.org/index.php?x=1"); got != "https://pt.example.org/index.php?x=1" {
		t.Fatalf("siteBaseURL = %q", got)
	}
	if got := siteBaseURL("not a url"); got == "" {
		t.Fatalf("无法解析时应返回原值而不是空串: %q", got)
	}
}

func TestCloneSiteConfigIsDeepCopy(t *testing.T) {
	source := map[string]any{
		"cookie": "a=1",
		"nested": map[string]any{"k": "v"},
	}
	cloned := cloneSiteConfig(source)
	if cloned["cookie"] != "a=1" {
		t.Fatalf("克隆结果缺少字段: %v", cloned)
	}
	cloned["cookie"] = "b=2"
	if source["cookie"] != "a=1" {
		t.Fatal("修改克隆不应影响原 map")
	}
}

func TestCloneSiteConfigHandlesNil(t *testing.T) {
	cloned := cloneSiteConfig(nil)
	if cloned == nil {
		t.Fatal("nil 输入应返回空 map 而不是 nil")
	}
	if len(cloned) != 0 {
		t.Fatalf("期望空 map，实际 %v", cloned)
	}
}

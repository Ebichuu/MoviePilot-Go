package httpapi

import (
	"encoding/base32"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// RFC 4226 附录里的标准种子与其 base32 形式：用它把 TOTP 实现钉在已知答案上，
// 避免「自己生成、自己验证」这种永远通过的假测试。
const (
	rfcSecretBase32 = "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ"
	rfcSecretASCII  = "12345678901234567890"
)

func TestMFACodeAtMatchesRFCVectors(t *testing.T) {
	cases := map[int64]string{
		0: "755224",
		1: "287082",
		2: "359152",
	}
	for counter, want := range cases {
		if got := mfaCodeAt(rfcSecretBase32, counter); got != want {
			t.Fatalf("mfaCodeAt(%d) = %q，期望 RFC 向量 %q", counter, got, want)
		}
	}
	if got := mfaCodeAt(strings.ToLower(rfcSecretBase32), 1); got != "287082" {
		t.Fatalf("小写种子应等价，实际 %q", got)
	}
}

func TestMFACodeAtRejectsInvalidSecret(t *testing.T) {
	for _, secret := range []string{"", "not-base32!!", "ABC"} {
		if got := mfaCodeAt(secret, 1); got != "" {
			t.Fatalf("非法种子 %q 应返回空串，实际 %q", secret, got)
		}
	}
}

func TestMFAValidateCodeAcceptsCurrentWindow(t *testing.T) {
	// 该种子在 counter=1 时的一次性口令为 287082，对应 30–59 秒。
	now := time.Unix(40, 0).UTC()
	if !mfaValidateCode(rfcSecretBase32, "287082", now) {
		t.Fatal("当前时间窗口内的口令应通过")
	}
	// 允许前后各一个窗口，容忍时钟偏差。
	if !mfaValidateCode(rfcSecretBase32, "287082", time.Unix(70, 0).UTC()) {
		t.Fatal("相邻窗口应通过（时钟偏差容忍）")
	}
	if !mfaValidateCode(rfcSecretBase32, "287082", time.Unix(10, 0).UTC()) {
		t.Fatal("前一个窗口应通过（时钟偏差容忍）")
	}
}

func TestMFAValidateCodeRejectsOutOfWindow(t *testing.T) {
	now := time.Unix(40, 0).UTC()
	if mfaValidateCode(rfcSecretBase32, "287082", now.Add(90*time.Second)) {
		t.Fatal("超出 ±1 窗口的口令必须拒绝")
	}
	if mfaValidateCode(rfcSecretBase32, "000000", now) {
		t.Fatal("错误口令必须拒绝")
	}
}

func TestMFAValidateCodeRejectsMalformedInput(t *testing.T) {
	now := time.Unix(40, 0).UTC()
	for _, code := range []string{"", "28708", "2870821", "28708a", " 287082 "} {
		if mfaValidateCode(rfcSecretBase32, code, now) {
			t.Fatalf("畸形口令 %q 必须拒绝", code)
		}
	}
	if mfaValidateCode("", "287082", now) {
		t.Fatal("空种子必须拒绝")
	}
}

func TestMFANewSecretIsUsableBase32(t *testing.T) {
	secret, err := mfaNewSecret()
	if err != nil {
		t.Fatalf("生成种子失败: %v", err)
	}
	if len(secret) != 32 {
		t.Fatalf("20 字节种子的 base32 长度应为 32，实际 %d", len(secret))
	}
	decoded, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(secret)
	if err != nil {
		t.Fatalf("生成的种子不是合法 base32: %v", err)
	}
	if string(decoded) == rfcSecretASCII {
		t.Fatal("种子必须随机生成，不能是固定值")
	}
	other, err := mfaNewSecret()
	if err != nil {
		t.Fatalf("二次生成种子失败: %v", err)
	}
	if other == secret {
		t.Fatal("两次生成的种子不应相同")
	}
	// 生成的种子必须能直接用于计算口令。
	if mfaCodeAt(secret, 1) == "" {
		t.Fatal("生成的种子应能算出非空口令")
	}
}

func TestMFASettingKeyIsNamespaced(t *testing.T) {
	if got := mfaSettingKey("admin"); got != "UserMFA:admin" {
		t.Fatalf("mfaSettingKey(admin) = %q", got)
	}
}

func TestReadMFAInputSourcesAndPrecedence(t *testing.T) {
	// 只从 query 取。
	request := httptest.NewRequest("POST", "/api/v1/mfa/verify?username=admin&code=287082", nil)
	input := readMFAInput(request)
	if input.Username != "admin" || input.Code != "287082" {
		t.Fatalf("query 解析失败: %+v", input)
	}

	// 只从 body 取，并去空格。
	request = httptest.NewRequest("POST", "/api/v1/mfa/verify", strings.NewReader(`{"username":"  bob  ","code":" 123456 "}`))
	input = readMFAInput(request)
	if input.Username != "bob" || input.Code != "123456" {
		t.Fatalf("body 解析失败: %+v", input)
	}

	// query 与 body 同时存在时以 query 为准。
	request = httptest.NewRequest("POST", "/api/v1/mfa/verify?username=admin&code=111111", strings.NewReader(`{"username":"bob","code":"222222"}`))
	input = readMFAInput(request)
	if input.Username != "admin" || input.Code != "111111" {
		t.Fatalf("query 应优先于 body: %+v", input)
	}

	// body 不是合法 JSON 时不能 500，只是拿不到 body 字段。
	request = httptest.NewRequest("POST", "/api/v1/mfa/verify?code=287082", strings.NewReader("not-json"))
	input = readMFAInput(request)
	if input.Code != "287082" || input.Username != "" {
		t.Fatalf("非法 body 应被忽略: %+v", input)
	}
}

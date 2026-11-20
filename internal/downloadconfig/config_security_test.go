package downloadconfig

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// secQB 构造一个「运行期可用」的下载器：可用性要求 Enabled + 受支持类型 + 合法 UUID。
// 运行期选择不看凭据，凭据只影响真正连下载器时的认证方式。
func secQB(name string, enabled, isDefault bool, config map[string]any) Downloader {
	return Downloader{
		ID: uuid.NewString(), Name: name, Type: "qbittorrent",
		Enabled: enabled, Default: isDefault, Config: config,
	}
}

func TestSecRuntimeDerivesAPIKeyAuthentication(t *testing.T) {
	runtime, err := secQB("qb", true, true, map[string]any{
		"host":   " http://qb:8080 ",
		"apikey": "secret-key",
	}).Runtime()
	if err != nil {
		t.Fatalf("Runtime 失败: %v", err)
	}
	if runtime.Host != "http://qb:8080" {
		t.Fatalf("host 未去空格: %q", runtime.Host)
	}
	auth := runtime.Authentication()
	if auth.Method() != AuthAPIKey {
		t.Fatalf("认证方式 = %q，期望 %q", auth.Method(), AuthAPIKey)
	}
	if auth.APIKey() != "secret-key" {
		t.Fatal("api key 未带上")
	}
	if auth.Username() != "" || auth.Password() != "" {
		t.Fatal("有 api key 时不应同时带用户名密码")
	}
}

func TestSecRuntimeDerivesUsernamePasswordAuthentication(t *testing.T) {
	runtime, err := secQB("qb", true, false, map[string]any{
		"host":     "http://qb:8080",
		"username": "admin",
		"password": "pass",
	}).Runtime()
	if err != nil {
		t.Fatalf("Runtime 失败: %v", err)
	}
	auth := runtime.Authentication()
	if auth.Method() != AuthUsernamePassword {
		t.Fatalf("认证方式 = %q，期望 %q", auth.Method(), AuthUsernamePassword)
	}
	if auth.Username() != "admin" || auth.Password() != "pass" {
		t.Fatal("用户名密码未被投影")
	}
	if auth.APIKey() != "" {
		t.Fatal("没有 api key 时不应有值")
	}
}

func TestSecRuntimeWithoutCredentialsIsAuthNone(t *testing.T) {
	runtime, err := secQB("qb", true, false, map[string]any{"host": "http://qb:8080"}).Runtime()
	if err != nil {
		t.Fatalf("Runtime 失败: %v", err)
	}
	if runtime.Authentication().Method() != AuthNone {
		t.Fatalf("认证方式 = %q", runtime.Authentication().Method())
	}
}

func TestSecRuntimeRejectsNonStringHost(t *testing.T) {
	if _, err := secQB("qb", true, false, map[string]any{"host": 8080}).Runtime(); err == nil {
		t.Fatal("host 不是字符串时应当报错")
	}
}

func TestSecForceResumeIsProjected(t *testing.T) {
	runtime, err := secQB("qb", true, false, map[string]any{
		"host":         "http://qb:8080",
		"force_resume": true,
	}).Runtime()
	if err != nil {
		t.Fatalf("Runtime 失败: %v", err)
	}
	if !runtime.ForceResume() {
		t.Fatal("force_resume 未投影")
	}
}

func TestSecLogValueNeverLeaksSecrets(t *testing.T) {
	settings := secQB("qb", true, true, map[string]any{
		"host":     "http://user:secret-pass@qb:8080/rpc?token=abc",
		"username": "admin",
		"password": "super-secret",
		"apikey":   "api-key-value",
	})
	text := fmt.Sprint(settings.LogValue())
	for _, secret := range []string{"super-secret", "api-key-value", "token=abc"} {
		if strings.Contains(text, secret) {
			t.Fatalf("日志投影泄露了 %q: %s", secret, text)
		}
	}
	if !strings.Contains(text, "qb") {
		t.Fatalf("日志投影丢了下载器名字: %s", text)
	}
}

func TestSecAuthenticationStringIsMethodOnly(t *testing.T) {
	auth := Authentication{method: AuthUsernamePassword, username: "u", password: "p", apiKey: "k"}
	if got := auth.String(); got != string(AuthUsernamePassword) {
		t.Fatalf("String() = %q，不应包含凭据", got)
	}
	logged := fmt.Sprint(auth.LogValue())
	if strings.Contains(logged, "p") && strings.Contains(logged, "u") && strings.Contains(logged, "k") {
		t.Fatalf("认证日志值泄露凭据: %s", logged)
	}
}

func TestSecDecodeEmptyArray(t *testing.T) {
	settings, err := Decode(json.RawMessage(`[]`))
	if err != nil {
		t.Fatalf("解析空数组失败: %v", err)
	}
	if len(settings) != 0 {
		t.Fatalf("期望 0 个下载器，实际 %d", len(settings))
	}
}

func TestSecDecodeRejectsInvalidJSON(t *testing.T) {
	if _, err := Decode(json.RawMessage(`{`)); err == nil {
		t.Fatal("非法 JSON 应当报错")
	}
}

func TestSecNormalizeAssignsStableID(t *testing.T) {
	settings := []Downloader{secQB("qb", true, false, map[string]any{"host": "http://qb:8080"})}
	changed, err := Normalize(settings)
	if err != nil {
		t.Fatalf("Normalize 失败: %v", err)
	}
	if !changed {
		t.Fatal("补齐 ID 应算作改动")
	}
	if settings[0].ID == "" {
		t.Fatal("Normalize 后应当有稳定 ID")
	}
	before := settings[0].ID
	changedAgain, err := Normalize(settings)
	if err != nil {
		t.Fatalf("二次 Normalize 失败: %v", err)
	}
	if changedAgain {
		t.Fatal("二次规范化不应再改动")
	}
	if settings[0].ID != before {
		t.Fatalf("ID 被改写: %q -> %q", before, settings[0].ID)
	}
}

func TestSecSelectEnabledDownloaderPrefersDefault(t *testing.T) {
	settings := []Downloader{
		secQB("secondary", true, false, map[string]any{"host": "http://b:8080", "username": "u", "password": "p"}),
		secQB("primary", true, true, map[string]any{"host": "http://a:8080", "username": "u", "password": "p"}),
	}
	selected, ok := SelectEnabledDownloader(settings, "")
	if !ok {
		t.Fatal("应当能选出默认下载器")
	}
	if selected.Name != "primary" {
		t.Fatalf("选中 = %q，期望默认下载器 primary", selected.Name)
	}
}

func TestSecSelectEnabledDownloaderHonoursSelector(t *testing.T) {
	settings := []Downloader{
		secQB("primary", true, true, map[string]any{"host": "http://a:8080", "username": "u", "password": "p"}),
		secQB("secondary", true, false, map[string]any{"host": "http://b:8080", "username": "u", "password": "p"}),
	}
	selected, ok := SelectEnabledDownloader(settings, "secondary")
	if !ok || selected.Name != "secondary" {
		t.Fatalf("按名字选择失败: %q ok=%v", selected.Name, ok)
	}
	if _, ok := SelectEnabledDownloader(settings, "不存在"); ok {
		t.Fatal("未知名字不应选中任何下载器")
	}
}

func TestSecSelectEnabledDownloaderIgnoresDisabled(t *testing.T) {
	settings := []Downloader{secQB("off", false, true, map[string]any{"host": "http://a:8080", "username": "u", "password": "p"})}
	if _, ok := SelectEnabledDownloader(settings, ""); ok {
		t.Fatal("未启用的下载器不应被选中")
	}
}

// 可用性看的是「运行期身份」：Enabled + 受支持类型 + 合法 UUID，与凭据无关。
func TestSecSelectEnabledDownloaderRejectsMissingStableID(t *testing.T) {
	settings := []Downloader{{Name: "noid", Type: "qbittorrent", Enabled: true, Default: true,
		Config: map[string]any{"host": "http://a:8080"}}}
	if _, ok := SelectEnabledDownloader(settings, ""); ok {
		t.Fatal("缺少稳定 UUID 的下载器不应被选中")
	}
}

func TestSecSelectEnabledDownloaderRejectsUnsupportedType(t *testing.T) {
	unsupported := secQB("weird", true, true, map[string]any{"host": "http://a:8080"})
	unsupported.Type = "deluge"
	if _, ok := SelectEnabledDownloader([]Downloader{unsupported}, ""); ok {
		t.Fatal("未支持的下载器类型不应被选中")
	}
}

func TestSecAllowedLocalSavePathRejectsWhenNoDirectories(t *testing.T) {
	if _, ok := AllowedLocalSavePath(nil, "/downloads/movie"); ok {
		t.Fatal("没有配置目录时不应允许任何本地路径")
	}
}

package app

import (
	"strings"
	"testing"

	"github.com/moviepilot-go/moviepilot-go/internal/database"
	"github.com/moviepilot-go/moviepilot-go/internal/mediaconfig"
)

func TestStringConfigValue(t *testing.T) {
	config := map[string]any{
		"Cookie": "  abc=1  ",
		"rss":    "https://example.org/rss",
		"num":    42,
	}
	if got := stringConfigValue(config, "cookie"); got != "abc=1" {
		t.Fatalf("大小写不敏感取字符串失败: %q", got)
	}
	if got := stringConfigValue(config, "rss"); got != "https://example.org/rss" {
		t.Fatalf("rss = %q", got)
	}
	if got := stringConfigValue(config, "num"); got != "" {
		t.Fatalf("非字符串应返回空串，实际 %q", got)
	}
	if got := stringConfigValue(config, "missing"); got != "" {
		t.Fatalf("缺失键应返回空串，实际 %q", got)
	}
	if got := stringConfigValue(nil, "cookie"); got != "" {
		t.Fatalf("nil map 应返回空串，实际 %q", got)
	}
}

func TestConfigBoolValue(t *testing.T) {
	cases := []struct {
		value any
		want  bool
	}{
		{true, true},
		{false, false},
		{1, true},
		{0, false},
		{int64(2), true},
		{float64(0), false},
		{"1", true},
		{"true", true},
		{"TRUE", true},
		{"yes", false},
		{"", false},
	}
	for _, tc := range cases {
		config := map[string]any{"Public": tc.value}
		if got := configBoolValue(config, "public"); got != tc.want {
			t.Fatalf("configBoolValue(%#v) = %v，期望 %v", tc.value, got, tc.want)
		}
	}
	if configBoolValue(nil, "public") {
		t.Fatal("nil map 应为 false")
	}
}

func TestConfigIntValue(t *testing.T) {
	cases := []struct {
		value any
		want  int
	}{
		{30, 30},
		{int64(45), 45},
		{float64(60.9), 60},
		{"90", 90},
		{"  120  ", 120},
		{"abc", 15},
		{true, 15},
	}
	for _, tc := range cases {
		config := map[string]any{"timeout": tc.value}
		if got := configIntValue(config, "timeout", 15); got != tc.want {
			t.Fatalf("configIntValue(%#v) = %d，期望 %d", tc.value, got, tc.want)
		}
	}
	if got := configIntValue(nil, "timeout", 15); got != 15 {
		t.Fatalf("nil map 应回落到默认值，实际 %d", got)
	}
}

func TestCloneCookieCloudConfigIsShallowCopy(t *testing.T) {
	if cloneCookieCloudConfig(nil) != nil {
		t.Fatal("nil 应保持 nil")
	}
	source := map[string]any{"cookie": "a=1"}
	clone := cloneCookieCloudConfig(source)
	clone["cookie"] = "b=2"
	if source["cookie"] != "a=1" {
		t.Fatal("修改克隆不应影响原 map")
	}
}

func TestDeleteCookieCloudConfigKeyIgnoresCase(t *testing.T) {
	config := map[string]any{"Cookie": "a=1", "rss": "x"}
	deleteCookieCloudConfigKey(config, "cookie")
	if _, ok := config["Cookie"]; ok {
		t.Fatal("大小写不同的同名键应被删除")
	}
	if _, ok := config["rss"]; !ok {
		t.Fatal("其它键不应被删除")
	}
	deleteCookieCloudConfigKey(nil, "cookie") // 不应 panic
}

func TestSiteMatchesCookieDomainUsesMainDomain(t *testing.T) {
	site := database.Site{Name: "pt", URL: "https://pt.example.org", Domain: "pt.example.org"}
	if !siteMatchesCookieDomain(site, "example.org") {
		t.Fatal("子域应归一到主域后匹配")
	}
	if siteMatchesCookieDomain(site, "other.org") {
		t.Fatal("不同主域不应匹配")
	}
}

func TestCookieCloudSiteFromDatabase(t *testing.T) {
	site := database.Site{
		ID: 3, Name: "pt", URL: "https://pt.example.org", Domain: "example.org",
		Adapter: "declarative", Enabled: true,
		Config: map[string]any{
			"cookie": "a=1", "rss": "https://pt.example.org/rss", "public": true,
			"ua": "ua-string", "proxy": 1, "timeout": 30,
		},
	}
	converted := cookieCloudSiteFromDatabase(site)
	if converted.ID != 3 || converted.Name != "pt" || !converted.Active {
		t.Fatalf("基础字段映射错误: %+v", converted)
	}
	if converted.Cookie != "a=1" || converted.RSS == "" || !converted.Public || !converted.Proxy {
		t.Fatalf("配置字段映射错误: %+v", converted)
	}
	if converted.Timeout != 30 {
		t.Fatalf("timeout = %d，期望 30", converted.Timeout)
	}

	// 缺省 timeout 应当是 15 秒，而不是 0（0 会被当成无超时）。
	defaults := cookieCloudSiteFromDatabase(database.Site{Config: map[string]any{}})
	if defaults.Timeout != 15 {
		t.Fatalf("缺省 timeout = %d，期望 15", defaults.Timeout)
	}
}

func TestWorkerCountIfEnabled(t *testing.T) {
	if got := workerCountIfEnabled(false, 4); got != 0 {
		t.Fatalf("未启用时应为 0，实际 %d", got)
	}
	if got := workerCountIfEnabled(true, 4); got != 4 {
		t.Fatalf("启用时应原样返回，实际 %d", got)
	}
}

func TestTmdbProviderID(t *testing.T) {
	if got := tmdbProviderID(603); got != "tmdb.603" {
		t.Fatalf("tmdbProviderID(603) = %q", got)
	}
	if got := tmdbProviderID(0); got != "" {
		t.Fatalf("0 应返回空串，实际 %q", got)
	}
	if got := tmdbProviderID(-1); got != "" {
		t.Fatalf("负数应返回空串，实际 %q", got)
	}
}

func TestIsVideoTarget(t *testing.T) {
	yes := []string{"a.mkv", "B.MP4", "c.avi", "d.mov", "e.ts", "f.m2ts", "g.wmv", "h.flv", "i.webm", "j.iso"}
	for _, path := range yes {
		if !isVideoTarget(path) {
			t.Fatalf("%q 应被识别为视频", path)
		}
	}
	no := []string{"a.nfo", "b.srt", "c.ass", "noext", "d.jpg"}
	for _, path := range no {
		if isVideoTarget(path) {
			t.Fatalf("%q 不应被识别为视频", path)
		}
	}
}

func TestEmbyAuthorizationCarriesDeviceID(t *testing.T) {
	header := embyAuthorization("device-123")
	if !strings.Contains(header, `DeviceId="device-123"`) {
		t.Fatalf("授权头缺少 DeviceId: %s", header)
	}
	if !strings.Contains(header, "MoviePilot-Go") {
		t.Fatalf("授权头缺少客户端标识: %s", header)
	}
}

func TestMediaServerEndpointFingerprintIsStableAndDiscriminating(t *testing.T) {
	base := mediaconfig.Server{Type: "emby", Config: mediaconfig.Config{Host: "http://emby:8096"}}
	first := mediaServerEndpointFingerprint(base)
	if first != mediaServerEndpointFingerprint(base) {
		t.Fatal("相同配置应得到相同指纹")
	}
	caseVariant := mediaconfig.Server{Type: "EMBY", Config: mediaconfig.Config{Host: "http://emby:8096"}}
	if first != mediaServerEndpointFingerprint(caseVariant) {
		t.Fatal("类型大小写不应影响指纹")
	}
	other := mediaconfig.Server{Type: "emby", Config: mediaconfig.Config{Host: "http://other:8096"}}
	if first == mediaServerEndpointFingerprint(other) {
		t.Fatal("不同主机应得到不同指纹")
	}
}

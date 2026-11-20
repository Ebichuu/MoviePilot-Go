package app

import (
	"testing"

	"github.com/moviepilot-go/moviepilot-go/internal/config"
	"github.com/moviepilot-go/moviepilot-go/internal/database"
)

func TestNewCookieCloudSyncerRequiresConfiguration(t *testing.T) {
	store := &database.Store{}
	cfg := config.Config{CookieCloudKey: "key", CookieCloudPassword: "pass"}

	if newCookieCloudSyncer(cfg, nil, nil, nil, nil, nil, nil) != nil {
		t.Fatal("没有站点存储时不应创建同步器")
	}
	if newCookieCloudSyncer(config.Config{CookieCloudPassword: "pass"}, store, nil, nil, nil, nil, nil) != nil {
		t.Fatal("缺少 key 时不应创建同步器")
	}
	if newCookieCloudSyncer(config.Config{CookieCloudKey: "key"}, store, nil, nil, nil, nil, nil) != nil {
		t.Fatal("缺少 password 时不应创建同步器")
	}

	syncer := newCookieCloudSyncer(cfg, store, nil, nil, nil, nil, nil)
	if syncer == nil {
		t.Fatal("配置齐全时应创建同步器")
	}
	if syncer.helper == nil {
		t.Fatal("同步器必须持有 helper")
	}
	if syncer.store != store {
		t.Fatal("同步器应保留站点存储")
	}
}

func TestCookieCloudSiteFromDatabaseMapsFields(t *testing.T) {
	site := database.Site{
		ID:      7,
		Name:    "示例站",
		URL:     "https://www.example.com",
		Domain:  "example.com",
		Adapter: "mteam",
		Enabled: true,
		Config: map[string]any{
			"cookie":  "  uid=1  ",
			"rss":     "https://www.example.com/rss",
			"public":  true,
			"ua":      "Mozilla",
			"proxy":   1,
			"timeout": 30,
		},
	}
	converted := cookieCloudSiteFromDatabase(site)

	if converted.ID != 7 || converted.Name != "示例站" || converted.Domain != "example.com" || converted.Adapter != "mteam" {
		t.Fatalf("基础字段映射错误: %+v", converted)
	}
	// Active 对应 Enabled，不是「有 cookie 就算启用」。
	if !converted.Active {
		t.Fatal("Active 应跟随 Enabled")
	}
	if converted.Cookie != "uid=1" {
		t.Fatalf("cookie 应去空格，实际 %q", converted.Cookie)
	}
	if converted.RSS != "https://www.example.com/rss" || converted.UA != "Mozilla" {
		t.Fatalf("rss/ua 映射错误: %+v", converted)
	}
	if !converted.Public || !converted.Proxy {
		t.Fatalf("布尔字段映射错误: %+v", converted)
	}
	if converted.Timeout != 30 {
		t.Fatalf("timeout = %d", converted.Timeout)
	}
	// Config 必须是副本：否则改转换结果会污染数据库对象的配置。
	if converted.Config == nil {
		t.Fatal("Config 不应为空")
	}
	converted.Config["cookie"] = "changed"
	if site.Config["cookie"] != "  uid=1  " {
		t.Fatal("Config 未做深拷贝")
	}
}

func TestCookieCloudSiteFromDatabaseDefaults(t *testing.T) {
	converted := cookieCloudSiteFromDatabase(database.Site{Name: "裸站"})
	if converted.Timeout != 15 {
		t.Fatalf("缺省超时应为 15，实际 %d", converted.Timeout)
	}
	if converted.Cookie != "" || converted.Active {
		t.Fatalf("缺省字段应留空: %+v", converted)
	}
	if converted.Config != nil {
		t.Fatalf("nil 配置应保持 nil，实际 %+v", converted.Config)
	}
}

func TestSiteMatchesCookieDomain(t *testing.T) {
	site := database.Site{Name: "示例站", Domain: "example.com", URL: "https://www.example.com", Adapter: "mteam"}

	if !siteMatchesCookieDomain(site, "example.com") {
		t.Fatal("应匹配站点 domain")
	}
	if siteMatchesCookieDomain(site, "other.example") {
		t.Fatal("不相关域名不应匹配")
	}
	// 空域名不应匹配任何站点（否则空 cookie 域会命中所有站点）。
	if siteMatchesCookieDomain(site, "") {
		t.Fatal("空域名不应匹配")
	}
}

func TestCloneCookieCloudConfig(t *testing.T) {
	if cloneCookieCloudConfig(nil) != nil {
		t.Fatal("nil 应保持 nil")
	}
	source := map[string]any{"cookie": "a", "timeout": 15}
	clone := cloneCookieCloudConfig(source)
	if len(clone) != len(source) {
		t.Fatalf("克隆长度不一致: %+v", clone)
	}
	clone["cookie"] = "b"
	if source["cookie"] != "a" {
		t.Fatal("克隆必须与源隔离")
	}
}

func TestDeleteCookieCloudConfigKey(t *testing.T) {
	cfg := map[string]any{"Cookie": 1, "COOKIE": 2, "rss": 3}
	deleteCookieCloudConfigKey(cfg, "cookie")
	if len(cfg) != 1 {
		t.Fatalf("大小写不同的同名键都应被删除: %+v", cfg)
	}
	if _, ok := cfg["rss"]; !ok {
		t.Fatal("无关键不应被删除")
	}
	// 带空格的 key 也要能删掉。
	cfg = map[string]any{" timeout ": 1}
	deleteCookieCloudConfigKey(cfg, "timeout")
	if len(cfg) != 0 {
		t.Fatalf("带空格的键应能被删除: %+v", cfg)
	}
}

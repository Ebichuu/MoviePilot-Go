package indexer

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestSiteDirectoryDomainKeys(t *testing.T) {
	cases := map[string][]string{
		"":                                 nil,
		"   ":                              nil,
		"Example.COM":                      {"example.com"},
		"https://sub.example.com/path?q=1": {"sub.example.com", "example.com"},
		"a.b.example.com":                  {"a.b.example.com", "example.com"},
		".example.com":                     {"example.com"},
		"https://example.com.":             {"example.com"},
		"http://[::1]:8080":                {"::1"},
		"https://":                         nil,
		"example.co.uk":                    {"example.co.uk", "co.uk"},
	}
	for raw, want := range cases {
		if got := siteDirectoryDomainKeys(raw); !reflect.DeepEqual(got, want) {
			t.Fatalf("siteDirectoryDomainKeys(%q) = %#v, want %#v", raw, got, want)
		}
	}
}

func TestSiteDirectoryLookup(t *testing.T) {
	directory := NewSiteDirectory([]SiteDirectoryEntry{{
		Name: "示例站", Domain: "example.com", Adapter: "nexusphp",
		ExtDomains: []string{"mirror.example.org"},
		Config:     map[string]any{"cookie": "x"},
		Category:   []any{map[string]any{"id": "movie"}},
	}})

	for _, domain := range []string{"example.com", "EXAMPLE.COM", "https://example.com/login.php", "mirror.example.org"} {
		entry, ok := directory.Lookup(domain)
		if !ok || entry.Name != "示例站" {
			t.Fatalf("Lookup(%q) = (%#v, %v)", domain, entry, ok)
		}
	}

	if entry, ok := directory.Lookup("unknown.example.net"); ok || entry.Name != "" {
		t.Fatalf("未知域名 = (%#v, %v)", entry, ok)
	}
	var nilDirectory *SiteDirectory
	if _, ok := nilDirectory.Lookup("example.com"); ok {
		t.Fatal("nil 目录不应命中")
	}

	// 返回的是深拷贝：改写不会污染目录内部状态。
	entry, _ := directory.Lookup("example.com")
	entry.ExtDomains[0] = "hacked.example"
	entry.Config["cookie"] = "hacked"
	again, _ := directory.Lookup("example.com")
	if again.ExtDomains[0] != "mirror.example.org" || again.Config["cookie"] != "x" {
		t.Fatalf("目录内部状态被改写: %#v", again)
	}
}

func TestNewSiteDirectoryFirstEntryWins(t *testing.T) {
	directory := NewSiteDirectory([]SiteDirectoryEntry{
		{Name: "第一个", Domain: "example.com"},
		{Name: "第二个", Domain: "example.com"},
	})
	entry, ok := directory.Lookup("example.com")
	if !ok || entry.Name != "第一个" {
		t.Fatalf("重复域名 = (%#v, %v)", entry, ok)
	}
}

func TestSiteDirectoryEntryFromConfig(t *testing.T) {
	config := map[string]any{
		"Domain":      " config.example.com ",
		"adapter":     " nexusphp ",
		"ext_domains": "a.example.com, b.example.com;A.example.com",
		"public":      true,
		"category":    []any{map[string]any{"id": "tv"}},
	}
	entry := SiteDirectoryEntryFromConfig(" 示例站 ", "stored.example.com", "stored-adapter", config)
	if entry.Name != "示例站" || entry.Domain != "config.example.com" || entry.Adapter != "nexusphp" {
		t.Fatalf("基础字段 = %#v", entry)
	}
	if !reflect.DeepEqual(entry.ExtDomains, []string{"a.example.com", "b.example.com"}) {
		t.Fatalf("扩展域名 = %#v", entry.ExtDomains)
	}
	if !entry.Public {
		t.Fatal("public 应为 true")
	}
	if _, ok := entry.Category.([]any); !ok {
		t.Fatalf("category = %#v", entry.Category)
	}

	// 修改入参不影响已构造的条目。
	config["domain"] = "changed.example.com"
	if entry.Config["Domain"] != " config.example.com " {
		t.Fatalf("配置未被深拷贝: %#v", entry.Config)
	}

	// adapter 依次回落到 parser 与存储值；domain 回落到存储域名。
	parserEntry := SiteDirectoryEntryFromConfig("n", "stored.example.com", "stored-adapter", map[string]any{"parser": "unit3d"})
	if parserEntry.Adapter != "unit3d" || parserEntry.Domain != "stored.example.com" {
		t.Fatalf("parser 回落 = %#v", parserEntry)
	}
	storedEntry := SiteDirectoryEntryFromConfig("n", "stored.example.com", " stored-adapter ", nil)
	if storedEntry.Adapter != "stored-adapter" || storedEntry.Domain != "stored.example.com" || storedEntry.Public {
		t.Fatalf("存储值回落 = %#v", storedEntry)
	}
	if storedEntry.ExtDomains != nil && len(storedEntry.ExtDomains) != 0 {
		t.Fatalf("空配置扩展域名 = %#v", storedEntry.ExtDomains)
	}
	if storedEntry.Config != nil {
		t.Fatalf("nil 配置应保持 nil: %#v", storedEntry.Config)
	}
}

func TestDirectoryConfigHelpers(t *testing.T) {
	config := map[string]any{
		"Domain":      " example.com ",
		"ext_domains": []any{"a.example.com", " ", "b.example.com", 42},
		"int_bool":    1,
		"int64_bool":  int64(0),
		"float_bool":  float64(1),
		"str_bool":    " TRUE ",
		"one_bool":    "1",
		"no_bool":     "yes",
		"bytes":       []byte("text"),
		"string_list": []string{"x.example.com", "y.example.com"},
	}

	if got := directoryConfigString(config, "domain"); got != "example.com" {
		t.Fatalf("directoryConfigString = %q", got)
	}
	if got := directoryConfigString(config, "missing"); got != "" {
		t.Fatalf("缺失键 = %q", got)
	}
	if got := directoryString(config["bytes"]); got != "text" {
		t.Fatalf("[]byte = %q", got)
	}
	if got := directoryString(42); got != "" {
		t.Fatalf("非字符串 = %q", got)
	}
	if !directoryBool(config["int_bool"]) || directoryBool(config["int64_bool"]) ||
		!directoryBool(config["float_bool"]) || !directoryBool(config["str_bool"]) ||
		!directoryBool(config["one_bool"]) || directoryBool(config["no_bool"]) || directoryBool(nil) {
		t.Fatal("directoryBool 分支判定错误")
	}
	if got := directoryStringList(config["ext_domains"]); !reflect.DeepEqual(got, []string{"a.example.com", "b.example.com"}) {
		t.Fatalf("[]any 列表 = %#v", got)
	}
	if got := directoryStringList(config["string_list"]); !reflect.DeepEqual(got, []string{"x.example.com", "y.example.com"}) {
		t.Fatalf("[]string 列表 = %#v", got)
	}
	if got := directoryStringList("b.com,a.com; b.com"); !reflect.DeepEqual(got, []string{"b.com", "a.com"}) {
		t.Fatalf("字符串列表 = %#v", got)
	}
	if got := directoryStringList(nil); len(got) != 0 {
		t.Fatalf("nil 列表 = %#v", got)
	}
}

func TestSiteCategories(t *testing.T) {
	directory := NewSiteDirectory([]SiteDirectoryEntry{
		{Name: "嵌套", Domain: "nested.example", Category: map[string]any{
			"movies": []any{map[string]any{"id": "movie", "name": "电影"}},
			"tv":     map[string]any{"id": "tv", "name": "剧集"},
		}},
		{Name: "字符串映射", Domain: "stringmap.example", Category: map[string]string{"id": "music"}},
		{Name: "重复", Domain: "dup.example", Category: []any{
			map[string]any{"name": "电影"},
			map[string]any{"name": " 电影 "},
		}},
		{Name: "空", Domain: "empty.example"},
	})

	if _, _, err := directory.SiteCategories(nil, "nested.example"); err == nil {
		t.Fatal("nil ctx 应报错")
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := directory.SiteCategories(canceled, "nested.example"); !errors.Is(err, context.Canceled) {
		t.Fatalf("已取消 ctx = %v", err)
	}

	categories, found, err := directory.SiteCategories(t.Context(), "nested.example")
	if err != nil || !found || len(categories) != 2 {
		t.Fatalf("嵌套分类 = (%#v, %v, %v)", categories, found, err)
	}

	categories, found, err = directory.SiteCategories(t.Context(), "stringmap.example")
	if err != nil || !found || len(categories) != 1 || categories[0]["id"] != "music" {
		t.Fatalf("字符串映射分类 = (%#v, %v, %v)", categories, found, err)
	}

	categories, found, err = directory.SiteCategories(t.Context(), "dup.example")
	if err != nil || !found || len(categories) != 1 {
		t.Fatalf("去重分类 = (%#v, %v, %v)", categories, found, err)
	}

	categories, found, err = directory.SiteCategories(t.Context(), "empty.example")
	if err != nil || !found || len(categories) != 0 {
		t.Fatalf("空分类 = (%#v, %v, %v)", categories, found, err)
	}

	categories, found, err = directory.SiteCategories(t.Context(), "unknown.example")
	if err != nil || found || len(categories) != 0 {
		t.Fatalf("未知站点 = (%#v, %v, %v)", categories, found, err)
	}

	// 修改返回值不影响目录内的原始分类。
	categories, _, _ = directory.SiteCategories(t.Context(), "nested.example")
	categories[0]["id"] = "hacked"
	categories, _, _ = directory.SiteCategories(t.Context(), "nested.example")
	for _, category := range categories {
		if category["id"] == "hacked" {
			t.Fatalf("分类未被拷贝: %#v", categories)
		}
	}
}

func TestFlattenSiteCategories(t *testing.T) {
	if got := flattenSiteCategories(nil); len(got) != 0 {
		t.Fatalf("nil = %#v", got)
	}
	if got := flattenSiteCategories([]any{nil, "text", 42}); len(got) != 0 {
		t.Fatalf("非分类值 = %#v", got)
	}
	if got := flattenSiteCategories(map[string]any{"id": "movie", "name": "电影"}); len(got) != 1 {
		t.Fatalf("直接分类 = %#v", got)
	}
	if got := flattenSiteCategories([]map[string]any{{"cat": "tv"}, {"value": "anime"}}); len(got) != 2 {
		t.Fatalf("[]map 分类 = %#v", got)
	}
	// 同一 id 用不同大小写/空白写入时视为同一分类。
	got := flattenSiteCategories([]any{
		map[string]any{"id": "movie"},
		map[string]any{"ID": " movie "},
	})
	if len(got) != 1 {
		t.Fatalf("大小写去重 = %#v", got)
	}
}

func TestIsSiteCategoryAndIdentity(t *testing.T) {
	for _, value := range []map[string]any{
		{"id": 1}, {"name": "x"}, {"cat": "y"}, {"category": "z"}, {"value": "v"}, {" ID ": 1},
	} {
		if !isSiteCategory(value) {
			t.Fatalf("应判为分类: %#v", value)
		}
	}
	for _, value := range []map[string]any{{}, {"other": 1}, {"id2": 1}} {
		if isSiteCategory(value) {
			t.Fatalf("不应判为分类: %#v", value)
		}
	}

	if got := siteCategoryIdentity(map[string]any{"id": " movie "}); got != "id\x00movie" {
		t.Fatalf("id 身份 = %q", got)
	}
	if got := siteCategoryIdentity(map[string]any{"name": "电影"}); got != "name\x00电影" {
		t.Fatalf("name 身份 = %q", got)
	}
	// 没有任何已知键时按排序后的键值对生成身份。
	first := siteCategoryIdentity(map[string]any{"b": 2, "a": 1})
	second := siteCategoryIdentity(map[string]any{"a": 1, "b": 2})
	if first != second || first == "" {
		t.Fatalf("键值身份 = %q / %q", first, second)
	}
	if siteCategoryIdentity(map[string]any{"a": "1"}) == siteCategoryIdentity(map[string]any{"a": "2"}) {
		t.Fatal("不同值应有不同身份")
	}
	// 非字符串值不参与身份计算（directoryString 只认 string/[]byte）。
	if siteCategoryIdentity(map[string]any{"a": 1}) != siteCategoryIdentity(map[string]any{"a": 2}) {
		t.Fatal("非字符串值应归一为空")
	}
}

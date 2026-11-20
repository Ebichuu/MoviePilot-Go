package indexer

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestHaidanFactorsAndCategory(t *testing.T) {
	factors := map[string][2]float64{
		"2": {0, 1}, "3": {1, 2}, "4": {0, 2}, "5": {0.5, 1},
		"6": {0.5, 2}, "7": {0.3, 1}, "1": {1, 1}, "": {1, 1}, " 3 ": {1, 2},
	}
	for value, want := range factors {
		up, down := haidanFactors(value)
		if up != want[0] || down != want[1] {
			t.Fatalf("haidanFactors(%q) = (%v, %v), want %v", value, up, down, want)
		}
	}
	for value, want := range map[string]string{
		"401": "电影", "404": "电影", "405": "电影",
		"402": "电视剧", "403": "电视剧",
		"": "未知", "999": "未知",
	} {
		if got := haidanCategory(value); got != want {
			t.Fatalf("haidanCategory(%q) = %q, want %q", value, got, want)
		}
	}
}

func TestHDDolbyFactorsAndCategory(t *testing.T) {
	factors := map[int][2]float64{
		2: {0, 1}, 3: {1, 2}, 4: {1, 2}, 5: {0.5, 1},
		6: {1, 2}, 7: {0.3, 1}, 0: {1, 1}, 1: {1, 1}, 99: {1, 1},
	}
	for value, want := range factors {
		up, down := hddolbyFactors(value)
		if up != want[0] || down != want[1] {
			t.Fatalf("hddolbyFactors(%d) = (%v, %v), want %v", value, up, down, want)
		}
	}
	for value, want := range map[int]string{
		401: "电影", 404: "电影", 405: "电影",
		402: "电视剧", 403: "电视剧",
		0: "未知", 999: "未知",
	} {
		if got := hddolbyCategory(value); got != want {
			t.Fatalf("hddolbyCategory(%d) = %q, want %q", value, got, want)
		}
	}
}

func TestLookupJSONAndFieldHelpers(t *testing.T) {
	payload := map[string]any{
		"data": map[string]any{
			"items": []any{
				map[string]any{"title": " 第一项 "},
				map[string]any{"title": "第二项"},
			},
			"count": json.Number("12"),
			"blank": "   ",
		},
		"tags": "a, b|c;d",
	}

	cases := map[string]any{
		"data.count":     json.Number("12"),
		"data.items.0":   map[string]any{"title": " 第一项 "},
		".data.items.1.": map[string]any{"title": "第二项"},
		"data.items.9":   nil,
		"data.items.-1":  nil,
		"data.count.x":   nil,
		"missing":        nil,
		"":               payload,
	}
	for path, want := range cases {
		if got := lookupJSON(payload, path); !reflect.DeepEqual(got, want) {
			t.Fatalf("lookupJSON(%q) = %#v, want %#v", path, got, want)
		}
	}
	if got := lookupJSON("plain string", "a"); got != nil {
		t.Fatalf("非容器查找 = %#v", got)
	}

	field := func(selector string) Field { return Field{Selector: selector} }
	if got := jsonFieldString(payload, field("")); got != "" {
		t.Fatalf("空选择器 = %q", got)
	}
	if got := jsonFieldString(payload, field("data.items.0.title")); got != "第一项" {
		t.Fatalf("字符串裁剪 = %q", got)
	}
	if got := jsonFieldString(payload, field("data.count")); got != "12" {
		t.Fatalf("json.Number = %q", got)
	}
	if got := jsonFieldString(map[string]any{"n": 7}, field("n")); got != "7" {
		t.Fatalf("数字转字符串 = %q", got)
	}
	if got := jsonFieldString(payload, field("data.blank")); got != "" {
		t.Fatalf("空白字符串 = %q", got)
	}
	if got := jsonFieldString(payload, field("missing")); got != "" {
		t.Fatalf("缺失键 = %q", got)
	}

	if got := jsonFieldStrings(payload, field("")); got != nil {
		t.Fatalf("空选择器列表 = %#v", got)
	}
	if got := jsonFieldStrings(payload, field("tags")); !reflect.DeepEqual(got, []string{"a", "b", "c", "d"}) {
		t.Fatalf("分隔字符串 = %#v", got)
	}
	// 空串被跳过；nil 经 fmt.Sprint 变成 "<nil>"（与 V2 标签渲染一致，不做特例）。
	mixed := map[string]any{"list": []any{" 官方 ", 2, "", nil, "国语"}}
	if got := jsonFieldStrings(mixed, field("list")); !reflect.DeepEqual(got, []string{"官方", "2", "<nil>", "国语"}) {
		t.Fatalf("数组标签 = %#v", got)
	}
	// 标签数量上限 64。
	many := make([]any, 80)
	for index := range many {
		many[index] = "标签"
	}
	if got := jsonFieldStrings(map[string]any{"list": many}, field("list")); len(got) != 64 {
		t.Fatalf("标签上限 = %d", len(got))
	}
}

func TestSplitLabels(t *testing.T) {
	cases := map[string][]string{
		"a,b|c;d":   {"a", "b", "c", "d"},
		" a , , b ": {"a", "b"},
		"":          nil,
	}
	for value, want := range cases {
		if got := splitLabels(value); !reflect.DeepEqual(got, want) && !(len(got) == 0 && len(want) == 0) {
			t.Fatalf("splitLabels(%q) = %#v, want %#v", value, got, want)
		}
	}
	many := strings.Repeat("x,", 80)
	if got := splitLabels(many); len(got) != 64 {
		t.Fatalf("splitLabels 上限 = %d", len(got))
	}
}

func TestParseSizeAndInt(t *testing.T) {
	sizes := map[string]int64{
		"":                            0,
		"abc":                         0,
		"-5 MB":                       0,
		"1,024 MB":                    1024 << 20,
		"512KB":                       512 << 10,
		"3 MiB":                       3 << 20,
		"1 TB":                        1 << 40,
		"2.5 GB":                      int64(2.5 * float64(1<<30)),
		"999999999999999999999999 GB": 0,
		"unknownunit":                 0,
	}
	// 2.69GB 用浮点乘算，单独精确比较。
	gib := float64(1 << 30)
	if got := parseSize("2.69GB"); got != int64(2.69*gib) {
		t.Fatalf("parseSize(2.69GB) = %d", got)
	}
	for value, want := range sizes {
		if got := parseSize(value); got != want {
			t.Fatalf("parseSize(%q) = %d, want %d", value, got, want)
		}
	}

	integers := map[string]int{
		"1,024": 1024, "12.7": 12, "0": 0, "-3": 0, "abc": 0, "": 0,
		"1e400": 0,
	}
	for value, want := range integers {
		if got := parseInt(value); got != want {
			t.Fatalf("parseInt(%q) = %d, want %d", value, got, want)
		}
	}
	if got := parseInt("1e19"); got != 0 {
		t.Fatalf("parseInt(1e19) = %d（超过 int 上限应归零）", got)
	}
	if got := parseInt("1e10"); got != 10000000000 {
		t.Fatalf("parseInt(1e10) = %d", got)
	}
}

func TestRousiUserDataCodeZero(t *testing.T) {
	cases := map[string]bool{
		"json zero":   rousiUserDataCodeZero(json.Number("0")),
		"json one":    rousiUserDataCodeZero(json.Number("1")),
		"json bad":    rousiUserDataCodeZero(json.Number("x")),
		"float zero":  rousiUserDataCodeZero(float64(0)),
		"float one":   rousiUserDataCodeZero(float64(1)),
		"int zero":    rousiUserDataCodeZero(0),
		"int one":     rousiUserDataCodeZero(1),
		"int64 zero":  rousiUserDataCodeZero(int64(0)),
		"int64 one":   rousiUserDataCodeZero(int64(1)),
		"string zero": rousiUserDataCodeZero("0"),
		"nil":         rousiUserDataCodeZero(nil),
	}
	want := map[string]bool{
		"json zero": true, "json one": false, "json bad": false,
		"float zero": true, "float one": false,
		"int zero": true, "int one": false,
		"int64 zero": true, "int64 one": false,
		"string zero": false, "nil": false,
	}
	for name, got := range cases {
		if got != want[name] {
			t.Fatalf("%s = %v, want %v", name, got, want[name])
		}
	}
}

func TestNewAdapterFromConfigDispatch(t *testing.T) {
	client := newIndexerHTTPClient(t, 1)
	credentials := map[string]any{"cookie": "c", "apikey": "k"}

	resolvers := map[string]string{
		"yema":           "yema",
		"yemaspider":     "yema",
		"tnode":          "tnode",
		"torrentleech":   "torrentleech",
		"mtorrentspider": "mtorrent",
		"mteam":          "mtorrent",
		"sunnyptspider":  "sunnypt",
		"rousipro":       "rousi",
		"haidanspider":   "haidan",
		"hddolbyspider":  "hddolby",
	}
	for adapterName, kind := range resolvers {
		t.Run(adapterName, func(t *testing.T) {
			adapter, err := NewAdapterFromConfig(client, 3, "站点", "https://example.test", adapterName, credentials)
			if err != nil {
				t.Fatalf("NewAdapterFromConfig(%q): %v", adapterName, err)
			}
			resolver, ok := adapter.(interface{ ResolverKind() string })
			if !ok || resolver.ResolverKind() != kind {
				t.Fatalf("%q 解析器 = %#v", adapterName, adapter)
			}
		})
	}

	formats := map[string]map[string]any{
		"html":             {"result_selector": ".item"},
		"goquery":          {"result_selector": ".item"},
		"declarative-html": {"result_selector": ".item"},
		"rss":              {},
		"gofeed":           {},
		"declarative-rss":  {},
		"json":             {"result_path": "data"},
		"declarative-json": {"result_path": "data"},
		"declarative":      {"format": "html", "result_selector": ".item"},
	}
	for adapterName, config := range formats {
		t.Run(adapterName, func(t *testing.T) {
			adapter, err := NewAdapterFromConfig(client, 4, "声明式", "https://example.test", adapterName, config)
			if err != nil {
				t.Fatalf("NewAdapterFromConfig(%q): %v", adapterName, err)
			}
			if adapter.Name() != "声明式" || adapter.(SiteIDAdapter).SiteID() != 4 {
				t.Fatalf("装配结果 = %#v", adapter)
			}
		})
	}

	// 配置里同时给了 rss 时，HTML 适配器会挂上 RSS 能力。
	withRSS, err := NewAdapterFromConfig(client, 5, "带RSS", "https://example.test", "html", map[string]any{
		"result_selector": ".item", "rss": " /feed.xml ",
	})
	if err != nil {
		t.Fatalf("带 RSS 的 HTML 站点: %v", err)
	}
	if _, ok := withRSS.(*declarativeRSSAdapter); !ok {
		t.Fatalf("应包装为 RSS 适配器: %#v", withRSS)
	}

	invalidCases := []struct {
		name    string
		adapter string
		config  map[string]any
		want    string
	}{
		{"未知 parser", "unknownspider", nil, "暂不支持站点 parser"},
		{"declarative 缺 format", "declarative", map[string]any{}, "必须设置 format"},
		{"html 缺 result_selector", "html", map[string]any{}, "result_selector"},
		{"json 缺 result_path", "json", map[string]any{}, "result_path"},
		{"配置类型不符", "html", map[string]any{"result_selector": ".item", "id": "不是数字"}, "站点配置格式无效"},
		{"配置无法编码", "html", map[string]any{"result_selector": ".item", "fn": func() {}}, "站点配置无法编码"},
		{"配置过大", "html", map[string]any{"result_selector": ".item", "blob": strings.Repeat("a", 300<<10)}, "超过 256 KiB"},
	}
	for _, testCase := range invalidCases {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := NewAdapterFromConfig(client, 6, "坏站点", "https://example.test", testCase.adapter, testCase.config); err == nil ||
				!strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("错误 = %v，期望包含 %q", err, testCase.want)
			}
		})
	}

	if _, err := NewAdapterFromConfig(nil, 7, "无客户端", "https://example.test", "html", map[string]any{"result_selector": ".item"}); err == nil {
		t.Fatal("nil 客户端应报错")
	}
}

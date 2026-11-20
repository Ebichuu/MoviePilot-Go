package notificationtemplate

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParsePythonLiteralStrings(t *testing.T) {
	cases := map[string]string{
		`'plain'`:      "plain",
		`"double"`:     "double",
		`'a\nb'`:       "a\nb",
		`'a\rb'`:       "a\rb",
		`'a\tb'`:       "a\tb",
		`'a\bb'`:       "a\bb",
		`'a\fb'`:       "a\fb",
		`'a\vb'`:       "a\vb",
		`'a\ab'`:       "a\ab",
		`'a\\b'`:       `a\b`,
		`'a\'b'`:       "a'b",
		`"a\"b"`:       `a"b`,
		`'a\qb'`:       `a\qb`,
		`'\x41'`:       "A",
		`'\u4e2d'`:     "中",
		`'\U0001F600'`: "😀",
		`'''a'b'''`:    "a'b",
		`"""a"b"""`:    `a"b`,
		`'''line1
line2'''`: "line1\nline2",
		// 相邻字符串按 Python 规则拼接。
		`'a' 'b' "c"`:    "abc",
		`{'a': 'x' 'y'}`: "xy",
	}
	for source, want := range cases {
		got, err := parsePythonLiteral(source)
		if err != nil {
			t.Fatalf("解析 %q 失败: %v", source, err)
		}
		if dict, ok := got.(map[string]any); ok {
			got = dict["a"]
		}
		if got != want {
			t.Fatalf("解析 %q = %#v，期望 %#v", source, got, want)
		}
	}

	// 反斜杠换行是行连接符，直接丢弃。
	if got, err := parsePythonLiteral(`'a\
b'`); err != nil || got != "ab" {
		t.Fatalf("行连接 = (%#v, %v)", got, err)
	}
	if got, err := parsePythonLiteral("'a\\\r\nb'"); err != nil || got != "ab" {
		t.Fatalf("CRLF 行连接 = (%#v, %v)", got, err)
	}
}

func TestParsePythonLiteralStringErrors(t *testing.T) {
	cases := map[string]string{
		"未闭合":    `'abc`,
		"结尾反斜杠":  `'a\`,
		"十六进制过短": `'\x4'`,
		"十六进制非法": `'\xzz'`,
		"代理区码点":  `'\ud800'`,
		"码点越界":   `'\U00110000'`,
	}
	for name, source := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := parsePythonLiteral(source); err == nil {
				t.Fatalf("%s 应当报错: %q", name, source)
			}
		})
	}
}

func TestParsePythonLiteralContainers(t *testing.T) {
	got, err := parsePythonLiteral(`{'a': 1, 'b': [1, 2], 'c': (3, 4), 'd': {}}`)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	dict := got.(map[string]any)
	if list, ok := dict["b"].([]any); !ok || len(list) != 2 {
		t.Fatalf("b = %#v", dict["b"])
	}
	if tuple, ok := dict["c"].([]any); !ok || len(tuple) != 2 {
		t.Fatalf("c = %#v", dict["c"])
	}
	if nested, ok := dict["d"].(map[string]any); !ok || len(nested) != 0 {
		t.Fatalf("d = %#v", dict["d"])
	}

	for _, source := range []string{`{}`, `[]`, `()`, `[1,]`, `{'a': 1,}`} {
		if _, err := parsePythonLiteral(source); err != nil {
			t.Fatalf("空容器或尾逗号 %q 应接受: %v", source, err)
		}
	}

	// 注释在任意位置都被跳过。
	for _, source := range []string{
		"# 前导注释\n{'a': 1}",
		"{'a': 1, # 行内注释\n 'b': 2}",
		"{'a': 1} # 尾部注释",
	} {
		if _, err := parsePythonLiteral(source); err != nil {
			t.Fatalf("注释 %q 应接受: %v", source, err)
		}
	}
}

func TestParsePythonLiteralContainerErrors(t *testing.T) {
	cases := map[string]string{
		"键非字符串":    `{1: 2}`,
		"缺冒号":      `{'a' 1}`,
		"缺逗号":      `{'a': 1 'b': 2}`,
		"列表缺逗号":    `[1 2]`,
		"值位置非法字符":  `[,]`,
		"不支持的原子":   `foo`,
		"原子后有多余内容": `'a' 'b' c`,
		"未闭合字典":    `{'a': 'b'`,
		"未闭合列表":    `[1, 2`,
	}
	for name, source := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := parsePythonLiteral(source); err == nil {
				t.Fatalf("%s 应当报错: %q", name, source)
			}
		})
	}
}

func TestParsePythonLiteralAtoms(t *testing.T) {
	cases := map[string]any{
		`0x10`:  int64(16),
		`-7`:    int64(-7),
		`1.5`:   1.5,
		`-2.5`:  -2.5,
		`True`:  true,
		`False`: false,
		`None`:  nil,
	}
	for source, want := range cases {
		got, err := parsePythonLiteral(source)
		if err != nil {
			t.Fatalf("解析 %q 失败: %v", source, err)
		}
		if got != want {
			t.Fatalf("解析 %q = %#v (%T)，期望 %#v", source, got, got, want)
		}
	}
}

func TestDecodeTemplateValueBranches(t *testing.T) {
	if _, err := decodeTemplateValue(json.RawMessage("   ")); err == nil ||
		!strings.Contains(err.Error(), "empty template") {
		t.Fatalf("空模板 = %v", err)
	}

	// JSON 字符串里再套 JSON 对象。
	value, err := decodeTemplateValue(json.RawMessage(`"{\"title\":\"x\"}"`))
	if err != nil {
		t.Fatalf("嵌套 JSON 对象: %v", err)
	}
	if dict, ok := value.(map[string]any); !ok || dict["title"] != "x" {
		t.Fatalf("嵌套 JSON 对象 = %#v", value)
	}

	// JSON 字符串里是 Python 字面量。
	value, err = decodeTemplateValue(json.RawMessage(`"{'title': 'y'}"`))
	if err != nil {
		t.Fatalf("Python 字面量字符串: %v", err)
	}
	if dict, ok := value.(map[string]any); !ok || dict["title"] != "y" {
		t.Fatalf("Python 字面量字符串 = %#v", value)
	}

	// 完全不是 JSON：按 Python 字面量解析。
	value, err = decodeTemplateValue(json.RawMessage(`{'title': 'z'}`))
	if err != nil {
		t.Fatalf("裸 Python 字面量: %v", err)
	}
	if dict, ok := value.(map[string]any); !ok || dict["title"] != "z" {
		t.Fatalf("裸 Python 字面量 = %#v", value)
	}

	// 非字典的 Python 字面量、标量 JSON、坏字面量都拒绝。
	for _, raw := range []string{
		`['a']`,
		`123`,
		`true`,
		`[1, 2]`,
		`{'a': `,
	} {
		if _, err := decodeTemplateValue(json.RawMessage(raw)); err == nil {
			t.Fatalf("%s 应当被拒绝", raw)
		}
	}
}

package templating

import (
	"strings"
	"testing"
)

func renderExpectError(t *testing.T, source string, context map[string]any) error {
	t.Helper()
	out, err := Render(source, context)
	if err == nil {
		t.Fatalf("渲染 %q 应报错，实际输出 %q", source, out)
	}
	return err
}

func TestRenderElifAndElseBranches(t *testing.T) {
	source := `{% if first %}A{% elif second %}B{% else %}C{% endif %}`
	cases := []struct {
		name    string
		context map[string]any
		want    string
	}{
		{"命中第一支", map[string]any{"first": true, "second": true}, "A"},
		{"命中 elif", map[string]any{"first": false, "second": true}, "B"},
		{"落到 else", map[string]any{"first": false, "second": false}, "C"},
		{"变量缺失走 else", nil, "C"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if out := renderToString(t, source, testCase.context); out != testCase.want {
				t.Fatalf("输出 = %q，期望 %q", out, testCase.want)
			}
		})
	}
}

func TestRenderCollectionFilters(t *testing.T) {
	context := map[string]any{
		"items": []any{"甲", "乙", "丙"},
		"empty": []any{},
		"text":  "hello world",
		"dict":  map[string]any{"a": 1, "b": 2},
	}
	cases := map[string]string{
		`{{ items | first }}`:               "甲",
		`{{ items | last }}`:                "丙",
		`{{ items | count }}`:               "3",
		`{{ items | length }}`:              "3",
		`{{ text | title }}`:                "Hello World",
		`{{ dict | length }}`:               "2",
		`{{ empty | length }}`:              "0",
		`{{ "a-b-c" | replace("-", "+") }}`: "a+b+c",
		`{{ "12" | int }}`:                  "12",
		`{{ 3 | float }}`:                   "3",
		`{{ 12 | string }}`:                 "12",
	}
	for source, want := range cases {
		if out := renderToString(t, source, context); out != want {
			t.Fatalf("%s = %q，期望 %q", source, out, want)
		}
	}
	// 空集合取首尾是空串而不是报错。
	if out := renderToString(t, `[{{ empty | first }}]`, context); out != "[]" {
		t.Fatalf("空集合 first = %q", out)
	}
}

func TestRenderAttributeChains(t *testing.T) {
	context := map[string]any{
		"media":  map[string]any{"info": map[string]any{"title": "沙丘", "year": 2021}},
		"scalar": 42,
	}
	cases := map[string]string{
		`{{ media.info.title }}`: "沙丘",
		`{{ media.info.year }}`:  "2021",
		// 链上缺一段就渲染空串，不能报错中断整条通知。
		`[{{ media.info.missing }}]`: "[]",
		`[{{ media.absent.deep }}]`:  "[]",
		// 对标量做属性访问同样是空串。
		`[{{ scalar.field }}]`: "[]",
	}
	for source, want := range cases {
		if out := renderToString(t, source, context); out != want {
			t.Fatalf("%s = %q，期望 %q", source, out, want)
		}
	}
}

func TestRenderForOverMapAndString(t *testing.T) {
	// map 的迭代顺序必须确定（按键名升序），否则通知内容每次不同。
	if out := renderToString(t, `{% for key in mapping %}{{ key }},{% endfor %}`,
		map[string]any{"mapping": map[string]any{"b": 2, "a": 1, "c": 3}}); out != "a,b,c," {
		t.Fatalf("map 迭代 = %q", out)
	}
	if out := renderToString(t, `{% for character in word %}{{ character }}|{% endfor %}`,
		map[string]any{"word": "abc"}); out != "a|b|c|" {
		t.Fatalf("字符串迭代 = %q", out)
	}
	// 不可迭代的对象必须报错，而不是静默渲染空的循环体。
	renderExpectError(t, `{% for item in number %}{{ item }}{% endfor %}`, map[string]any{"number": 7})
	// 缺失变量按空集合处理：循环体不渲染。
	if out := renderToString(t, `[{% for item in nothing %}{{ item }}{% endfor %}]`, nil); out != "[]" {
		t.Fatalf("缺失可迭代对象 = %q", out)
	}
}

func TestRenderForUnpackingPairs(t *testing.T) {
	pairs := [][]any{{"甲", 1}, {"乙", 2}}
	out := renderToString(t, `{% for name, index in pairs %}{{ name }}={{ index }};{% endfor %}`,
		map[string]any{"pairs": pairs})
	if out != "甲=1;乙=2;" {
		t.Fatalf("解包结果 = %q", out)
	}
	// 元素不是序列、或元素长度不足时必须报错。
	renderExpectError(t, `{% for a, b in flat %}{{ a }}{% endfor %}`,
		map[string]any{"flat": []any{"x"}})
	renderExpectError(t, `{% for a, b in short %}{{ a }}{% endfor %}`,
		map[string]any{"short": [][]any{{"only"}}})
}

func TestRenderNestedLoopsAndConditions(t *testing.T) {
	source := `{% for group in groups %}{{ group.name }}:{% for item in group.items %}{% if item.enabled %}[{{ item.title }}]{% endif %}{% endfor %};{% endfor %}`
	context := map[string]any{"groups": []any{
		map[string]any{"name": "A", "items": []any{
			map[string]any{"title": "一号", "enabled": true},
			map[string]any{"title": "二号", "enabled": false},
		}},
		map[string]any{"name": "B", "items": []any{}},
	}}
	want := "A:[一号];B:;"
	if out := renderToString(t, source, context); out != want {
		t.Fatalf("嵌套输出 = %q，期望 %q", out, want)
	}
}

func TestRenderLoopScopeDoesNotLeak(t *testing.T) {
	// 循环变量属于循环作用域：循环结束后不应残留，更不能覆盖外层同名变量。
	out := renderToString(t, `{% for item in items %}{{ item }}{% endfor %}[{{ item }}]`,
		map[string]any{"items": []any{"x"}})
	if out != "x[]" {
		t.Fatalf("循环作用域 = %q", out)
	}
	out = renderToString(t, `{% for item in items %}{{ item }}{% endfor %}[{{ item }}]`,
		map[string]any{"items": []any{"x"}, "item": "外层"})
	if out != "x[外层]" {
		t.Fatalf("循环变量不应覆盖外层同名变量，实际 %q", out)
	}
}

func TestRenderParenthesizedCondition(t *testing.T) {
	context := map[string]any{"a": false, "b": true, "c": true}
	if out := renderToString(t, `{% if (a or b) and c %}yes{% else %}no{% endif %}`, context); out != "yes" {
		t.Fatalf("括号表达式 = %q", out)
	}
	if out := renderToString(t, `{% if a and (b or c) %}yes{% else %}no{% endif %}`, context); out != "no" {
		t.Fatalf("括号表达式 = %q", out)
	}
	renderExpectError(t, `{% if (a and b %}x{% endif %}`, context)
}

func TestRenderExpressionSyntaxErrors(t *testing.T) {
	renderExpectError(t, `{{ }}`, map[string]any{"a": 1})
	renderExpectError(t, `{{ media. }}`, map[string]any{"media": map[string]any{"x": 1}})
	renderExpectError(t, `{% for item %}{% endfor %}`, nil)
	renderExpectError(t, `{% for item in items %}{{ item }}`, map[string]any{"items": []any{1}})
	renderExpectError(t, `{% if a %}x`, map[string]any{"a": true})
}

func TestRenderStatementDelimitersAreRequired(t *testing.T) {
	// 只有开始标记没有结束标记的模板必须报错，不能把半截模板当正文发出去。
	renderExpectError(t, `{% if a %}x`, map[string]any{"a": true})
	renderExpectError(t, `{{ name`, map[string]any{"name": "x"})
	// 大括号不成对时按普通文本处理（正文里出现单个 { 很常见）。
	if out := renderToString(t, `{ 单个大括号 }`, nil); out != `{ 单个大括号 }` {
		t.Fatalf("普通文本 = %q", out)
	}
}

func TestRenderEscapesNothingButKeepsText(t *testing.T) {
	// 渲染结果是给通知正文用的纯文本，不做 HTML 转义，但变量内容必须原样出现。
	out := renderToString(t, `{{ name }}`, map[string]any{"name": "A & B <x>"})
	if out != "A & B <x>" {
		t.Fatalf("输出 = %q", out)
	}
	if strings.Contains(out, "&amp;") {
		t.Fatal("不应做 HTML 转义")
	}
}

package templating

import (
	"strings"
	"testing"
)

func renderToString(t *testing.T, source string, context map[string]any) string {
	t.Helper()
	out, err := Render(source, context)
	if err != nil {
		t.Fatalf("渲染 %q 失败: %v", source, err)
	}
	return out
}

func TestRenderInterpolatesVariables(t *testing.T) {
	out := renderToString(t, "你好 {{ name }}！", map[string]any{"name": "沙丘"})
	if out != "你好 沙丘！" {
		t.Fatalf("渲染结果 = %q", out)
	}
}

func TestRenderMissingVariableRendersEmpty(t *testing.T) {
	out := renderToString(t, "[{{ nothing }}]", nil)
	if out != "[]" {
		t.Fatalf("缺失变量应渲染为空串，实际 %q", out)
	}
}

func TestRenderDefaultFilterFallsBack(t *testing.T) {
	out := renderToString(t, `{{ missing | default("兜底") }}`, nil)
	if out != "兜底" {
		t.Fatalf("default 过滤器结果 = %q", out)
	}
}

func TestRenderStringFilters(t *testing.T) {
	ctx := map[string]any{"title": "  the batman  "}
	cases := map[string]string{
		`{{ title | trim | upper }}`:             "THE BATMAN",
		`{{ title | trim | lower }}`:             "the batman",
		`{{ title | trim | replace(" ", "-") }}`: "the-batman",
		`{{ title | trim | length }}`:            "10",
	}
	for source, want := range cases {
		if got := renderToString(t, source, ctx); got != want {
			t.Fatalf("%q 渲染为 %q，期望 %q", source, got, want)
		}
	}
}

func TestRenderUnknownFilterIsAnError(t *testing.T) {
	if _, err := Render(`{{ name | 不存在 }}`, map[string]any{"name": "x"}); err == nil {
		t.Fatal("未知过滤器应当报错，而不是静默输出空串")
	}
}

func TestRenderFilterMissingParenthesis(t *testing.T) {
	if _, err := Render(`{{ name | default("x" }}`, map[string]any{"name": ""}); err == nil {
		t.Fatal("过滤器参数缺少右括号应当报错")
	}
}

func TestRenderConditionalBlock(t *testing.T) {
	source := "{% if has_done %}已完成{% else %}进行中{% endif %}"
	if got := renderToString(t, source, map[string]any{"has_done": true}); got != "已完成" {
		t.Fatalf("true 分支 = %q", got)
	}
	if got := renderToString(t, source, map[string]any{"has_done": false}); got != "进行中" {
		t.Fatalf("false 分支 = %q", got)
	}
}

func TestRenderComparisonInCondition(t *testing.T) {
	source := "{% if votes > 100 %}热门{% else %}小众{% endif %}"
	if got := renderToString(t, source, map[string]any{"votes": 200}); got != "热门" {
		t.Fatalf("比较分支 = %q", got)
	}
	if got := renderToString(t, source, map[string]any{"votes": 5}); got != "小众" {
		t.Fatalf("比较分支 = %q", got)
	}
}

func TestRenderForLoop(t *testing.T) {
	source := "{% for item in items %}{{ item }};{% endfor %}"
	got := renderToString(t, source, map[string]any{"items": []any{"a", "b"}})
	if got != "a;b;" {
		t.Fatalf("循环结果 = %q", got)
	}
}

func TestRenderJoinFilter(t *testing.T) {
	got := renderToString(t, `{{ items | join(", ") }}`, map[string]any{"items": []any{"a", "b", "c"}})
	if got != "a, b, c" {
		t.Fatalf("join 结果 = %q", got)
	}
}

func TestRenderUnclosedTagIsAnError(t *testing.T) {
	if _, err := Render("{% if x %}没有收尾", map[string]any{"x": true}); err == nil {
		t.Fatal("未闭合的 if 应当报错")
	}
}

func TestParseTemplateRejectsDanglingDelimiter(t *testing.T) {
	if _, err := parseTemplate("正常 {{ name"); err == nil {
		t.Fatal("未闭合的 {{ 应当报错")
	}
}

func TestTruthyValues(t *testing.T) {
	cases := map[string]struct {
		value any
		want  bool
	}{
		"nil":   {nil, false},
		"空串":    {"", false},
		"非空串":   {"x", true},
		"零":     {0, false},
		"非零":    {3, true},
		"false": {false, false},
		"true":  {true, true},
		"空数组":   {[]any{}, false},
		"非空数组":  {[]any{1}, true},
		"空白串为真": {" ", true},
	}
	for name, tc := range cases {
		if got := truthy(tc.value); got != tc.want {
			t.Fatalf("%s: truthy(%v) = %v，期望 %v", name, tc.value, got, tc.want)
		}
	}
}

func TestStringifyUsesJSONForContainers(t *testing.T) {
	got := stringify([]any{1, 2})
	if !strings.Contains(got, "1") || !strings.Contains(got, "2") {
		t.Fatalf("stringify 容器结果 = %q", got)
	}
}

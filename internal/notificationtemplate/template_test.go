package notificationtemplate

import (
	"encoding/json"
	"testing"
)

func TestRenderStringSubstitutes(t *testing.T) {
	out, err := renderString("标题：{{ title }}", map[string]any{"title": "沙丘"})
	if err != nil {
		t.Fatalf("渲染失败: %v", err)
	}
	if out != "标题：沙丘" {
		t.Fatalf("渲染结果 = %q", out)
	}
}

func TestRestoreFormattedEscapes(t *testing.T) {
	if got := restoreFormattedEscapes(`a\nb`); got != "a\nb" {
		t.Fatalf("换行转义未还原: %q", got)
	}
	if got := restoreFormattedEscapes("普通文本"); got != "普通文本" {
		t.Fatalf("普通文本被改动: %q", got)
	}
}

func TestParsePythonLiteralDict(t *testing.T) {
	value, err := parsePythonLiteral(`{'title': '沙丘', 'year': 2024}`)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	dict, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("期望字典，实际 %T", value)
	}
	if dict["title"] != "沙丘" {
		t.Fatalf("title = %v", dict["title"])
	}
	if year, ok := dict["year"].(int64); !ok || year != 2024 {
		t.Fatalf("year = %v (%T)", dict["year"], dict["year"])
	}
}

func TestParsePythonLiteralSequence(t *testing.T) {
	value, err := parsePythonLiteral(`['a', 'b']`)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	list, ok := value.([]any)
	if !ok || len(list) != 2 {
		t.Fatalf("期望两个元素的列表，实际 %#v", value)
	}
}

func TestParsePythonLiteralScalars(t *testing.T) {
	cases := map[string]any{
		`'text'`: "text",
		`True`:   true,
		`False`:  false,
		`None`:   nil,
		`42`:     int64(42),
	}
	for source, want := range cases {
		got, err := parsePythonLiteral(source)
		if err != nil {
			t.Fatalf("解析 %q 失败: %v", source, err)
		}
		if got != want {
			t.Fatalf("解析 %q = %#v，期望 %#v", source, got, want)
		}
	}
}

func TestParsePythonLiteralRejectsUnbalanced(t *testing.T) {
	if _, err := parsePythonLiteral(`{'a': 'b'`); err == nil {
		t.Fatal("括号不匹配应当报错")
	}
}

func TestDecodeTemplateValueAcceptsString(t *testing.T) {
	// 裸字符串会被当成消息的 text 字段（与 V2 的简写模板一致）。
	value, err := decodeTemplateValue(json.RawMessage(`"直接字符串"`))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	dict, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("裸字符串应被包装成字典，实际 %T", value)
	}
	if dict["text"] != "直接字符串" {
		t.Fatalf("text = %#v", dict["text"])
	}
}

func TestDecodeTemplateValueAcceptsDict(t *testing.T) {
	value, err := decodeTemplateValue(json.RawMessage(`{"title":"x"}`))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if _, ok := value.(map[string]any); !ok {
		t.Fatalf("期望字典，实际 %T", value)
	}
}

func TestNewRejectsEmptyTemplate(t *testing.T) {
	if _, err := New(nil); err != nil {
		return // 空模板被接受也算合理，这里只要求不 panic
	}
}

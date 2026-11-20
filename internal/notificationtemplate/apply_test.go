package notificationtemplate

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/moviepilot-go/moviepilot-go/internal/notification"
)

func mustRenderer(t *testing.T, raw string) *Renderer {
	t.Helper()
	renderer, err := New(json.RawMessage(raw))
	if err != nil {
		t.Fatalf("解析模板失败: %v", err)
	}
	return renderer
}

func TestApplyRendersAllTemplateFields(t *testing.T) {
	renderer := mustRenderer(t, `{"A":{
		"title":"{{ title }}",
		"text":"第{{ season }}季\\n第{{ episode }}集",
		"image":"https://img.example/{{ tmdb_id }}.jpg",
		"link":"https://site.example/{{ tmdb_id }}"
	}}`)
	msg := notification.Message{ContentType: "A"}
	context := map[string]any{
		"title": "示例剧", "season": 2, "episode": 5, "tmdb_id": 1234,
	}

	applied, err := renderer.Apply(msg, context)
	if err != nil {
		t.Fatalf("渲染失败: %v", err)
	}
	if applied.Title != "示例剧" {
		t.Fatalf("标题 = %q", applied.Title)
	}
	// 模板里的 \n 是转义写法，渲染后必须变成真正的换行。
	if applied.Text != "第2季\n第5集" {
		t.Fatalf("正文 = %q", applied.Text)
	}
	if applied.Image != "https://img.example/1234.jpg" {
		t.Fatalf("图片 = %q", applied.Image)
	}
	if applied.Link != "https://site.example/1234" {
		t.Fatalf("链接 = %q", applied.Link)
	}
	// 入参不能被就地改写：渲染结果是副本。
	if msg.Title != "" || msg.Text != "" {
		t.Fatalf("原始消息被改写: %+v", msg)
	}
}

func TestApplyReturnsMessageUntouchedWhenNothingToDo(t *testing.T) {
	renderer := mustRenderer(t, `{"A":{"text":"渲染后"}}`)
	cases := map[string]struct {
		renderer *Renderer
		message  notification.Message
	}{
		"nil 渲染器":    {nil, notification.Message{ContentType: "A"}},
		"内容类型为空":     {renderer, notification.Message{}},
		"标题已存在":      {renderer, notification.Message{ContentType: "A", Title: "现成标题"}},
		"正文已存在":      {renderer, notification.Message{ContentType: "A", Text: "现成正文"}},
		"没有对应模板":     {renderer, notification.Message{ContentType: "B"}},
		"模板字典为空":     {mustRenderer(t, `{}`), notification.Message{ContentType: "A"}},
		"模板配置为 null": {mustRenderer(t, `null`), notification.Message{ContentType: "A"}},
	}
	for name, testCase := range cases {
		applied, err := testCase.renderer.Apply(testCase.message, map[string]any{"x": 1})
		if err != nil {
			t.Fatalf("%s 不应报错: %v", name, err)
		}
		if applied != testCase.message {
			t.Fatalf("%s 消息被改动: %+v -> %+v", name, testCase.message, applied)
		}
	}
}

func TestApplyKeepsFieldsMissingFromTemplate(t *testing.T) {
	renderer := mustRenderer(t, `{"A":{"title":"{{ title }}"}}`)
	msg := notification.Message{
		ContentType: "A",
		Image:       "https://img.example/original.jpg",
		Link:        "https://site.example/original",
	}
	applied, err := renderer.Apply(msg, map[string]any{"title": "新标题"})
	if err != nil {
		t.Fatalf("渲染失败: %v", err)
	}
	if applied.Title != "新标题" {
		t.Fatalf("标题 = %q", applied.Title)
	}
	if applied.Image != msg.Image || applied.Link != msg.Link {
		t.Fatalf("模板未覆盖的字段应保持原值: %+v", applied)
	}
}

func TestApplySkipsNonStringAndUnknownFields(t *testing.T) {
	// retry 是数字、flag 是布尔：不是字符串就跳过，不能把「5」渲染进正文。
	renderer := mustRenderer(t, `{"A":{"text":"正文","retry":5,"flag":true,"footer":"{{ unknown }}"}}`)
	applied, err := renderer.Apply(notification.Message{ContentType: "A"}, nil)
	if err != nil {
		t.Fatalf("渲染失败: %v", err)
	}
	if applied.Text != "正文" {
		t.Fatalf("正文 = %q", applied.Text)
	}
	// 未知字段渲染后丢弃，不应影响其它字段。
	if strings.Contains(applied.Text, "footer") {
		t.Fatalf("未知字段泄漏进正文: %q", applied.Text)
	}
}

func TestApplyReportsRenderErrors(t *testing.T) {
	renderer := mustRenderer(t, `{"A":{"text":"{{ 未闭合 "}}`)
	original := notification.Message{ContentType: "A"}
	applied, err := renderer.Apply(original, map[string]any{})
	if err == nil {
		t.Fatal("非法模板应报错")
	}
	if !strings.Contains(err.Error(), `"A"`) || !strings.Contains(err.Error(), `"text"`) {
		t.Fatalf("错误应带内容类型与字段: %v", err)
	}
	// 渲染失败必须返回原消息，不能返回半成品。
	if applied != original {
		t.Fatalf("失败时应返回原消息: %+v", applied)
	}
}

func TestApplyUsesEmptyContextWhenNil(t *testing.T) {
	renderer := mustRenderer(t, `{"A":{"text":"前{{ missing }}后"}}`)
	applied, err := renderer.Apply(notification.Message{ContentType: "A"}, nil)
	if err != nil {
		t.Fatalf("渲染失败: %v", err)
	}
	// gonja 对未定义变量输出空串；这里只断言不炸、且前后缀保留。
	if applied.Text != "前后" {
		t.Fatalf("正文 = %q", applied.Text)
	}
}

func TestNewRejectsScalarTemplate(t *testing.T) {
	if _, err := New(json.RawMessage(`{"A":123}`)); err == nil {
		t.Fatal("数字模板应被拒绝")
	}
	if _, err := New(json.RawMessage(`{"A":[1,2]}`)); err == nil {
		t.Fatal("数组模板应被拒绝")
	}
	if _, err := New(json.RawMessage(`{bad json`)); err == nil {
		t.Fatal("非法 JSON 应被拒绝")
	}
}

func TestNewAcceptsPythonLiteralBody(t *testing.T) {
	// V2 的默认模板里存在 Python 字面量（单引号）而不是 JSON。
	renderer := mustRenderer(t, `{"A":"{'title': '{{ title }}', 'text': '共 {{ count }} 条'}"}`)
	applied, err := renderer.Apply(notification.Message{ContentType: "A"}, map[string]any{"title": "汇总", "count": 3})
	if err != nil {
		t.Fatalf("渲染失败: %v", err)
	}
	if applied.Title != "汇总" || applied.Text != "共 3 条" {
		t.Fatalf("渲染结果 = %+v", applied)
	}
}

func TestNewAcceptsPlainTextTemplate(t *testing.T) {
	// 纯字符串模板等价于只设置正文。
	renderer := mustRenderer(t, `{"A":"{{ title }} 已入库"}`)
	applied, err := renderer.Apply(notification.Message{ContentType: "A"}, map[string]any{"title": "示例"})
	if err != nil {
		t.Fatalf("渲染失败: %v", err)
	}
	if applied.Text != "示例 已入库" {
		t.Fatalf("正文 = %q", applied.Text)
	}
}

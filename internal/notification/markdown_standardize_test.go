package notification

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/moviepilot-go/moviepilot-go/internal/notificationconfig"
)

func TestStandardizeTelegramMarkdown(t *testing.T) {
	cases := map[string]string{
		// MarkdownV2 保留字符逐个转义。
		"Hello (world)!": `Hello \(world\)\!`,
		"a.b-c+d=e":      `a\.b\-c\+d\=e`,
		// 常见语法被翻译成 MarkdownV2 的实体写法。
		"**粗体**": `*粗体*`,
		"*斜体*":   `_斜体_`,
		"_斜体_":   `_斜体_`,
		"~~删除~~": `~删除~`,
		"||剧透||": `||剧透||`,
		// 行内代码只转义反斜杠和反引号。
		"`code_x`": "`code_x`",
		// 代码块同理。
		"```go\nx := 1\n```": "```go\nx := 1\n```",
		// 未闭合的标记按普通字符转义。
		"*未闭合": `\*未闭合`,
		// 反斜杠转义：标点保持转义、非标点写成字面反斜杠。
		`\*不是粗体\*`: `\*不是粗体\*`,
		`\a`:       `\\a`,
		"尾部\\":     "尾部\\\\",
		// 行首 > 保留为引用，行内 > 转义。
		"> 引用一\n> 引用二": "> 引用一\n> 引用二",
		"a > b":        `a \> b`,
		// 链接：标签内部可以带格式，链接里的括号按深度匹配。
		"[标题](https://example.test/a(b)c)": `[标题](https://example.test/a(b\)c)`,
		"[**粗**](https://example.test/x)":  `[*粗*](https://example.test/x)`,
		// 没有闭合的链接整体按普通文本转义。
		"[a](b": `\[a\]\(b`,
	}
	for input, want := range cases {
		if got := standardizeTelegramMarkdown(input); got != want {
			t.Fatalf("standardizeTelegramMarkdown(%q)\n got %q\nwant %q", input, got, want)
		}
	}
}

func TestStandardizeMarkdownLinkAt(t *testing.T) {
	if _, _, ok := standardizeMarkdownLinkAt("plain", 0); ok {
		t.Fatal("非 [ 开头不应匹配")
	}
	if _, _, ok := standardizeMarkdownLinkAt("x[a](b)", 0); ok {
		t.Fatal("索引不在 [ 上不应匹配")
	}
	raw, end, ok := standardizeMarkdownLinkAt("[a](b)", 0)
	if !ok || raw != "[a](b)" || end != len("[a](b)") {
		t.Fatalf("简单链接 = (%q, %d, %v)", raw, end, ok)
	}
	// 链接地址内的括号按深度匹配，最外层 ) 结束；地址里的 ) 统一转义。
	raw, _, ok = standardizeMarkdownLinkAt("[a](b(c)d)", 0)
	if !ok || raw != `[a](b(c\)d)` {
		t.Fatalf("嵌套括号 = (%q, %v)", raw, ok)
	}
	// 链接之外的 ) 不属于链接，返回的结束位置正好在链接之后。
	raw, end, ok = standardizeMarkdownLinkAt("[a](b) 其余", 0)
	if !ok || raw != "[a](b)" || end != len("[a](b)") {
		t.Fatalf("尾随文本 = (%q, %d, %v)", raw, end, ok)
	}
	for _, text := range []string{"[a](b", "[a] (b)", "[a]"} {
		if _, _, ok := standardizeMarkdownLinkAt(text, 0); ok {
			t.Fatalf("%q 不应匹配为链接", text)
		}
	}
}

func TestTelegramMarkdownMarkerAt(t *testing.T) {
	code := []telegramEntity{{key: "md:`", open: "`", close: "`"}}
	if marker := telegramMarkdownMarkerAt("`x`", 0, code); marker != "`" {
		t.Fatalf("代码实体内的标记 = %q", marker)
	}
	if marker := telegramMarkdownMarkerAt("x`", 0, code); marker != "" {
		t.Fatalf("代码实体外位置 = %q", marker)
	}
	// 代码实体优先：``__`` 在代码里不当作加粗。
	if marker := telegramMarkdownMarkerAt("__x", 0, code); marker != "" {
		t.Fatalf("代码内不应匹配其它标记: %q", marker)
	}

	for _, testCase := range []struct {
		text string
		want string
	}{
		{"```code```", "```"},
		{"||spoiler||", "||"},
		{"__bold__", "__"},
		{"*italic*", "*"},
		{"_italic_", "_"},
		{"~strike~", "~"},
		{"`code`", "`"},
		{"plain", ""},
	} {
		if marker := telegramMarkdownMarkerAt(testCase.text, 0, nil); marker != testCase.want {
			t.Fatalf("telegramMarkdownMarkerAt(%q) = %q, want %q", testCase.text, marker, testCase.want)
		}
	}
}

func TestNonNilLogger(t *testing.T) {
	if nonNilLogger(nil) == nil {
		t.Fatal("nil logger 应回落到默认 logger")
	}
	custom := slog.New(slog.NewTextHandler(io.Discard, nil))
	if nonNilLogger(custom) != custom {
		t.Fatal("非 nil logger 应原样返回")
	}
}

// plainChannel 是最小的 ChannelSender，用于 Registry/NewSender 装配测试。
type plainChannel struct{ name Channel }

func (c plainChannel) Name() Channel { return c.name }

func (c plainChannel) Send(context.Context, Message) error { return nil }

func TestNewSenderNilLogger(t *testing.T) {
	dispatching := NewSender(nil, plainChannel{name: ChannelWebPush})
	if dispatching == nil {
		t.Fatal("NewSender 返回 nil")
	}
	if err := dispatching.Send(context.Background(), Message{Title: "标题"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	// 未知渠道。
	if err := dispatching.Send(context.Background(), Message{Channel: ChannelTelegram}); err == nil {
		t.Fatal("未注册渠道应报错")
	}
}

func TestRegistryReplace(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	var nilRegistry *Registry
	if err := nilRegistry.Replace(notificationconfig.Snapshot{}); !errors.Is(err, errRegistryUnavailable) {
		t.Fatalf("nil registry = %v", err)
	}

	broken := newRegistryWithFactory(logger, func(*slog.Logger, notificationconfig.Telegram) (ChannelSender, error) {
		return nil, errors.New("build failed")
	})
	if err := broken.Replace(notificationconfig.Snapshot{Telegrams: []notificationconfig.Telegram{{Name: "tg", BotToken: "t"}}}); !errors.Is(err, errRegistryBuild) {
		t.Fatalf("工厂失败 = %v", err)
	}
	if state := broken.load(); len(state.senders) != 0 {
		t.Fatalf("失败后状态被改写: %#v", state)
	}

	if err := broken.Replace(notificationconfig.Snapshot{Telegrams: []notificationconfig.Telegram{{Name: ""}}}); !errors.Is(err, errRegistryBuild) {
		t.Fatalf("空名称 = %v", err)
	}
	if err := broken.Replace(notificationconfig.Snapshot{Telegrams: []notificationconfig.Telegram{{Name: "dup"}, {Name: "dup"}}}); !errors.Is(err, errRegistryBuild) {
		t.Fatalf("重名 = %v", err)
	}

	wrongChannel := newRegistryWithFactory(logger, func(*slog.Logger, notificationconfig.Telegram) (ChannelSender, error) {
		return plainChannel{name: ChannelWebPush}, nil
	})
	if err := wrongChannel.Replace(notificationconfig.Snapshot{Telegrams: []notificationconfig.Telegram{{Name: "tg"}}}); !errors.Is(err, errRegistryBuild) {
		t.Fatalf("渠道不符 = %v", err)
	}

	// 正常路径：Telegram 走工厂，配置渠道走 newConfiguredChannelSender；
	// 未启用、空名称、配置不完整的渠道被跳过。
	registry := NewRegistry(logger)
	err := registry.Replace(notificationconfig.Snapshot{
		Telegrams: []notificationconfig.Telegram{{Name: "我的 Bot", BotToken: "token", ChatID: "1", Switchs: []string{"download"}}},
		Configs: []notificationconfig.Config{
			{Name: "Discord", Type: string(ChannelDiscord), Enabled: true, Config: map[string]any{"DISCORD_BOT_TOKEN": "t"}},
			{Name: "禁用", Type: string(ChannelDiscord), Enabled: false, Config: map[string]any{"DISCORD_BOT_TOKEN": "t"}},
			{Name: "", Type: string(ChannelSlack), Enabled: true, Config: map[string]any{"SLACK_OAUTH_TOKEN": "t"}},
			{Name: "缺配置", Type: string(ChannelSlack), Enabled: true, Config: map[string]any{}},
			{Name: "跳过的 Telegram", Type: string(ChannelTelegram), Enabled: true},
		},
	})
	if err != nil {
		t.Fatalf("Replace: %v", err)
	}
	state := registry.load()
	if len(state.senders) != 1 || state.senders["我的 Bot"] == nil {
		t.Fatalf("Telegram 发送器 = %#v", state.senders)
	}
	if len(state.channels[ChannelTelegram]) != 1 {
		t.Fatalf("Telegram 渠道 = %#v", state.channels[ChannelTelegram])
	}
	if len(state.channels[ChannelDiscord]) != 1 {
		t.Fatalf("Discord 渠道 = %#v", state.channels[ChannelDiscord])
	}
	if len(state.channels[ChannelSlack]) != 0 {
		t.Fatalf("不完整配置不应注入: %#v", state.channels[ChannelSlack])
	}

	// 目标投影与装配结果一致。
	targets := registry.Targets(Message{Channel: ChannelDiscord})
	if len(targets) != 1 || targets[0].Source != "Discord" {
		t.Fatalf("Discord 目标 = %#v", targets)
	}
}

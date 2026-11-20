package notification

import (
	"strings"
	"testing"
)

func TestSupportedChannel(t *testing.T) {
	for _, channel := range []Channel{
		ChannelWebPush, ChannelTelegram, ChannelWebhook, ChannelDiscord, ChannelFeishu,
		ChannelSlack, ChannelSynologyChat, ChannelVoceChat, ChannelQQ, ChannelWeChat,
	} {
		if !SupportedChannel(channel) {
			t.Fatalf("%q 应受支持", channel)
		}
	}
	for _, channel := range []Channel{"", "unknown", "email"} {
		if SupportedChannel(channel) {
			t.Fatalf("%q 不应受支持", channel)
		}
	}
}

func TestEscapeTelegramCode(t *testing.T) {
	// Telegram MarkdownV2 的代码块里只需转义反斜杠与反引号。
	if got := escapeTelegramCode("普通文本"); got != "普通文本" {
		t.Fatalf("不必要的转义: %q", got)
	}
	if got := escapeTelegramCode("a`b"); got != "a\\`b" {
		t.Fatalf("反引号转义 = %q", got)
	}
	if got := escapeTelegramCode(`a\b`); got != `a\\b` {
		t.Fatalf("反斜杠转义 = %q", got)
	}
	// 反斜杠必须先转义，否则会把后面的反引号转义吃掉。
	if got := escapeTelegramCode("a\\`b"); got != "a\\\\\\`b" {
		t.Fatalf("组合转义 = %q", got)
	}
}

func TestSplitWechatContent(t *testing.T) {
	// 空内容返回单个空块（调用方仍需发一条消息）。
	if got := splitWechatContent("", 10); len(got) != 1 || got[0] != "" {
		t.Fatalf("空内容 = %v", got)
	}
	// 未超限时保持原文。
	if got := splitWechatContent("short", 10); len(got) != 1 || got[0] != "short" {
		t.Fatalf("短内容 = %v", got)
	}
	// 超限时按字节切分，且拼回去必须与原文一致（不能丢字符）。
	content := strings.Repeat("消", 20) // 3 字节/字符，共 60 字节
	chunks := splitWechatContent(content, 30)
	if len(chunks) < 2 {
		t.Fatalf("应切成多块: %v", chunks)
	}
	for _, chunk := range chunks {
		if chunk == "" {
			t.Fatalf("不应出现空块: %v", chunks)
		}
	}
	if strings.Join(chunks, "") != content {
		t.Fatalf("拼接后与原文不一致: %v", chunks)
	}
}

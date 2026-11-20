package notification

import "testing"

func TestMessageTextJoinsNonEmptyParts(t *testing.T) {
	if got := messageText(Message{Title: "标题", Text: "正文", Link: "https://x/y"}); got != "标题\n正文\nhttps://x/y" {
		t.Fatalf("完整消息拼接错误: %q", got)
	}
	// 空字段不能留下多余的空行——不少渠道对首尾空行很敏感。
	if got := messageText(Message{Text: "只有正文"}); got != "只有正文" {
		t.Fatalf("缺少标题/链接时不应有空行: %q", got)
	}
	if got := messageText(Message{Title: "标题", Link: "https://x/y"}); got != "标题\nhttps://x/y" {
		t.Fatalf("缺少正文时不应有空行: %q", got)
	}
	if got := messageText(Message{}); got != "" {
		t.Fatalf("空消息应为空串，实际 %q", got)
	}
}

func TestIsSlackChannelID(t *testing.T) {
	for _, value := range []string{"C0123456", "G9876543", "D0000001"} {
		if !isSlackChannelID(value) {
			t.Fatalf("%q 应被识别为 Slack 频道 ID", value)
		}
	}
	// 频道名（general / #general）不是 ID，必须走 conversations.list 查找。
	for _, value := range []string{"", "general", "#general", "c0123456", " UC123"} {
		if isSlackChannelID(value) {
			t.Fatalf("%q 不应被识别为 Slack 频道 ID", value)
		}
	}
}

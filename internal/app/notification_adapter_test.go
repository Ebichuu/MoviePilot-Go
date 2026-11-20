package app

import (
	"context"
	"errors"
	"testing"

	"github.com/moviepilot-go/moviepilot-go/internal/notification"
	"github.com/moviepilot-go/moviepilot-go/internal/organizationflow"
)

type organizationNotificationSenderFake struct {
	messages []notification.Message
	err      error
}

func (s *organizationNotificationSenderFake) Send(_ context.Context, msg notification.Message) error {
	s.messages = append(s.messages, msg)
	return s.err
}

func TestNewOrganizationNotificationAdapter(t *testing.T) {
	t.Parallel()

	if got := newOrganizationNotificationAdapter(nil); got != nil {
		t.Fatalf("nil sender adapter = %#v", got)
	}
	sender := &organizationNotificationSenderFake{}
	adapter := newOrganizationNotificationAdapter(sender)
	if adapter == nil {
		t.Fatal("adapter = nil")
	}
}

func TestOrganizationNotificationAdapterMapsMessage(t *testing.T) {
	t.Parallel()

	sender := &organizationNotificationSenderFake{}
	adapter := newOrganizationNotificationAdapter(sender)
	message := organizationflow.NotificationMessage{
		Type:        "整理入库",
		ContentType: "整理完成",
		Title:       "标题",
		Text:        "正文",
		Image:       "https://example.test/poster.jpg",
		Link:        "https://example.test/item/1",
	}
	if err := adapter.Send(t.Context(), message); err != nil {
		t.Fatalf("Send() error = %v", err)
	}
	if len(sender.messages) != 1 {
		t.Fatalf("下游收到 %d 条", len(sender.messages))
	}
	got := sender.messages[0]
	// 类型是字符串别名，必须原样映射，否则渠道分流会挑错目标。
	if string(got.Type) != message.Type || string(got.ContentType) != message.ContentType {
		t.Fatalf("类型映射 = (%q, %q)", got.Type, got.ContentType)
	}
	if got.Title != message.Title || got.Text != message.Text ||
		got.Image != message.Image || got.Link != message.Link {
		t.Fatalf("内容映射 = %+v", got)
	}

	// 渠道错误原样上抛，整理流程据此判断通知是否送达。
	boom := errors.New("通知渠道不可用")
	failing := &organizationNotificationSenderFake{err: boom}
	if err := newOrganizationNotificationAdapter(failing).Send(t.Context(), message); !errors.Is(err, boom) {
		t.Fatalf("Send() error = %v", err)
	}
}

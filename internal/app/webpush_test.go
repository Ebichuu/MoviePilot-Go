package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/moviepilot-go/moviepilot-go/internal/config"
	"github.com/moviepilot-go/moviepilot-go/internal/database"
	"github.com/moviepilot-go/moviepilot-go/internal/httpapi"
	"github.com/moviepilot-go/moviepilot-go/internal/notification"
)

type webPushRecordingSender struct {
	messages []notification.Message
	err      error
}

func (s *webPushRecordingSender) Send(_ context.Context, msg notification.Message) error {
	s.messages = append(s.messages, msg)
	return s.err
}

func TestNewWebPushSenderRequiresConfiguration(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	store := &database.Store{}
	// 缺任一依赖都返回 nil：调用方据此不上报「WebPush 可用」。
	if got := newWebPushSender(config.Config{VAPIDPrivateKey: "key", VAPIDSubject: "mailto:a@b"}, nil, logger); got != nil {
		t.Fatalf("缺存储 = %#v", got)
	}
	if got := newWebPushSender(config.Config{VAPIDSubject: "mailto:a@b"}, store, logger); got != nil {
		t.Fatalf("缺私钥 = %#v", got)
	}
	if got := newWebPushSender(config.Config{VAPIDPrivateKey: "key"}, store, logger); got != nil {
		t.Fatalf("缺 subject = %#v", got)
	}
	// 空白不算配置：避免用空格拼出一个永远发不出去的渠道。
	if got := newWebPushSender(config.Config{VAPIDPrivateKey: "  ", VAPIDSubject: " "}, store, logger); got != nil {
		t.Fatalf("空白配置 = %#v", got)
	}

	sender := newWebPushSender(config.Config{
		VAPIDPrivateKey: "private-key", VAPIDPublicKey: "public-key", VAPIDSubject: "mailto:a@b",
	}, store, logger)
	if sender == nil {
		t.Fatal("配置齐全时 sender = nil")
	}
	if sender.Name() != notification.ChannelWebPush {
		t.Fatalf("渠道名 = %q", sender.Name())
	}
}

func TestWebPushDispatcherFrom(t *testing.T) {
	t.Parallel()

	if got := webPushDispatcherFrom(nil); got != nil {
		t.Fatalf("nil sender dispatcher = %#v", got)
	}
	sender := notification.NewWebPushSender(nil, notification.WebPushConfig{
		VAPIDPrivateKey: "private-key", VAPIDSubject: "mailto:a@b",
	}, nil)
	dispatcher := webPushDispatcherFrom(sender)
	if dispatcher == nil {
		t.Fatal("dispatcher = nil")
	}
	// 未接渠道的分发器是「静默跳过」，不是错误：手动推送在没配 WebPush 时不该失败。
	if err := (webPushDispatcher{}).SendWebPush(t.Context(), "标题", "正文", "https://example.test"); err != nil {
		t.Fatalf("空分发器 error = %v", err)
	}
	var compileCheck httpapi.WebPushDispatcher = dispatcher
	_ = compileCheck
}

func TestNamedNotificationSenderDelegates(t *testing.T) {
	t.Parallel()

	var nilSender namedNotificationSender
	if got := nilSender.Name(); got != "" {
		t.Fatalf("nil sender Name() = %q", got)
	}

	recorder := &webPushRecordingSender{}
	sender := namedNotificationSender{name: notification.ChannelWebhook, sender: recorder}
	if sender.Name() != notification.ChannelWebhook {
		t.Fatalf("Name() = %q", sender.Name())
	}
	message := notification.Message{Title: "标题", Text: "正文", Type: notification.TypeManual}
	if err := sender.Send(t.Context(), message); err != nil {
		t.Fatalf("Send() error = %v", err)
	}
	if len(recorder.messages) != 1 || recorder.messages[0].Title != "标题" {
		t.Fatalf("下游收到 = %#v", recorder.messages)
	}

	// 下游错误原样返回，分发层才能判断哪个渠道失败。
	boom := errors.New("渠道发送失败")
	failing := namedNotificationSender{name: notification.ChannelWebhook, sender: &webPushRecordingSender{err: boom}}
	if err := failing.Send(t.Context(), message); !errors.Is(err, boom) {
		t.Fatalf("Send() error = %v", err)
	}
}

package app

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/moviepilot-go/moviepilot-go/internal/notification"
	"github.com/moviepilot-go/moviepilot-go/internal/notificationconfig"
)

type webPushSourceStub struct {
	subscriptions []notification.WebPushSubscription
	listErr       error
	removeErr     error
	listCalls     int
	removed       []string
}

func (s *webPushSourceStub) List(context.Context) ([]notification.WebPushSubscription, error) {
	s.listCalls++
	if s.listErr != nil {
		return nil, s.listErr
	}
	return append([]notification.WebPushSubscription(nil), s.subscriptions...), nil
}

func (s *webPushSourceStub) Remove(_ context.Context, endpoint string) error {
	if s.removeErr != nil {
		return s.removeErr
	}
	s.removed = append(s.removed, endpoint)
	return nil
}

// newWebPushRegistry 构造一个已把 WebPush 配置投影为投递目标的注册表。
func newWebPushRegistry(t *testing.T, available bool, name string) *notification.Registry {
	t.Helper()
	registry := notification.NewRegistry(slog.Default())
	if err := registry.Replace(notificationconfig.Snapshot{Configs: []notificationconfig.Config{{
		Name: name, Type: string(notification.ChannelWebPush), Enabled: true,
	}}}); err != nil {
		t.Fatalf("Replace() error = %v", err)
	}
	registry.SetWebPushAvailable(available)
	return registry
}

func newWebPushSenderForTest(source notification.WebPushSubscriptionSource) *notification.WebPushSender {
	return notification.NewWebPushSender(slog.Default(), notification.WebPushConfig{
		VAPIDPrivateKey: "test-private-key",
		VAPIDPublicKey:  "test-public-key",
		VAPIDSubject:    "mailto:test@example.com",
	}, source)
}

// TestNotificationChannelDispatcherSend 覆盖按渠道分发：WebPush 直投、
// 空渠道同时扇出、来源过滤、不可用渠道与错误聚合。
func TestNotificationChannelDispatcherSend(t *testing.T) {
	ctx := context.Background()
	message := notification.Message{
		Type: notification.TypeSubscribe, Title: "标题", Text: "正文", UserID: "42",
	}

	if err := (notificationChannelDispatcher{}).Send(nil, message); !errors.Is(err, errNotificationChannelUnavailable) {
		t.Fatalf("nil ctx error = %v", err)
	}
	if err := (notificationChannelDispatcher{}).Send(ctx, message); !errors.Is(err, errNotificationChannelUnavailable) {
		t.Fatalf("nil registry error = %v", err)
	}

	registry := newWebPushRegistry(t, true, "主推送")
	source := &webPushSourceStub{}
	sender := newWebPushSenderForTest(source)
	dispatcher := notificationChannelDispatcher{registry: registry, webPush: sender}

	// WebPush 渠道直投：没有订阅时视为成功，但确实查询了订阅表。
	webPushMessage := message
	webPushMessage.Channel = notification.ChannelWebPush
	if err := dispatcher.Send(ctx, webPushMessage); err != nil {
		t.Fatalf("WebPush Send error = %v", err)
	}
	if source.listCalls != 1 {
		t.Fatalf("订阅表查询次数 = %d, want 1", source.listCalls)
	}

	// 来源过滤命中不了目标。
	filtered := webPushMessage
	filtered.Source = "别的来源"
	if err := dispatcher.Send(ctx, filtered); !errors.Is(err, errNotificationChannelUnavailable) {
		t.Fatalf("来源不匹配 error = %v", err)
	}

	// 未配置 WebPush 运行时。
	unavailable := notificationChannelDispatcher{registry: newWebPushRegistry(t, false, "主推送")}
	if err := unavailable.Send(ctx, webPushMessage); !errors.Is(err, errNotificationChannelUnavailable) {
		t.Fatalf("WebPush 未启用 error = %v", err)
	}

	// 空渠道且只有 WebPush 目标：不调用 registry，直接走 WebPush。
	before := source.listCalls
	if err := dispatcher.Send(ctx, message); err != nil {
		t.Fatalf("空渠道 WebPush Send error = %v", err)
	}
	if source.listCalls != before+1 {
		t.Fatalf("空渠道订阅查询次数 = %d, want %d", source.listCalls, before+1)
	}

	// 空渠道且没有任何目标。
	empty := notificationChannelDispatcher{registry: notification.NewRegistry(slog.Default())}
	if err := empty.Send(ctx, message); !errors.Is(err, errNotificationChannelUnavailable) {
		t.Fatalf("无目标 error = %v", err)
	}

	// 显式非 WebPush 渠道交给 registry，注册表里没有对应发送器时报错。
	telegramMessage := message
	telegramMessage.Channel = notification.ChannelTelegram
	if err := dispatcher.Send(ctx, telegramMessage); err == nil {
		t.Fatal("缺少 Telegram 发送器时应返回错误")
	}

	// WebPush 查询失败时把错误返回给调用方。
	failing := notificationChannelDispatcher{
		registry: newWebPushRegistry(t, true, "主推送"),
		webPush:  newWebPushSenderForTest(&webPushSourceStub{listErr: errors.New("数据库不可用")}),
	}
	if err := failing.Send(ctx, message); err == nil {
		t.Fatal("订阅表不可用时应返回错误")
	}

	// 未设置发送器时 hasWebPushTarget/hasNonWebPushTarget 均为假。
	if (notificationChannelDispatcher{}).hasWebPushTarget(message) ||
		(notificationChannelDispatcher{}).hasNonWebPushTarget(message) {
		t.Fatal("nil registry 不应报告任何目标")
	}
	if !dispatcher.hasWebPushTarget(webPushMessage) {
		t.Fatal("应识别出 WebPush 目标")
	}
	if dispatcher.hasNonWebPushTarget(webPushMessage) {
		t.Fatal("只有 WebPush 配置时不应报告非 WebPush 目标")
	}
}

// TestWebPushDispatcherSendWebPush 覆盖 `/message/webpush/send` 的手动推送入口。
func TestWebPushDispatcherSendWebPush(t *testing.T) {
	ctx := context.Background()
	if err := (webPushDispatcher{}).SendWebPush(ctx, "标题", "正文", ""); err != nil {
		t.Fatalf("nil sender error = %v", err)
	}
	if webPushDispatcherFrom(nil) != nil {
		t.Fatal("nil 发送器不应产出 dispatcher")
	}

	source := &webPushSourceStub{}
	dispatcher := webPushDispatcherFrom(newWebPushSenderForTest(source))
	if dispatcher == nil {
		t.Fatal("webPushDispatcherFrom() = nil")
	}
	if err := dispatcher.SendWebPush(ctx, "标题", "正文", "https://example.com"); err != nil {
		t.Fatalf("SendWebPush() error = %v", err)
	}
	if source.listCalls != 1 {
		t.Fatalf("订阅表查询次数 = %d, want 1", source.listCalls)
	}
}

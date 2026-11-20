package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/moviepilot-go/moviepilot-go/internal/config"
	"github.com/moviepilot-go/moviepilot-go/internal/database"
	"github.com/moviepilot-go/moviepilot-go/internal/notification"
)

type appWebPushSource struct {
	calls int
	subs  []notification.WebPushSubscription
}

func (s *appWebPushSource) List(context.Context) ([]notification.WebPushSubscription, error) {
	s.calls++
	return s.subs, nil
}

func (s *appWebPushSource) Remove(context.Context, string) error { return nil }

func validVAPIDConfig() config.Config {
	return config.Config{
		VAPIDPrivateKey: "nQ1WGy8mJ3l0dK9sXzq4bT7vR2cY5hF8aM6pE3uN0oI",
		VAPIDSubject:    "mailto:admin@example.com",
	}
}

func TestNewWebPushSenderRequiresVAPIDAndStore(t *testing.T) {
	store := &database.Store{}
	cfg := validVAPIDConfig()
	// 三条必要条件缺一不可：没有订阅表或没有 VAPID 配置时不能假装渠道可用。
	if newWebPushSender(cfg, nil, nil) != nil {
		t.Fatal("缺少数据库时不应构建 WebPush 渠道")
	}
	noPrivate := cfg
	noPrivate.VAPIDPrivateKey = "   "
	if newWebPushSender(noPrivate, store, nil) != nil {
		t.Fatal("缺少 VAPID 私钥时不应构建 WebPush 渠道")
	}
	noSubject := cfg
	noSubject.VAPIDSubject = ""
	if newWebPushSender(noSubject, store, nil) != nil {
		t.Fatal("缺少 VAPID subject 时不应构建 WebPush 渠道")
	}
	if newWebPushSender(cfg, store, nil) == nil {
		t.Fatal("配置齐全时应构建 WebPush 渠道")
	}
}

func TestWebPushDispatcherNilSenderIsNoop(t *testing.T) {
	// 未配置 WebPush 时手动推送接口必须安静地什么都不做，而不是报错吓到用户。
	if err := (webPushDispatcher{}).SendWebPush(context.Background(), "标题", "正文", "/x"); err != nil {
		t.Fatalf("空分发器 = %v", err)
	}
	if webPushDispatcherFrom(nil) != nil {
		t.Fatal("nil 渠道不应生成分发器")
	}
}

func TestWebPushDispatcherSendsManualMessage(t *testing.T) {
	source := &appWebPushSource{}
	sender := notification.NewWebPushSender(nil, notification.WebPushConfig{
		VAPIDPrivateKey: validVAPIDConfig().VAPIDPrivateKey,
		VAPIDSubject:    validVAPIDConfig().VAPIDSubject,
	}, source)
	dispatcher := webPushDispatcherFrom(sender)
	if dispatcher == nil {
		t.Fatal("应生成分发器")
	}
	// 没有订阅时依然走完整个发送流程（证明分发器真的调到了渠道）。
	if err := dispatcher.SendWebPush(context.Background(), "手动标题", "手动正文", "/#/x"); err != nil {
		t.Fatalf("发送失败: %v", err)
	}
	if source.calls != 1 {
		t.Fatalf("订阅列表调用次数 = %d", source.calls)
	}

	// 已取消的上下文要如实返回取消错误，而不是被吞掉。
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := dispatcher.SendWebPush(ctx, "标题", "正文", ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("已取消 = %v", err)
	}
}

func TestNotificationChannelDispatcherGuards(t *testing.T) {
	dispatcher := notificationChannelDispatcher{}
	// 没有注册表时无法分发，必须报「渠道不可用」。
	if err := dispatcher.Send(context.Background(), notification.Message{Title: "x"}); !errors.Is(err, errNotificationChannelUnavailable) {
		t.Fatalf("空注册表 = %v", err)
	}
	if err := dispatcher.Send(nil, notification.Message{Title: "x"}); !errors.Is(err, errNotificationChannelUnavailable) {
		t.Fatalf("nil 上下文 = %v", err)
	}
	// 没有注册表时目标探测一律为 false（不能凭空认为有目标）。
	if dispatcher.hasWebPushTarget(notification.Message{}) {
		t.Fatal("空注册表不应有 WebPush 目标")
	}
	if dispatcher.hasNonWebPushTarget(notification.Message{}) {
		t.Fatal("空注册表不应有其它渠道目标")
	}
	// 显式指定 WebPush 渠道但没有 WebPush 渠道时返回不可用。
	if err := dispatcher.Send(context.Background(), notification.Message{Channel: notification.ChannelWebPush}); !errors.Is(err, errNotificationChannelUnavailable) {
		t.Fatalf("缺 WebPush 渠道 = %v", err)
	}
}

func TestWebPushDispatcherTimeoutBudget(t *testing.T) {
	// 手动推送必须能在合理时间内返回：用已超时上下文验证不会阻塞。
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	<-ctx.Done()
	source := &appWebPushSource{}
	sender := notification.NewWebPushSender(nil, notification.WebPushConfig{
		VAPIDPrivateKey: validVAPIDConfig().VAPIDPrivateKey,
		VAPIDSubject:    validVAPIDConfig().VAPIDSubject,
	}, source)
	if err := webPushDispatcherFrom(sender).SendWebPush(ctx, "标题", "正文", ""); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("超时 = %v", err)
	}
}

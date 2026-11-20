package app

import (
	"context"
	"log/slog"
	"strings"

	"github.com/moviepilot-go/moviepilot-go/internal/config"
	"github.com/moviepilot-go/moviepilot-go/internal/database"
	"github.com/moviepilot-go/moviepilot-go/internal/httpapi"
	"github.com/moviepilot-go/moviepilot-go/internal/notification"
)

// webPushSubscriptionStore 把 Go 订阅表接到 V2 WebPush 订阅端口。
type webPushSubscriptionStore struct {
	store *database.Store
}

func (s *webPushSubscriptionStore) List(ctx context.Context) ([]notification.WebPushSubscription, error) {
	rows, err := s.store.ListWebPushSubscriptions(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]notification.WebPushSubscription, 0, len(rows))
	for _, row := range rows {
		result = append(result, notification.WebPushSubscription{
			Endpoint: row.Endpoint, Username: row.Username, P256dh: row.P256dh, Auth: row.Auth,
		})
	}
	return result, nil
}

func (s *webPushSubscriptionStore) Remove(ctx context.Context, endpoint string) error {
	_, err := s.store.DeleteWebPushSubscription(ctx, endpoint)
	return err
}

// newWebPushSender 在配置了 VAPID 私钥与 subject 时构建真实 WebPush 渠道。
func newWebPushSender(
	cfg config.Config,
	store *database.Store,
	logger *slog.Logger,
) *notification.WebPushSender {
	if store == nil ||
		strings.TrimSpace(cfg.VAPIDPrivateKey) == "" ||
		strings.TrimSpace(cfg.VAPIDSubject) == "" {
		return nil
	}
	return notification.NewWebPushSender(logger, notification.WebPushConfig{
		VAPIDPrivateKey: cfg.VAPIDPrivateKey,
		VAPIDPublicKey:  cfg.VAPIDPublicKey,
		VAPIDSubject:    cfg.VAPIDSubject,
		Usernames:       cfg.WebPushUsernames,
	}, &webPushSubscriptionStore{store: store})
}

// namedNotificationSender 让普通 Sender 参与按渠道分发。
type namedNotificationSender struct {
	name   notification.Channel
	sender notification.Sender
}

func (s namedNotificationSender) Name() notification.Channel { return s.name }

func (s namedNotificationSender) Send(ctx context.Context, msg notification.Message) error {
	return s.sender.Send(ctx, msg)
}

// webPushDispatcher 实现 httpapi.WebPushDispatcher，把 `/message/webpush/send`
// 请求交给已配置的 WebPush 渠道。
type webPushDispatcher struct {
	sender *notification.WebPushSender
}

func (d webPushDispatcher) SendWebPush(ctx context.Context, title, body, link string) error {
	if d.sender == nil {
		return nil
	}
	return d.sender.Send(ctx, notification.Message{
		Channel: notification.ChannelWebPush, Type: notification.TypeManual,
		ContentType: notification.ContentManualProcessing,
		Title:       title, Text: body, Link: link,
	})
}

func webPushDispatcherFrom(sender *notification.WebPushSender) httpapi.WebPushDispatcher {
	if sender == nil {
		return nil
	}
	return webPushDispatcher{sender: sender}
}

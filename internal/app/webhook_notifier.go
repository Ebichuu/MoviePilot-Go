package app

import (
	"context"

	"github.com/moviepilot-go/moviepilot-go/internal/httpapi"
	"github.com/moviepilot-go/moviepilot-go/internal/notification"
)

type webhookNotificationBridge struct {
	sender notification.Sender
}

func (b webhookNotificationBridge) NotifyWebhook(ctx context.Context, event httpapi.WebhookEvent) error {
	if b.sender == nil {
		return nil
	}
	return b.sender.Send(ctx, notification.Message{
		Type:  notification.TypeMediaServer,
		Title: event.Title,
		Text:  event.Text,
	})
}

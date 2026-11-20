package app

import (
	"context"

	"github.com/moviepilot-go/moviepilot-go/internal/notification"
	"github.com/moviepilot-go/moviepilot-go/internal/organizationflow"
)

// organizationNotificationAdapter adapts notification.Sender to organizationflow.NotificationSender.
type organizationNotificationAdapter struct {
	sender notification.Sender
}

func (a *organizationNotificationAdapter) Send(ctx context.Context, msg organizationflow.NotificationMessage) error {
	return a.sender.Send(ctx, notification.Message{
		Type:        notification.Type(msg.Type),
		ContentType: notification.ContentType(msg.ContentType),
		Title:       msg.Title,
		Text:        msg.Text,
		Image:       msg.Image,
		Link:        msg.Link,
	})
}

func newOrganizationNotificationAdapter(sender notification.Sender) organizationflow.NotificationSender {
	if sender == nil {
		return nil
	}
	return &organizationNotificationAdapter{sender: sender}
}

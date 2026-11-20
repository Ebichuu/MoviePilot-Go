package app

import (
	"context"
	"errors"

	"github.com/moviepilot-go/moviepilot-go/internal/notification"
)

var errNotificationChannelUnavailable = errors.New("notification channel unavailable")

type notificationChannelDispatcher struct {
	registry *notification.Registry
	webPush  *notification.WebPushSender
}

func (d notificationChannelDispatcher) Send(ctx context.Context, msg notification.Message) error {
	if ctx == nil || d.registry == nil {
		return errNotificationChannelUnavailable
	}
	if msg.Channel == notification.ChannelWebPush {
		if d.webPush == nil || !d.hasWebPushTarget(msg) {
			return errNotificationChannelUnavailable
		}
		return d.webPush.Send(ctx, msg)
	}
	if msg.Channel != "" {
		return d.registry.Send(ctx, msg)
	}

	var registryErr, webPushErr error
	if d.hasNonWebPushTarget(msg) {
		registryErr = d.registry.Send(ctx, msg)
	}
	if d.webPush != nil && d.hasWebPushTarget(notification.Message{
		Channel: notification.ChannelWebPush, Type: msg.Type, UserID: msg.UserID,
	}) {
		webPushErr = d.webPush.Send(ctx, msg)
	}
	if registryErr == nil && webPushErr == nil {
		if d.hasNonWebPushTarget(msg) || d.hasWebPushTarget(notification.Message{
			Channel: notification.ChannelWebPush, Type: msg.Type, UserID: msg.UserID,
		}) {
			return nil
		}
		return errNotificationChannelUnavailable
	}
	return errors.Join(registryErr, webPushErr)
}

func (d notificationChannelDispatcher) hasWebPushTarget(msg notification.Message) bool {
	if d.registry == nil {
		return false
	}
	for _, target := range d.registry.Targets(msg) {
		if target.Channel == notification.ChannelWebPush &&
			(msg.Source == "" || target.Source == msg.Source) {
			return true
		}
	}
	return false
}

func (d notificationChannelDispatcher) hasNonWebPushTarget(msg notification.Message) bool {
	if d.registry == nil {
		return false
	}
	for _, target := range d.registry.Targets(msg) {
		if target.Channel != notification.ChannelWebPush {
			return true
		}
	}
	return false
}

var _ notification.Sender = notificationChannelDispatcher{}

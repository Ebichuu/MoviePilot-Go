// Package notification provides unified message delivery to multiple channels.
package notification

import (
	"context"
	"time"
)

// Type represents the notification category.
type Type string

const (
	TypeDownload    Type = "资源下载"
	TypeOrganize    Type = "整理入库"
	TypeSubscribe   Type = "订阅"
	TypeSiteMessage Type = "站点"
	TypeMediaServer Type = "媒体服务器"
	TypeManual      Type = "手动处理"
	TypePlugin      Type = "插件"
)

// ContentType represents specific notification events.
type ContentType string

const (
	ContentOrganizeSuccess    ContentType = "organizeSuccess"
	ContentDownloadAdded      ContentType = "downloadAdded"
	ContentSubscribeAdded     ContentType = "subscribeAdded"
	ContentSubscribeComplete  ContentType = "subscribeComplete"
	ContentManualProcessing   ContentType = "manualProcessing"
	ContentManualProcessingRe ContentType = "manualProcessingRe"
)

// Channel represents the delivery method.
type Channel string

const (
	ChannelWebPush      Channel = "webpush"
	ChannelTelegram     Channel = "telegram"
	ChannelWebhook      Channel = "webhook"
	ChannelDiscord      Channel = "discord"
	ChannelFeishu       Channel = "feishu"
	ChannelSlack        Channel = "slack"
	ChannelSynologyChat Channel = "synologychat"
	ChannelVoceChat     Channel = "vocechat"
	ChannelQQ           Channel = "qqbot"
	ChannelWeChat       Channel = "wechat"
	ChannelQQBot                = ChannelQQ
	ChannelWechat               = ChannelWeChat
)

// SupportedChannel reports whether the notification runtime recognizes a
// persisted channel identifier.
func SupportedChannel(channel Channel) bool {
	switch channel {
	case ChannelWebPush, ChannelTelegram, ChannelWebhook, ChannelDiscord, ChannelFeishu,
		ChannelSlack, ChannelSynologyChat, ChannelVoceChat, ChannelQQ, ChannelWeChat:
		return true
	default:
		return false
	}
}

// Message contains all fields needed to deliver a notification.
type Message struct {
	// Channel specifies the delivery method; empty sends to all enabled channels.
	Channel Channel
	// Source identifies the notification configuration name.
	Source string
	// Type categorizes the notification (download, organize, subscribe, etc.).
	Type Type
	// ContentType specifies the exact event.
	ContentType ContentType
	// Title is the notification heading.
	Title string
	// Text is the main content.
	Text string
	// Image is an optional poster or backdrop URL.
	Image string
	// Link is an optional URL for user action.
	Link string
	// UserID targets a specific user; empty sends to default recipients.
	UserID string
	// Username is the display name for the user.
	Username string
	// ParseMode selects the V2 per-message Telegram formatting mode. Empty
	// keeps the sender default; recognized values are MarkdownV2/mdv2 and HTML.
	ParseMode string
	// DisableWebPagePreview preserves V2's tri-state Telegram option. Nil lets
	// Telegram apply its default; non-nil values are sent unchanged.
	DisableWebPagePreview *bool `json:",omitempty"`
	// Immediately mirrors V2 `MessageQueueManager.send_message(immediately=...)`:
	// it bypasses the `NotificationSendTime` allow-window and sends right away.
	Immediately bool
	// Date is the event timestamp.
	Date time.Time
}

// Sender delivers notifications to configured channels.
type Sender interface {
	// Send delivers the message to all enabled channels matching the filter.
	// Returns the first critical error; partial failures are logged but not returned.
	Send(ctx context.Context, msg Message) error
}

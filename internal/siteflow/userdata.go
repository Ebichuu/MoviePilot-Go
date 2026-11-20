// Package siteflow implements V2 site refresh side effects.
package siteflow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"time"

	"github.com/google/uuid"

	"github.com/moviepilot-go/moviepilot-go/internal/indexer"
	"github.com/moviepilot-go/moviepilot-go/internal/notification"
	"github.com/moviepilot-go/moviepilot-go/internal/notificationhistory"
)

var (
	errInvalidRefresh = errors.New("站点刷新副作用参数无效")
	vipLevelPattern   = regexp.MustCompile(`(?i)(贵宾|VIP?)`)
)

// HistoryStore persists one V2 notification-history event before delivery.
type HistoryStore interface {
	PublishNotificationHistory(context.Context, notificationhistory.Plan) (bool, error)
}

// RefreshedPublisher enqueues the V2 SiteRefreshed event boundary.
type RefreshedPublisher interface {
	PublishSiteRefreshed(context.Context, int)
}

// Service applies the side effects that V2 runs after a successful snapshot save.
type Service struct {
	history   HistoryStore
	sender    notification.Sender
	publisher RefreshedPublisher
	logger    *slog.Logger
	now       func() time.Time
	newID     func() string
}

// NewService builds a site refresh side-effect service.
func NewService(
	history HistoryStore,
	sender notification.Sender,
	publisher RefreshedPublisher,
	logger *slog.Logger,
) (*Service, error) {
	if history == nil || sender == nil {
		return nil, errInvalidRefresh
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{
		history: history, sender: sender, publisher: publisher, logger: logger,
		now: time.Now, newID: uuid.NewString,
	}, nil
}

// AfterSiteUserDataRefresh reproduces V2's event, unread-message, and ratio-warning order.
func (s *Service) AfterSiteUserDataRefresh(
	ctx context.Context,
	siteID int,
	siteName string,
	siteURL string,
	data indexer.SiteUserData,
) error {
	if ctx == nil || s == nil || s.history == nil || s.sender == nil || siteID <= 0 {
		return errInvalidRefresh
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.publisher != nil {
		s.publisher.PublishSiteRefreshed(ctx, siteID)
	}
	if err := s.postUnreadMessages(ctx, siteName, siteURL, data); err != nil {
		return err
	}
	if data.Ratio != 0 && data.Ratio < 1 && !vipLevelPattern.MatchString(stringValue(data.UserLevel)) {
		return s.postMessage(ctx, "", notification.Message{
			Type:  notification.TypeSiteMessage,
			Title: "【站点分享率低预警】",
			Text:  fmt.Sprintf("站点 %s 分享率 %v，请注意！", siteName, data.Ratio),
		})
	}
	return nil
}

func (s *Service) postUnreadMessages(
	ctx context.Context,
	siteName string,
	siteURL string,
	data indexer.SiteUserData,
) error {
	if data.MessageUnread == 0 {
		return nil
	}
	if len(data.MessageUnreadContents) == 0 {
		return s.postMessage(ctx, "", notification.Message{
			Type: notification.TypeSiteMessage,
			Title: fmt.Sprintf(
				"站点 %s 收到 %d 条新消息，请登陆查看",
				siteName,
				data.MessageUnread,
			),
			Link: siteURL,
		})
	}
	for _, fields := range data.MessageUnreadContents {
		if len(fields) < 3 {
			continue
		}
		source := ""
		if len(fields) > 3 && fields[3] != nil {
			source = fmt.Sprint(fields[3])
		}
		if err := s.postMessage(ctx, source, notification.Message{
			Type:  notification.TypeSiteMessage,
			Title: fmt.Sprintf("【站点 %s 消息】", siteName),
			Text: fmt.Sprintf(
				"时间：%s\n标题：%s\n内容：\n%s",
				pythonString(fields[1]),
				pythonString(fields[0]),
				pythonString(fields[2]),
			),
			Link: siteURL,
		}); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) postMessage(ctx context.Context, businessSource string, message notification.Message) error {
	now := s.now()
	message.Date = now
	message.Source = businessSource
	eventKey := "site-message:" + s.newID()
	var source *string
	if businessSource != "" {
		source = &businessSource
		digest := sha256.Sum256([]byte(businessSource))
		eventKey = "site-message-source:" + hex.EncodeToString(digest[:])
	}
	mtype, title := string(message.Type), message.Title
	plan := notificationhistory.Plan{
		EventKey:    eventKey,
		Source:      source,
		MType:       &mtype,
		Title:       &title,
		PublishedAt: &now,
	}
	if message.Text != "" {
		text := message.Text
		plan.Text = &text
	}
	if message.Link != "" {
		link := message.Link
		plan.Link = &link
	}
	created, err := s.history.PublishNotificationHistory(ctx, plan)
	if err != nil || !created {
		return err
	}
	if err := s.sender.Send(ctx, message); err != nil {
		s.logger.WarnContext(ctx, "站点通知投递失败", "type", message.Type)
	}
	return nil
}

func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func pythonString(value any) string {
	if value == nil {
		return "None"
	}
	if boolean, ok := value.(bool); ok {
		if boolean {
			return "True"
		}
		return "False"
	}
	return fmt.Sprint(value)
}

package siteflow

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"testing"
	"time"

	"github.com/moviepilot-go/moviepilot-go/internal/indexer"
	"github.com/moviepilot-go/moviepilot-go/internal/notification"
	"github.com/moviepilot-go/moviepilot-go/internal/notificationhistory"
)

func TestServiceAfterSiteUserDataRefreshMatchesV2OrderAndDedup(t *testing.T) {
	operations := make([]string, 0, 16)
	history := &siteHistoryFake{seen: map[string]struct{}{}, operations: &operations}
	sender := &siteSenderFake{operations: &operations}
	publisher := &sitePublisherFake{operations: &operations}
	service := newSiteFlowTestService(t, history, sender, publisher)
	level := "Power User"
	data := indexer.SiteUserData{
		Ratio: 0.5, UserLevel: &level, MessageUnread: 2,
		MessageUnreadContents: [][]any{
			{"标题一", "2026-08-16 10:00:00", "正文一", "sunnypt-message:9"},
			{"标题二", nil, "正文二"},
		},
	}

	if err := service.AfterSiteUserDataRefresh(
		t.Context(), 7, "SunnyPT", "https://sunnypt.top", data,
	); err != nil {
		t.Fatal(err)
	}
	wantOperations := []string{"event", "history", "send", "history", "send", "history", "send"}
	if !reflect.DeepEqual(operations, wantOperations) {
		t.Fatalf("operations = %#v, want %#v", operations, wantOperations)
	}
	if !reflect.DeepEqual(publisher.siteIDs, []int{7}) || len(history.plans) != 3 || len(sender.messages) != 3 {
		t.Fatalf("publisher=%#v history=%d sender=%d", publisher.siteIDs, len(history.plans), len(sender.messages))
	}
	first := history.plans[0]
	if first.Source == nil || *first.Source != "sunnypt-message:9" || first.Title == nil ||
		*first.Title != "【站点 SunnyPT 消息】" || first.Text == nil ||
		*first.Text != "时间：2026-08-16 10:00:00\n标题：标题一\n内容：\n正文一" ||
		first.Link == nil || *first.Link != "https://sunnypt.top" {
		t.Fatalf("first history = %#v", first)
	}
	if got := sender.messages[0].Source; got != "sunnypt-message:9" {
		t.Fatalf("first delivery source = %q, want %q", got, "sunnypt-message:9")
	}
	if second := history.plans[1]; second.Source != nil || second.Text == nil ||
		*second.Text != "时间：None\n标题：标题二\n内容：\n正文二" {
		t.Fatalf("second history = %#v", second)
	}
	if got := sender.messages[1].Source; got != "" {
		t.Fatalf("second delivery source = %q, want empty", got)
	}
	if warning := sender.messages[2]; warning.Source != "" || warning.Type != notification.TypeSiteMessage ||
		warning.Title != "【站点分享率低预警】" || warning.Text != "站点 SunnyPT 分享率 0.5，请注意！" {
		t.Fatalf("warning = %#v", warning)
	}

	operations = operations[:0]
	if err := service.AfterSiteUserDataRefresh(
		t.Context(), 7, "SunnyPT", "https://sunnypt.top", data,
	); err != nil {
		t.Fatal(err)
	}
	if len(sender.messages) != 5 || len(history.plans) != 6 {
		t.Fatalf("重复刷新后 history=%d sender=%d, want 6/5", len(history.plans), len(sender.messages))
	}
	if !reflect.DeepEqual(operations, []string{"event", "history", "history", "send", "history", "send"}) {
		t.Fatalf("重复刷新 operations = %#v", operations)
	}
}

func TestServiceAfterSiteUserDataRefreshMessageAndRatioBoundaries(t *testing.T) {
	tests := []struct {
		name      string
		data      indexer.SiteUserData
		wantTitle string
		wantCount int
	}{
		{name: "zero unread ignores contents", data: indexer.SiteUserData{
			Ratio: 2, MessageUnreadContents: [][]any{{"title", "date", "body"}},
		}},
		{name: "summary without contents", data: indexer.SiteUserData{MessageUnread: 3},
			wantTitle: "站点 Tracker 收到 3 条新消息，请登陆查看", wantCount: 1},
		{name: "zero ratio has no warning", data: indexer.SiteUserData{}},
		{name: "ratio one has no warning", data: indexer.SiteUserData{Ratio: 1}},
		{name: "VIP skips warning", data: userDataRatioLevel(0.5, "vip")},
		{name: "VI skips warning", data: userDataRatioLevel(0.5, "VI")},
		{name: "Chinese VIP skips warning", data: userDataRatioLevel(0.5, "贵宾用户")},
		{name: "ordinary level warns", data: userDataRatioLevel(0.5, "User"),
			wantTitle: "【站点分享率低预警】", wantCount: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			history := &siteHistoryFake{seen: map[string]struct{}{}}
			sender := &siteSenderFake{}
			service := newSiteFlowTestService(t, history, sender, nil)
			if err := service.AfterSiteUserDataRefresh(
				t.Context(), 1, "Tracker", "https://tracker.example", tt.data,
			); err != nil {
				t.Fatal(err)
			}
			if len(sender.messages) != tt.wantCount || len(history.plans) != tt.wantCount {
				t.Fatalf("history=%d sender=%d, want %d", len(history.plans), len(sender.messages), tt.wantCount)
			}
			if tt.wantCount > 0 {
				if got := sender.messages[0].Title; got != tt.wantTitle {
					t.Fatalf("title = %q, want %q", got, tt.wantTitle)
				}
			}
		})
	}
}

func TestServiceAfterSiteUserDataRefreshFailureSemantics(t *testing.T) {
	sentinel := errors.New("postgres unavailable")
	tests := []struct {
		name       string
		historyErr error
		senderErr  error
		wantErr    error
		wantSends  int
	}{
		{name: "history error propagates", historyErr: sentinel, wantErr: sentinel},
		{name: "delivery error is ignored", senderErr: sentinel, wantSends: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			history := &siteHistoryFake{seen: map[string]struct{}{}, err: tt.historyErr}
			sender := &siteSenderFake{err: tt.senderErr}
			service := newSiteFlowTestService(t, history, sender, nil)
			err := service.AfterSiteUserDataRefresh(
				t.Context(), 1, "Tracker", "https://tracker.example", indexer.SiteUserData{MessageUnread: 1},
			)
			if !errors.Is(err, tt.wantErr) || len(sender.messages) != tt.wantSends {
				t.Fatalf("error=%v sends=%d, want %v/%d", err, len(sender.messages), tt.wantErr, tt.wantSends)
			}
		})
	}
}

func newSiteFlowTestService(
	t *testing.T,
	history *siteHistoryFake,
	sender *siteSenderFake,
	publisher RefreshedPublisher,
) *Service {
	t.Helper()
	service, err := NewService(
		history,
		sender,
		publisher,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC) }
	next := 0
	service.newID = func() string {
		next++
		return fmt.Sprintf("event-%d", next)
	}
	return service
}

func userDataRatioLevel(ratio float64, level string) indexer.SiteUserData {
	return indexer.SiteUserData{Ratio: ratio, UserLevel: &level}
}

type siteHistoryFake struct {
	plans      []notificationhistory.Plan
	seen       map[string]struct{}
	err        error
	operations *[]string
}

func (f *siteHistoryFake) PublishNotificationHistory(
	_ context.Context,
	plan notificationhistory.Plan,
) (bool, error) {
	if f.operations != nil {
		*f.operations = append(*f.operations, "history")
	}
	f.plans = append(f.plans, plan.Clone())
	if f.err != nil {
		return false, f.err
	}
	if _, exists := f.seen[plan.EventKey]; exists {
		return false, nil
	}
	f.seen[plan.EventKey] = struct{}{}
	return true, nil
}

type siteSenderFake struct {
	messages   []notification.Message
	err        error
	operations *[]string
}

func (f *siteSenderFake) Send(_ context.Context, message notification.Message) error {
	if f.operations != nil {
		*f.operations = append(*f.operations, "send")
	}
	f.messages = append(f.messages, message)
	return f.err
}

type sitePublisherFake struct {
	siteIDs    []int
	operations *[]string
}

func (f *sitePublisherFake) PublishSiteRefreshed(_ context.Context, siteID int) {
	if f.operations != nil {
		*f.operations = append(*f.operations, "event")
	}
	f.siteIDs = append(f.siteIDs, siteID)
}

//go:build integration

package database

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/moviepilot-go/moviepilot-go/internal/notificationhistory"
)

func TestPublishNotificationHistoryPostgresIsIdempotent(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	publishedAt := time.Date(2026, 8, 16, 12, 34, 56, 0, time.UTC)
	source := "sunnypt-message:" + uuid.NewString()
	mtype, title, text := "站点", "【站点 SunnyPT 消息】", "正文"
	plan := notificationhistory.Plan{
		EventKey: "site-message-source:" + uuid.NewString(),
		Source:   &source, MType: &mtype, Title: &title, Text: &text,
		PublishedAt: &publishedAt,
	}

	created, err := store.PublishNotificationHistory(t.Context(), plan)
	if err != nil || !created {
		t.Fatalf("首次 PublishNotificationHistory() = %v, %v", created, err)
	}
	created, err = store.PublishNotificationHistory(t.Context(), plan)
	if err != nil || created {
		t.Fatalf("重复 PublishNotificationHistory() = %v, %v", created, err)
	}
	events, err := store.ListNotificationHistory(t.Context(), 1, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Source == nil || *events[0].Source != source ||
		events[0].MType == nil || *events[0].MType != mtype || events[0].Title == nil ||
		*events[0].Title != title || events[0].Text == nil || *events[0].Text != text ||
		events[0].PublishedAt == nil || !events[0].PublishedAt.Equal(publishedAt) {
		t.Fatalf("events = %#v", events)
	}
}

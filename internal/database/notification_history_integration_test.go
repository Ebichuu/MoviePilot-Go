//go:build integration

package database

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sync/errgroup"

	"github.com/moviepilot-go/moviepilot-go/internal/notificationhistory"
)

func TestNotificationHistoryPostgresPaginationAndAtomicClear(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)
	fixtures := []struct {
		key       string
		title     string
		image     *string
		published *time.Time
	}{
		{key: "old-system", title: "旧系统", published: notificationHistoryTimePointer(base.Add(time.Minute))},
		{key: "old-media", title: "旧媒体", image: notificationHistoryStringPointer("old.jpg"), published: notificationHistoryTimePointer(base.Add(2 * time.Minute))},
		{key: "new-system", title: "新系统", published: notificationHistoryTimePointer(base.Add(3 * time.Minute))},
		{key: "new-media", title: "新媒体", image: notificationHistoryStringPointer("new.jpg"), published: notificationHistoryTimePointer(base.Add(4 * time.Minute))},
		{key: "blocked", title: "未发布", published: nil},
	}
	for _, fixture := range fixtures {
		if _, err := store.db.Pool.Exec(t.Context(), `
			INSERT INTO notification_events
				(event_key, channel, title, image, note, published_at, created_at)
			VALUES ($1, '', $2, $3, '{}'::jsonb, $4, $5)`,
			fixture.key, fixture.title, fixture.image, fixture.published, base,
		); err != nil {
			t.Fatalf("插入历史 fixture %q: %v", fixture.key, err)
		}
	}

	firstPage, err := store.ListNotificationHistory(t.Context(), 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	secondPage, err := store.ListNotificationHistory(t.Context(), 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	if got := notificationHistoryTitles(firstPage); !equalNotificationHistoryTitles(got, []string{"新媒体", "新系统"}) {
		t.Fatalf("first page = %v", got)
	}
	if got := notificationHistoryTitles(secondPage); !equalNotificationHistoryTitles(got, []string{"旧媒体", "旧系统"}) {
		t.Fatalf("second page = %v", got)
	}

	allMillis := base.Add(30 * time.Second).UnixMilli()
	if _, err := store.UpdateNotificationClearBefore(t.Context(), notificationhistory.ScopeAll, allMillis); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	var group errgroup.Group
	for scope, millis := range map[notificationhistory.Scope]int64{
		notificationhistory.ScopeSystem: base.Add(150 * time.Second).UnixMilli(),
		notificationhistory.ScopeMedia:  base.Add(210 * time.Second).UnixMilli(),
	} {
		scope, millis := scope, millis
		group.Go(func() error {
			<-start
			_, updateErr := store.UpdateNotificationClearBefore(t.Context(), scope, millis)
			return updateErr
		})
	}
	close(start)
	if err := group.Wait(); err != nil {
		t.Fatal(err)
	}
	clearBefore, err := store.LoadNotificationClearBefore(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if clearBefore != (notificationhistory.ClearBefore{
		All:    allMillis,
		System: base.Add(150 * time.Second).UnixMilli(),
		Media:  base.Add(210 * time.Second).UnixMilli(),
	}) {
		t.Fatalf("clear before = %#v", clearBefore)
	}

	visible, err := store.ListNotificationHistory(t.Context(), 1, 20)
	if err != nil {
		t.Fatal(err)
	}
	if got := notificationHistoryTitles(visible); !equalNotificationHistoryTitles(got, []string{"新媒体", "新系统"}) {
		t.Fatalf("visible after scoped clear = %v", got)
	}
}

func TestNotificationHistoryPostgresAllowsSameChannelDifferentSources(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	username := "notification-history-" + uuid.NewString()
	if _, err := store.db.Pool.Exec(t.Context(), `
		INSERT INTO users (username, password_hash) VALUES ($1, 'fixture')`, username); err != nil {
		t.Fatal(err)
	}
	var eventID int64
	if err := store.db.Pool.QueryRow(t.Context(), `
		INSERT INTO notification_events (event_key, note, published_at)
		VALUES ($1, '{}'::jsonb, now())
		RETURNING id`, "event-"+uuid.NewString()).Scan(&eventID); err != nil {
		t.Fatal(err)
	}
	for _, source := range []string{"家庭通知", "管理通知"} {
		if _, err := store.db.Pool.Exec(t.Context(), `
			INSERT INTO notifications
				(id, username, channel, source, event_id, idempotency_key, state)
			VALUES ($1, $2, 'telegram', $3, $4, 'same-event', 'skipped')`,
			uuid.New(), username, source, eventID,
		); err != nil {
			t.Fatalf("插入 source %q: %v", source, err)
		}
	}
	if _, err := store.db.Pool.Exec(t.Context(), `
		INSERT INTO notifications
			(id, username, channel, source, event_id, idempotency_key, state)
		VALUES ($1, $2, 'telegram', '家庭通知', $3, 'same-event', 'skipped')`,
		uuid.New(), username, eventID,
	); err == nil {
		t.Fatal("相同 channel/source/idempotency_key 应触发唯一约束")
	}
	if _, err := store.db.Pool.Exec(t.Context(), `
		UPDATE notifications SET source='changed'
		WHERE username=$1 AND source='家庭通知'`, username); err == nil {
		t.Fatal("source 更新应触发不可变计划约束")
	}
}

func notificationHistoryTitles(events []notificationhistory.Event) []string {
	titles := make([]string, 0, len(events))
	for _, event := range events {
		if event.Title == nil {
			titles = append(titles, "")
			continue
		}
		titles = append(titles, *event.Title)
	}
	return titles
}

func equalNotificationHistoryTitles(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func notificationHistoryStringPointer(value string) *string {
	return &value
}

func notificationHistoryTimePointer(value time.Time) *time.Time {
	return &value
}

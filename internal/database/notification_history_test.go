package database

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/moviepilot-go/moviepilot-go/internal/notificationhistory"
)

func TestNotificationHistoryMigrationContract(t *testing.T) {
	t.Parallel()

	content, err := migrationFS.ReadFile("migrations/025_notification_history.sql")
	if err != nil {
		t.Fatal(err)
	}
	sql := strings.Join(strings.Fields(string(content)), " ")
	for _, fragment := range []string{
		"CREATE TABLE notification_events",
		"batch_id UUID UNIQUE REFERENCES organization_batches(id) ON DELETE SET NULL",
		"event_key TEXT NOT NULL UNIQUE",
		"note JSONB NOT NULL DEFAULT '{}'::jsonb",
		"ADD COLUMN event_id BIGINT REFERENCES notification_events(id)",
		"ADD COLUMN source TEXT NOT NULL DEFAULT ''",
		"UNIQUE (channel, source, idempotency_key)",
		"NEW.event_id IS DISTINCT FROM OLD.event_id",
		"NEW.source IS DISTINCT FROM OLD.source",
		"CREATE INDEX notification_events_published_idx",
		"DROP TABLE notification_events",
		"UNIQUE (channel, idempotency_key)",
	} {
		if !strings.Contains(sql, fragment) {
			t.Fatalf("migration missing %q", fragment)
		}
	}

	downAt := strings.Index(sql, "-- +goose Down")
	if downAt < 0 {
		t.Fatal("migration missing Down section")
	}
	down := sql[downAt:]
	functionAt := strings.Index(down, "CREATE OR REPLACE FUNCTION prevent_notification_plan_mutation()")
	dropColumnsAt := strings.Index(down, "DROP COLUMN event_id")
	if functionAt < 0 || dropColumnsAt < 0 || functionAt > dropColumnsAt {
		t.Fatal("Down must restore immutable trigger before dropping event_id/source")
	}
}

func TestDecodeNotificationClearBefore(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		raw  []byte
		want notificationhistory.ClearBefore
	}{
		{name: "空配置", raw: nil},
		{name: "对象", raw: []byte(`{"all":1000,"system":"2000","media":3000}`), want: notificationhistory.ClearBefore{All: 1000, System: 2000, Media: 3000}},
		{name: "旧标量", raw: []byte(`1234`), want: notificationhistory.ClearBefore{All: 1234}},
		{name: "旧字符串标量", raw: []byte(`"1234"`), want: notificationhistory.ClearBefore{All: 1234}},
		{name: "非法和负值归零", raw: []byte(`{"all":-1,"system":"bad","media":null}`)},
		{name: "布尔值兼容原版 int", raw: []byte(`{"all":true}`), want: notificationhistory.ClearBefore{All: 1}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := decodeNotificationClearBefore(tt.raw)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("decodeNotificationClearBefore(%s) = %#v, want %#v", tt.raw, got, tt.want)
			}
		})
	}

	if _, err := decodeNotificationClearBefore([]byte(`{`)); !errors.Is(err, notificationhistory.ErrPersistence) {
		t.Fatalf("corrupt JSON error = %v, want ErrPersistence", err)
	}
}

func TestScanNotificationHistoryEvent(t *testing.T) {
	t.Parallel()

	publishedAt := time.Date(2026, time.August, 14, 1, 2, 3, 0, time.UTC)
	event, err := scanNotificationHistoryEvent(scannerFunc(func(dest ...any) error {
		*dest[0].(*int64) = 7
		*dest[1].(*string) = "event-7"
		*dest[2].(*string) = ""
		*dest[3].(**string) = nil
		*dest[4].(**string) = stringPointerForNotificationHistory("整理入库")
		*dest[5].(**string) = stringPointerForNotificationHistory("整理完成")
		*dest[6].(**string) = nil
		*dest[7].(**string) = nil
		*dest[8].(**string) = nil
		*dest[9].(**string) = nil
		*dest[10].(*[]byte) = []byte(`{"fixture":true}`)
		*dest[11].(**time.Time) = &publishedAt
		*dest[12].(*time.Time) = publishedAt.Add(-time.Minute)
		return nil
	}))
	if err != nil || !event.Valid() || event.ID != 7 || string(event.Note) != `{"fixture":true}` {
		t.Fatalf("scanNotificationHistoryEvent() = %#v, %v", event, err)
	}

	if _, err := scanNotificationHistoryEvent(scannerFunc(func(dest ...any) error {
		*dest[0].(*int64) = 7
		*dest[1].(*string) = "event-7"
		*dest[2].(*string) = ""
		*dest[10].(*[]byte) = []byte(`1`)
		*dest[11].(**time.Time) = &publishedAt
		*dest[12].(*time.Time) = publishedAt
		return nil
	})); !errors.Is(err, notificationhistory.ErrPersistence) {
		t.Fatalf("corrupt event error = %v, want ErrPersistence", err)
	}
}

func TestNotificationHistoryStoreValidation(t *testing.T) {
	t.Parallel()

	var store *Store
	tests := []struct {
		name string
		call func() error
		want error
	}{
		{name: "列表拒绝 nil context", call: func() error {
			_, err := store.ListNotificationHistory(nil, 1, 20)
			return err
		}, want: notificationhistory.ErrPersistence},
		{name: "列表先拒绝非法分页", call: func() error {
			_, err := store.ListNotificationHistory(context.Background(), 0, 20)
			return err
		}, want: notificationhistory.ErrInvalidPage},
		{name: "读取拒绝 nil store", call: func() error {
			_, err := store.LoadNotificationClearBefore(context.Background())
			return err
		}, want: notificationhistory.ErrPersistence},
		{name: "更新拒绝非法 scope", call: func() error {
			_, err := store.UpdateNotificationClearBefore(context.Background(), notificationhistory.Scope("other"), 1)
			return err
		}, want: notificationhistory.ErrInvalidScope},
		{name: "更新拒绝非正时间", call: func() error {
			_, err := store.UpdateNotificationClearBefore(context.Background(), notificationhistory.ScopeAll, 0)
			return err
		}, want: notificationhistory.ErrInvalidClearBefore},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if err := tt.call(); !errors.Is(err, tt.want) {
				t.Fatalf("error = %v, want %v", err, tt.want)
			}
		})
	}
}

func stringPointerForNotificationHistory(value string) *string {
	return &value
}

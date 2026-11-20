package database

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/moviepilot-go/moviepilot-go/internal/notificationflow"
)

func TestNotificationRequestMigrationContract(t *testing.T) {
	t.Parallel()
	content, err := migrationFS.ReadFile("migrations/023_scrape_notification_requests.sql")
	if err != nil {
		t.Fatal(err)
	}
	sql := strings.Join(strings.Fields(string(content)), " ")
	for _, fragment := range []string{
		"ADD COLUMN IF NOT EXISTS batch_id UUID",
		"state IN ('blocked', 'pending', 'working', 'failed', 'sent', 'skipped')",
		"CREATE TRIGGER notification_plan_immutable",
		"WHERE state IN ('pending', 'failed') AND attempt_count < 5",
		"legacy_state=state, legacy_last_error=last_error, legacy_sent_at=sent_at",
		"SET state=legacy_state, last_error=legacy_last_error, sent_at=legacy_sent_at",
		"ALTER COLUMN last_error DROP NOT NULL",
		"ALTER COLUMN last_error DROP DEFAULT",
	} {
		if !strings.Contains(sql, fragment) {
			t.Fatalf("migration missing %q", fragment)
		}
	}
	initial, err := migrationFS.ReadFile("migrations/001_initial.sql")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(strings.Fields(string(initial)), " "), "UNIQUE (channel, idempotency_key)") {
		t.Fatal("initial notifications table lost its idempotency constraint")
	}
}

func TestNotificationRequestStoreValidation(t *testing.T) {
	t.Parallel()
	var store *Store
	requestID := uuid.New()
	now := time.Now().UTC()
	tests := []struct {
		name string
		call func() error
		want error
	}{
		{name: "nil context", call: func() error {
			_, _, err := store.ClaimNotification(nil, requestID, now, time.Minute)
			return err
		}, want: notificationflow.ErrPersistence},
		{name: "empty request", call: func() error {
			_, _, err := store.ClaimNotification(context.Background(), uuid.Nil, now, time.Minute)
			return err
		}, want: notificationflow.ErrInvalidPlan},
		{name: "invalid list limit", call: func() error {
			_, err := store.ListDueNotifications(context.Background(), now, notificationflow.SweepPageSize+1)
			return err
		}, want: notificationflow.ErrInvalidPlan},
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

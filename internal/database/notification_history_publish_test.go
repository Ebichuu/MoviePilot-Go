package database

import (
	"errors"
	"testing"
	"time"

	"github.com/moviepilot-go/moviepilot-go/internal/notificationhistory"
)

func TestPublishNotificationHistoryRejectsInvalidInputs(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name  string
		store *Store
		plan  notificationhistory.Plan
		want  error
	}{
		{name: "invalid plan", store: &Store{}, plan: notificationhistory.Plan{}, want: notificationhistory.ErrInvalidPlan},
		{name: "unpublished plan", store: &Store{}, plan: notificationhistory.Plan{EventKey: "event"}, want: notificationhistory.ErrInvalidPlan},
		{name: "unavailable store", store: &Store{}, plan: notificationhistory.Plan{
			EventKey: "event", PublishedAt: &now,
		}, want: notificationhistory.ErrPersistence},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			created, err := tt.store.PublishNotificationHistory(t.Context(), tt.plan)
			if created || !errors.Is(err, tt.want) {
				t.Fatalf("PublishNotificationHistory() = %v, %v, want false, %v", created, err, tt.want)
			}
		})
	}
}

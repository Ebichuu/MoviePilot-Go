package database

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

func TestBuildCompareAndSwapSubscriptionStateByIntent(t *testing.T) {
	t.Parallel()
	intentID := uuid.MustParse("55555555-6666-4777-8888-999999999999")
	tests := []struct {
		name     string
		username string
		intentID uuid.UUID
		current  subscription.State
		next     subscription.State
		wantArgs []any
		wantErr  error
	}{
		{
			name: "download confirmed", username: " alice ", intentID: intentID,
			current: subscription.Acquired, next: subscription.Downloading,
			wantArgs: []any{intentID, "alice", subscription.Downloading, "R", subscription.Acquired},
		},
		{
			name: "download completed", username: "alice", intentID: intentID,
			current: subscription.Downloading, next: subscription.PendingOrganization,
			wantArgs: []any{intentID, "alice", subscription.PendingOrganization, "R", subscription.Downloading},
		},
		{name: "empty owner", intentID: intentID, current: subscription.Acquired, next: subscription.Downloading, wantErr: ErrInvalidSubscriptionDownloadState},
		{name: "empty intent", username: "alice", current: subscription.Acquired, next: subscription.Downloading, wantErr: ErrInvalidSubscriptionDownloadState},
		{name: "illegal transition", username: "alice", intentID: intentID, current: subscription.Acquired, next: subscription.Indexed, wantErr: ErrInvalidSubscriptionDownloadState},
		{name: "same state", username: "alice", intentID: intentID, current: subscription.Downloading, next: subscription.Downloading, wantErr: ErrInvalidSubscriptionDownloadState},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			query, args, err := buildCompareAndSwapSubscriptionStateByIntent(
				tt.username, tt.intentID, tt.current, tt.next,
			)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("build error = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(args, tt.wantArgs) {
				t.Fatalf("args = %#v, want %#v", args, tt.wantArgs)
			}
			compact := strings.Join(strings.Fields(query), " ")
			for _, required := range []string{
				"di.id=$1 AND di.username=$2 AND di.subscription_id=s.id",
				"s.username=$2 AND s.completed_at IS NULL",
				"s.current_download_intent_id=di.id AND s.internal_state=$5",
			} {
				if !strings.Contains(compact, required) {
					t.Fatalf("query missing %q: %s", required, compact)
				}
			}
		})
	}
}

func TestSubscriptionDownloadStatePersistenceErrorsAreSanitized(t *testing.T) {
	t.Parallel()
	var store *Store
	err := store.CompareAndSwapSubscriptionStateByIntent(
		t.Context(),
		"sensitive-owner",
		uuid.MustParse("66666666-7777-4888-8999-aaaaaaaaaaaa"),
		subscription.Acquired,
		subscription.Downloading,
	)
	if !errors.Is(err, ErrSubscriptionDownloadStatePersistence) || strings.Contains(err.Error(), "sensitive-owner") {
		t.Fatalf("sanitized error = %v", err)
	}
}

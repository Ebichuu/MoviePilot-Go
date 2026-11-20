//go:build integration

package database

import (
	"errors"
	"testing"

	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

func TestSubscriptionStorePostgresSetV2State(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	insertSubscriptionIntegrationUsers(t, store, "state-owner", "state-other")

	tests := []struct {
		name              string
		mediaID           string
		caller            string
		controlState      string
		startCompleted    bool
		wantErr           bool
		wantInternalState subscription.State
		wantV2State       string
	}{
		{
			name:              "P 恢复为待识别",
			mediaID:           "state-control-p",
			caller:            "state-owner",
			controlState:      "P",
			wantInternalState: subscription.PendingIdentification,
			wantV2State:       "P",
		},
		{
			name:              "R 恢复为 Active",
			mediaID:           "state-control-r",
			caller:            "state-owner",
			controlState:      "R",
			wantInternalState: subscription.Active,
			wantV2State:       "R",
		},
		{
			name:              "S 暂停订阅",
			mediaID:           "state-control-s",
			caller:            "state-owner",
			controlState:      "S",
			wantInternalState: subscription.Paused,
			wantV2State:       "S",
		},
		{
			name:              "非所有者不能更新",
			mediaID:           "state-control-non-owner",
			caller:            "state-other",
			controlState:      "R",
			wantErr:           true,
			wantInternalState: subscription.PendingSearch,
			wantV2State:       "N",
		},
		{
			name:              "Completed 不可恢复",
			mediaID:           "state-control-completed",
			caller:            "state-owner",
			controlState:      "R",
			startCompleted:    true,
			wantErr:           true,
			wantInternalState: subscription.Completed,
			wantV2State:       "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id := createSubscriptionIntegrationFixture(t, store, "state-owner", tt.mediaID)
			if tt.startCompleted {
				result, err := store.db.Pool.Exec(t.Context(), `
					UPDATE subscriptions
					SET internal_state=$1, v2_state=$2, completed_at=now()
					WHERE id=$3`, subscription.Completed, "", id)
				if err != nil {
					t.Fatalf("准备 Completed 订阅: %v", err)
				}
				if result.RowsAffected() != 1 {
					t.Fatalf("准备 Completed 订阅影响行数 = %d, want 1", result.RowsAffected())
				}
			}

			err := store.SetSubscriptionV2State(t.Context(), id, tt.caller, false, tt.controlState)
			if tt.wantErr {
				if !errors.Is(err, ErrSubscriptionNotFound) {
					t.Fatalf("SetSubscriptionV2State() error = %v, want ErrSubscriptionNotFound", err)
				}
			} else if err != nil {
				t.Fatalf("SetSubscriptionV2State() error = %v", err)
			}

			var items []Subscription
			if tt.startCompleted {
				items, err = store.ListCompletedSubscriptions(t.Context(), "state-owner", false)
			} else {
				items, err = store.ListSubscriptions(t.Context(), "state-owner", false)
			}
			if err != nil {
				t.Fatalf("读取更新后的订阅: %v", err)
			}
			item, found := findSubscriptionByID(items, id)
			if !found {
				t.Fatalf("未找到订阅 id=%d", id)
			}
			if item.InternalState != tt.wantInternalState || item.V2State != tt.wantV2State {
				t.Fatalf("更新后状态 = (%q, %q), want (%q, %q)",
					item.InternalState, item.V2State, tt.wantInternalState, tt.wantV2State)
			}
		})
	}
}

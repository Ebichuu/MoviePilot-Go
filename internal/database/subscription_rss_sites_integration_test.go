//go:build integration

package database

import (
	"reflect"
	"testing"

	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

func TestGetSubscriptionRSSSitesPostgres(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(*testing.T, *Store)
		want    SubscriptionRSSSitesSnapshot
	}{
		{
			name: "无订阅",
			want: SubscriptionRSSSitesSnapshot{SiteSelections: [][]int{}},
		},
		{
			name: "只有完成历史",
			prepare: func(t *testing.T, store *Store) {
				id := createSubscriptionIntegrationFixture(t, store, "rss-sites-owner", "completed")
				if _, err := store.db.Pool.Exec(t.Context(), `
					UPDATE subscriptions
					SET internal_state=$1, v2_state='', completed_at=now(),
						config=jsonb_set(config, '{sites}', '[4,5]'::jsonb, true)
					WHERE id=$2`, subscription.Completed, id); err != nil {
					t.Fatalf("准备完成订阅: %v", err)
				}
			},
			want: SubscriptionRSSSitesSnapshot{SiteSelections: [][]int{}},
		},
		{
			name: "只有 N S 订阅",
			prepare: func(t *testing.T, store *Store) {
				pendingID := createSubscriptionIntegrationFixture(t, store, "rss-sites-owner", "pending")
				pausedID := createSubscriptionIntegrationFixture(t, store, "rss-sites-owner", "paused")
				setSubscriptionRSSSitesFixture(t, store, pendingID, subscription.PendingSearch, "N", `[1,2]`)
				setSubscriptionRSSSitesFixture(t, store, pausedID, subscription.Paused, "S", `[3,4]`)
			},
			want: SubscriptionRSSSitesSnapshot{
				HasSubscriptions: true,
				SiteSelections:   [][]int{},
			},
		},
		{
			name: "R P 空和非法 sites",
			prepare: func(t *testing.T, store *Store) {
				resolvedID := createSubscriptionIntegrationFixture(t, store, "rss-sites-owner", "resolved-empty")
				pendingID := createSubscriptionIntegrationFixture(t, store, "rss-sites-owner", "identified-invalid")
				setSubscriptionRSSSitesFixture(t, store, resolvedID, subscription.Active, "R", `[]`)
				setSubscriptionRSSSitesFixture(t, store, pendingID, subscription.PendingIdentification, "P", `"invalid"`)
			},
			want: SubscriptionRSSSitesSnapshot{
				HasSubscriptions: true,
				SiteSelections:   [][]int{{}},
			},
		},
		{
			name: "R P 多站点和重复选择",
			prepare: func(t *testing.T, store *Store) {
				firstID := createSubscriptionIntegrationFixture(t, store, "rss-sites-owner", "resolved-first")
				secondID := createSubscriptionIntegrationFixture(t, store, "rss-sites-owner", "identified-second")
				duplicateID := createSubscriptionIntegrationFixture(t, store, "rss-sites-owner", "resolved-duplicate")
				nonTargetID := createSubscriptionIntegrationFixture(t, store, "rss-sites-owner", "non-target")
				setSubscriptionRSSSitesFixture(t, store, firstID, subscription.Active, "R", `[3,1,3]`)
				setSubscriptionRSSSitesFixture(t, store, secondID, subscription.PendingIdentification, "P", `[9,7,9]`)
				setSubscriptionRSSSitesFixture(t, store, duplicateID, subscription.Active, "R", `[1,3]`)
				setSubscriptionRSSSitesFixture(t, store, nonTargetID, subscription.PendingSearch, "N", `[99]`)
			},
			want: SubscriptionRSSSitesSnapshot{
				HasSubscriptions: true,
				SiteSelections:   [][]int{{1, 3}, {7, 9}},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newSiteStatisticsIntegrationStore(t)
			insertSubscriptionIntegrationUsers(t, store, "rss-sites-owner")
			if tt.prepare != nil {
				tt.prepare(t, store)
			}

			got, err := store.GetSubscriptionRSSSites(t.Context())
			if err != nil {
				t.Fatalf("GetSubscriptionRSSSites() error = %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("GetSubscriptionRSSSites() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func setSubscriptionRSSSitesFixture(
	t *testing.T,
	store *Store,
	id int64,
	state subscription.State,
	v2State string,
	sitesJSON string,
) {
	t.Helper()
	if _, err := store.db.Pool.Exec(t.Context(), `
		UPDATE subscriptions
		SET internal_state=$1, v2_state=$2,
			config=jsonb_set(config, '{sites}', $3::jsonb, true)
		WHERE id=$4`, state, v2State, sitesJSON, id); err != nil {
		t.Fatalf("准备 RSS 订阅站点 fixture id=%d: %v", id, err)
	}
}

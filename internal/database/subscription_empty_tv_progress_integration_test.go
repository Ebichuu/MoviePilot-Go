//go:build integration

package database

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

func TestRecordEmptyTVSubscriptionProgressPostgresPreservesPAndFacts(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	const owner = "empty-tv-progress"
	insertSubscriptionIntegrationUsers(t, store, owner)
	for index, tc := range []struct {
		name      string
		mediaType string
		best      any
	}{
		{name: "ordinary boolean mode", mediaType: "电视剧", best: false},
		{name: "ordinary numeric mode and TV alias", mediaType: " TV ", best: 0},
		{name: "ordinary absent mode", mediaType: "tv"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := createTVSubmissionIntegrationParent(t, store, owner, fmt.Sprintf("empty-progress-%d", index), subscription.PendingIdentification, false)
			config := map[string]any{
				"total_episode": 0, "manual_total_episode": true, "start_episode": 2,
				"lack_episode": 7, "note": []int{9}, "current_priority": 90,
				"episode_priority": map[string]int{"2": 90}, "fixture": tc.name,
			}
			if tc.best != nil {
				config["best_version"] = tc.best
			}
			setTVQualityIntegrationConfig(t, store, id, config)
			version := time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC)
			if _, err := store.db.Pool.Exec(t.Context(), `UPDATE subscriptions SET media_type=$2, updated_at=$3 WHERE id=$1`, id, tc.mediaType, version); err != nil {
				t.Fatal(err)
			}
			before := mustActiveSubscription(t, store, owner, id)
			if err := store.RecordEmptyTVSubscriptionProgress(t.Context(), " "+owner+" ", id, subscription.PendingIdentification, before.UpdatedAt); err != nil {
				t.Fatal(err)
			}
			after := mustActiveSubscription(t, store, owner, id)
			if after.InternalState != before.InternalState || after.V2State != "P" || after.CompletedAt != nil || !after.UpdatedAt.After(before.UpdatedAt) {
				t.Fatalf("P state or completion changed: before=%+v after=%+v", before, after)
			}
			wantConfig := make(map[string]any, len(before.Config))
			for key, value := range before.Config {
				wantConfig[key] = value
			}
			wantConfig["lack_episode"] = float64(0)
			var currentIntent *uuid.UUID
			if err := store.db.Pool.QueryRow(t.Context(), `SELECT current_download_intent_id FROM subscriptions WHERE id=$1`, id).Scan(&currentIntent); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(after.Config, wantConfig) || !reflect.DeepEqual(after.Season, before.Season) || currentIntent != nil {
				t.Fatalf("progress update changed existing facts: before=%+v after=%+v", before, after)
			}
			var rows int
			if err := store.db.Pool.QueryRow(t.Context(), `SELECT count(*) FROM subscription_episodes WHERE subscription_id=$1`, id).Scan(&rows); err != nil || rows != 0 {
				t.Fatalf("empty target created episodes: count=%d error=%v", rows, err)
			}
			if err := store.RecordEmptyTVSubscriptionProgress(t.Context(), owner, id, subscription.PendingIdentification, before.UpdatedAt); !errors.Is(err, ErrSubscriptionPreciseSearchStateConflict) {
				t.Fatalf("stale progress replay error=%v", err)
			}
		})
	}
}

func TestRecordEmptyTVSubscriptionProgressPostgresGuards(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	const owner = "empty-tv-guard"
	insertSubscriptionIntegrationUsers(t, store, owner, "empty-tv-other")
	for index, tc := range []struct {
		name      string
		change    func(*testing.T, *Store, int64)
		owner     string
		missingID bool
		cancel    bool
		want      error
	}{
		{name: "foreign owner", owner: "empty-tv-other", want: ErrSubscriptionNotFound},
		{name: "absent id", missingID: true, want: ErrSubscriptionNotFound},
		{name: "stale version", change: func(t *testing.T, store *Store, id int64) {
			mustEmptyTVProgressMutation(t, store, `UPDATE subscriptions SET updated_at=updated_at + interval '1 second' WHERE id=$1`, id)
		}, want: ErrSubscriptionPreciseSearchStateConflict},
		{name: "internal state changed", change: func(t *testing.T, store *Store, id int64) {
			mustEmptyTVProgressMutation(t, store, `UPDATE subscriptions SET internal_state='active',v2_state='R' WHERE id=$1`, id)
		}, want: ErrSubscriptionPreciseSearchStateConflict},
		{name: "V2 state changed while internal state stays P", change: func(t *testing.T, store *Store, id int64) {
			mustEmptyTVProgressMutation(t, store, `UPDATE subscriptions SET v2_state='R' WHERE id=$1`, id)
		}, want: ErrSubscriptionPreciseSearchStateConflict},
		{name: "best boolean mode", change: func(t *testing.T, store *Store, id int64) {
			mustEmptyTVProgressMutation(t, store, `UPDATE subscriptions SET config=jsonb_set(config,'{best_version}','true') WHERE id=$1`, id)
		}, want: ErrSubscriptionPreciseSearchStateConflict},
		{name: "best positive integer mode", change: func(t *testing.T, store *Store, id int64) {
			mustEmptyTVProgressMutation(t, store, `UPDATE subscriptions SET config=jsonb_set(config,'{best_version}','1') WHERE id=$1`, id)
		}, want: ErrSubscriptionPreciseSearchStateConflict},
		{name: "best negative integer mode", change: func(t *testing.T, store *Store, id int64) {
			mustEmptyTVProgressMutation(t, store, `UPDATE subscriptions SET config=jsonb_set(config,'{best_version}','-1') WHERE id=$1`, id)
		}, want: ErrSubscriptionPreciseSearchStateConflict},
		{name: "movie type", change: func(t *testing.T, store *Store, id int64) {
			mustEmptyTVProgressMutation(t, store, `UPDATE subscriptions SET media_type='movie' WHERE id=$1`, id)
		}, want: ErrSubscriptionPreciseSearchStateConflict},
		{name: "current intent", change: func(t *testing.T, store *Store, id int64) {
			intent := createDownloadIntentAcceptanceIntent(t, store, owner, fmt.Sprintf("empty-progress-intent-%d", id))
			mustEmptyTVProgressMutation(t, store, `UPDATE subscriptions SET current_download_intent_id=$2 WHERE id=$1`, id, intent.ID)
		}, want: ErrSubscriptionPreciseSearchStateConflict},
		{name: "config is not an object", change: func(t *testing.T, store *Store, id int64) {
			mustEmptyTVProgressMutation(t, store, `UPDATE subscriptions SET config='[]'::jsonb WHERE id=$1`, id)
		}, want: ErrSubscriptionPreciseSearchStateConflict},
		{name: "already completed", change: func(t *testing.T, store *Store, id int64) {
			mustEmptyTVProgressMutation(t, store, `UPDATE subscriptions SET internal_state='completed',v2_state='',completed_at=now() WHERE id=$1`, id)
		}, want: ErrSubscriptionNotFound},
		{name: "canceled call", cancel: true, want: context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := createTVSubmissionIntegrationParent(t, store, owner, fmt.Sprintf("empty-guard-%d", index), subscription.PendingIdentification, false)
			observed := mustActiveSubscription(t, store, owner, id)
			if tc.change != nil {
				tc.change(t, store, id)
			}
			before := emptyTVProgressSnapshot(t, store, id)
			callOwner, callID := owner, id
			if tc.owner != "" {
				callOwner = tc.owner
			}
			if tc.missingID {
				callID += 1_000_000
			}
			ctx := t.Context()
			if tc.cancel {
				canceled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = canceled
			}
			err := store.RecordEmptyTVSubscriptionProgress(ctx, callOwner, callID, subscription.PendingIdentification, observed.UpdatedAt)
			if !errors.Is(err, tc.want) {
				t.Fatalf("progress guard error=%v want=%v", err, tc.want)
			}
			after := emptyTVProgressSnapshot(t, store, id)
			if !reflect.DeepEqual(after, before) {
				t.Fatalf("rejected progress changed subscription: before=%+v after=%+v", before, after)
			}
		})
	}
}

func mustEmptyTVProgressMutation(t *testing.T, store *Store, query string, args ...any) {
	t.Helper()
	if _, err := store.db.Pool.Exec(t.Context(), query, args...); err != nil {
		t.Fatal(err)
	}
}

func emptyTVProgressSnapshot(t *testing.T, store *Store, id int64) any {
	t.Helper()
	var snapshot struct {
		Config      string
		State       subscription.State
		V2State     string
		CompletedAt *time.Time
		UpdatedAt   time.Time
		Intent      *uuid.UUID
	}
	if err := store.db.Pool.QueryRow(t.Context(), `
		SELECT config::text,internal_state,v2_state,completed_at,updated_at,current_download_intent_id
		FROM subscriptions WHERE id=$1`, id).Scan(
		&snapshot.Config, &snapshot.State, &snapshot.V2State, &snapshot.CompletedAt, &snapshot.UpdatedAt, &snapshot.Intent,
	); err != nil {
		t.Fatal(err)
	}
	return snapshot
}

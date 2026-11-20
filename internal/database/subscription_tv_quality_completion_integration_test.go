//go:build integration

package database

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

func TestCompleteSatisfiedBestVersionTVSubscriptionPostgresQualityAndOwner(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	const owner = "tv-quality-completion"
	insertSubscriptionIntegrationUsers(t, store, owner, "tv-quality-completion-other")
	for index, tc := range []struct {
		name         string
		full         bool
		total, start int
		current      *int
		priorities   map[string]any
		note         []int
		state        subscription.State
		wantComplete bool
		wantCurrent  int
		wantLack     int
	}{
		{"逐集恰好100", false, 2, 1, r08MoviePriority(90), map[string]any{"1": 100, "2": 100}, nil, subscription.PendingSearch, true, 100, 0},
		{"整季恰好100不捏造实际覆盖", true, 2, 1, r08MoviePriority(100), nil, nil, subscription.Active, true, 100, 2},
		{"整季100保留部分实际覆盖缺集", true, 2, 1, r08MoviePriority(100), nil, []int{1}, subscription.Active, true, 100, 1},
		{"空目标沿用原版总质量", false, 2, 3, r08MoviePriority(100), nil, nil, subscription.Active, true, 100, 0},
		{"未知目标沿用原版总质量", false, 0, 1, r08MoviePriority(100), nil, nil, subscription.PendingSearch, true, 100, 0},
		{"逐集0不完成", false, 2, 1, r08MoviePriority(100), map[string]any{"1": 0, "2": 100}, nil, subscription.Active, false, 0, 0},
		{"逐集负质量不完成", false, 2, 1, r08MoviePriority(100), map[string]any{"1": -1, "2": 100}, nil, subscription.Active, false, 0, 0},
		{"逐集101不完成", false, 2, 1, r08MoviePriority(100), map[string]any{"1": 101, "2": 100}, nil, subscription.PendingSearch, false, 0, 0},
		{"逐集缺质量不完成", false, 2, 1, nil, map[string]any{"1": 100}, nil, subscription.PendingSearch, false, 0, 0},
		{"整季未知质量不完成", true, 2, 1, nil, nil, nil, subscription.Active, false, 0, 0},
		{"整季101不完成", true, 2, 1, r08MoviePriority(101), nil, nil, subscription.Active, false, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := createTVSubmissionIntegrationParent(t, store, owner, fmt.Sprintf("quality-completion-%d", index), tc.state, true)
			config := map[string]any{
				"best_version": 1, "best_version_full": tc.full,
				"total_episode": tc.total, "start_episode": tc.start, "lack_episode": 2,
				"episode_priority": tc.priorities, "note": tc.note, "fixture": tc.name,
			}
			if tc.current != nil {
				config["current_priority"] = *tc.current
			}
			setTVQualityIntegrationConfig(t, store, id, config)
			before := mustActiveSubscription(t, store, owner, id)
			if err := store.CompleteSatisfiedBestVersionTVSubscription(t.Context(), "tv-quality-completion-other", id, tc.state, before.UpdatedAt); !errors.Is(err, ErrSubscriptionNotFound) {
				t.Fatalf("其他 owner 完成 err=%v", err)
			}
			err := store.CompleteSatisfiedBestVersionTVSubscription(t.Context(), " "+owner+" ", id, tc.state, before.UpdatedAt)
			if !tc.wantComplete {
				if !errors.Is(err, ErrSubscriptionPreciseSearchStateConflict) {
					t.Fatalf("非终态质量完成 err=%v", err)
				}
				after := mustActiveSubscription(t, store, owner, id)
				if after.InternalState != before.InternalState || !after.UpdatedAt.Equal(before.UpdatedAt) {
					t.Fatalf("拒绝完成改写订阅: before=%+v after=%+v", before, after)
				}
				return
			}
			if err != nil {
				t.Fatalf("终态质量完成: %v", err)
			}
			completed, err := store.GetOwnedSubscription(t.Context(), owner, id)
			if err != nil || completed.InternalState != subscription.Completed || completed.CompletedAt == nil || completed.V2State != "" {
				t.Fatalf("owner 读取历史订阅=%+v err=%v", completed, err)
			}
			if completed.Config["lack_episode"] != float64(tc.wantLack) || completed.Config["current_priority"] != float64(tc.wantCurrent) || completed.Config["fixture"] != tc.name {
				t.Fatalf("完成进度或现有配置丢失=%+v", completed.Config)
			}
			if _, err := store.GetActiveSubscription(t.Context(), owner, id); !errors.Is(err, ErrSubscriptionNotFound) {
				t.Fatalf("历史订阅仍为 active err=%v", err)
			}
			if _, err := store.GetOwnedSubscription(t.Context(), "tv-quality-completion-other", id); !errors.Is(err, ErrSubscriptionNotFound) {
				t.Fatalf("历史订阅跨 owner 可见 err=%v", err)
			}
			if err := store.CompleteSatisfiedBestVersionTVSubscription(t.Context(), owner, id, tc.state, before.UpdatedAt); !errors.Is(err, ErrSubscriptionNotFound) {
				t.Fatalf("完成重放 err=%v", err)
			}
		})
	}
}

func TestCompleteSatisfiedBestVersionTVSubscriptionPostgresRejectsStaleFacts(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	const owner = "tv-quality-cas"
	insertSubscriptionIntegrationUsers(t, store, owner)
	for index, mode := range []string{"quality", "ordinary", "full-mode", "season", "timestamp", "state", "current-intent", "completed"} {
		t.Run(mode, func(t *testing.T) {
			id := createTVSubmissionIntegrationParent(t, store, owner, fmt.Sprintf("quality-cas-%d", index), subscription.PendingSearch, true)
			setTVQualityIntegrationConfig(t, store, id, map[string]any{
				"best_version": true, "best_version_full": false, "total_episode": 2, "start_episode": 1,
				"current_priority": 90, "episode_priority": map[string]int{"1": 100, "2": 100},
			})
			observed := mustActiveSubscription(t, store, owner, id)
			query, args, err := prepareBestVersionTVCompletion(observed, owner, id, observed.InternalState, observed.UpdatedAt)
			if err != nil {
				t.Fatal(err)
			}
			// Keep updated_at unchanged for config and season mutations. This
			// exercises the snapshot CAS even for legacy writers that omit time.
			switch mode {
			case "quality":
				_, err = store.db.Pool.Exec(t.Context(), `UPDATE subscriptions SET config=jsonb_set(config,'{episode_priority,1}','90') WHERE id=$1`, id)
			case "ordinary":
				_, err = store.db.Pool.Exec(t.Context(), `UPDATE subscriptions SET config=jsonb_set(config,'{best_version}','false') WHERE id=$1`, id)
			case "full-mode":
				_, err = store.db.Pool.Exec(t.Context(), `UPDATE subscriptions SET config=jsonb_set(config,'{best_version_full}','true') WHERE id=$1`, id)
			case "season":
				_, err = store.db.Pool.Exec(t.Context(), `UPDATE subscriptions SET season=2 WHERE id=$1`, id)
			case "timestamp":
				_, err = store.db.Pool.Exec(t.Context(), `UPDATE subscriptions SET updated_at=updated_at + interval '1 second' WHERE id=$1`, id)
			case "state":
				_, err = store.db.Pool.Exec(t.Context(), `UPDATE subscriptions SET internal_state='active',v2_state='R' WHERE id=$1`, id)
			case "current-intent":
				intent := createDownloadIntentAcceptanceIntent(t, store, owner, fmt.Sprintf("quality-cas-intent-%d", index))
				_, err = store.db.Pool.Exec(t.Context(), `UPDATE subscriptions SET current_download_intent_id=$2 WHERE id=$1`, id, intent.ID)
			case "completed":
				_, err = store.db.Pool.Exec(t.Context(), `UPDATE subscriptions SET internal_state='completed',v2_state='',completed_at=now() WHERE id=$1`, id)
			}
			if err != nil {
				t.Fatal(err)
			}
			var completedID int64
			if err := store.db.Pool.QueryRow(t.Context(), query, args...).Scan(&completedID); !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("旧质量快照未被 CAS 拒绝: id=%d err=%v", completedID, err)
			}
			if mode != "completed" {
				var completed bool
				if err := store.db.Pool.QueryRow(t.Context(), `SELECT completed_at IS NOT NULL FROM subscriptions WHERE id=$1`, id).Scan(&completed); err != nil || completed {
					t.Fatalf("并发事实变化后被错误完成=%v err=%v", completed, err)
				}
			}
		})
	}
}

func TestCompleteSatisfiedBestVersionTVSubscriptionPostgresModeAndPGuards(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	const owner = "tv-quality-mode-guard"
	insertSubscriptionIntegrationUsers(t, store, owner)
	for index, mode := range []string{"ordinary", "P", "movie"} {
		t.Run(mode, func(t *testing.T) {
			state := subscription.Active
			if mode == "P" {
				state = subscription.PendingIdentification
			}
			id := createTVSubmissionIntegrationParent(t, store, owner, fmt.Sprintf("quality-mode-%d", index), state, mode != "ordinary")
			setTVQualityIntegrationConfig(t, store, id, map[string]any{
				"best_version": mode != "ordinary", "best_version_full": true,
				"current_priority": 100, "total_episode": 2,
			})
			if mode == "movie" {
				if _, err := store.db.Pool.Exec(t.Context(), `UPDATE subscriptions SET media_type='电影' WHERE id=$1`, id); err != nil {
					t.Fatal(err)
				}
			}
			before := mustActiveSubscription(t, store, owner, id)
			if err := store.CompleteSatisfiedBestVersionTVSubscription(t.Context(), owner, id, subscription.Active, before.UpdatedAt); !errors.Is(err, ErrSubscriptionPreciseSearchStateConflict) {
				t.Fatalf("%s 越界完成 err=%v", mode, err)
			}
			after := mustActiveSubscription(t, store, owner, id)
			if after.CompletedAt != nil || after.InternalState != state {
				t.Fatalf("%s 越界完成后=%+v", mode, after)
			}
		})
	}
}

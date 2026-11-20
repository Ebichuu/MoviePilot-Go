//go:build integration

package database

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/moviepilot-go/moviepilot-go/internal/downloader"
	"github.com/moviepilot-go/moviepilot-go/internal/organization"
	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

func TestTVBestVersionSubmissionPostgresIndependentCoverageTransaction(t *testing.T) {
	store := newDownloadIntentPlanIntegrationStore(t)
	const owner = "tv-independent-owner"
	insertDownloadIntentIntegrationUsers(t, store, owner)
	site := insertDownloadIntentPlanIntegrationSite(t, store, "tv-independent-site")
	episodes := []subscription.Episode{{Season: 1, Episode: 1}, {Season: 1, Episode: 2}}
	parentID := createTVSubmissionIntegrationParent(t, store, owner, "independent", subscription.PendingSearch, true)
	if err := store.UpsertSubscriptionEpisodes(t.Context(), owner, parentID, episodes); err != nil {
		t.Fatal(err)
	}
	old := createDownloadIntentAcceptanceIntent(t, store, owner, "independent-old")
	if _, err := store.db.Pool.Exec(t.Context(), `
		UPDATE download_intents SET state='downloading', torrent_hash=$2, subscription_id=$3 WHERE id=$1`,
		old.ID, databaseIntentV1Hash, parentID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Pool.Exec(t.Context(), `
		UPDATE subscription_episodes SET state='indexed', download_intent_id=$2 WHERE subscription_id=$1`,
		parentID, old.ID); err != nil {
		t.Fatal(err)
	}

	rolledBack := mustDatabaseDownloadIntent(t, owner, "independent-rollback")
	tx := beginTVSubmissionIntegrationTx(t, store)
	createTVSubmissionIntegrationFactsTx(t, store, tx, owner, rolledBack, site, episodes, r08MoviePriority(0))
	if err := store.BindDownloadIntentSubscriptionTx(t.Context(), tx, owner, rolledBack.ID, parentID); err != nil {
		t.Fatal(err)
	}
	if best, err := store.VerifyTVBestVersionSubmissionTx(t.Context(), tx, owner, parentID, rolledBack.ID, episodes); err != nil || !best {
		t.Fatalf("独立计划验证 best=%v err=%v", best, err)
	}
	if err := store.CompareAndSwapSubscriptionPreciseSearchStateTx(t.Context(), tx, owner, parentID, subscription.Acquired); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetDownloadIntent(t.Context(), owner, rolledBack.ID); !errors.Is(err, ErrDownloadIntentNotFound) {
		t.Fatalf("回滚后意图 err=%v", err)
	}
	if parent := readDownloadIntentAcceptanceParent(t, store, parentID); parent.state != subscription.PendingSearch || parent.currentIntent != nil {
		t.Fatalf("回滚后订阅=%+v", parent)
	}
	assertTVSubmissionIntegrationOldRows(t, store, parentID, old.ID, len(episodes))

	intent := mustDatabaseDownloadIntent(t, owner, "independent-commit")
	tx = beginTVSubmissionIntegrationTx(t, store)
	createTVSubmissionIntegrationFactsTx(t, store, tx, owner, intent, site, episodes, r08MoviePriority(-10))
	if err := store.BindDownloadIntentSubscriptionTx(t.Context(), tx, owner, intent.ID, parentID); err != nil {
		t.Fatal(err)
	}
	if best, err := store.VerifyTVBestVersionSubmissionTx(t.Context(), tx, owner, parentID, intent.ID, episodes); err != nil || !best {
		t.Fatalf("负质量仍为已知质量 best=%v err=%v", best, err)
	}
	if err := store.CompareAndSwapSubscriptionPreciseSearchStateTx(t.Context(), tx, owner, parentID, subscription.Acquired); err != nil {
		t.Fatal(err)
	}
	if err := store.CompareAndSwapSubscriptionPreciseSearchStateTx(t.Context(), tx, owner, parentID, subscription.Acquired); !errors.Is(err, ErrSubscriptionPreciseSearchStateConflict) {
		t.Fatalf("已 claim CAS err=%v", err)
	}
	if err := store.VerifyTVBestVersionSubmissionReplayTx(t.Context(), tx, owner, parentID, intent.ID, episodes, subscription.PendingSearch); err != nil {
		t.Fatalf("同意图 claim 后重放失败: %v", err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	assertDownloadIntentSubscriptionID(t, store, intent.ID, parentID)
	assertTVSubmissionIntegrationOldRows(t, store, parentID, old.ID, len(episodes))
	config := readTVQualityConfig(t, store, parentID)
	if _, exists := config["episode_priority"]; exists {
		t.Fatalf("提交阶段提前写入质量=%+v", config)
	}
}

func TestTVBestVersionSubmissionPostgresOwnerCurrentAndImmutableCoverage(t *testing.T) {
	store := newDownloadIntentPlanIntegrationStore(t)
	const owner = "tv-submit-guards"
	insertDownloadIntentIntegrationUsers(t, store, owner, "tv-submit-other")
	site := insertDownloadIntentPlanIntegrationSite(t, store, "tv-submit-guard-site")
	episodes := []subscription.Episode{{Season: 1, Episode: 1}}
	for index, tc := range []struct {
		name string
		mode string
	}{
		{"其他 owner", "owner"},
		{"当前意图已改变", "current"},
		{"父订阅已完成", "completed"},
		{"成功输入覆盖与计划不符", "success"},
		{"请求覆盖与计划不符", "requested"},
		{"计划质量未知", "quality"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			key := fmt.Sprintf("tv-submit-guard-%d", index)
			parentID := createTVSubmissionIntegrationParent(t, store, owner, key, subscription.PendingSearch, true)
			intent := mustDatabaseDownloadIntent(t, owner, key)
			tx := beginTVSubmissionIntegrationTx(t, store)
			priority := r08MoviePriority(100)
			if tc.mode == "quality" {
				priority = nil
			}
			if tc.mode == "success" {
				createTVSubmissionIntegrationFactsTx(t, store, tx, owner, intent, site, episodes, priority, nil)
			} else {
				createTVSubmissionIntegrationFactsTx(t, store, tx, owner, intent, site, episodes, priority)
			}
			if err := store.BindDownloadIntentSubscriptionTx(t.Context(), tx, owner, intent.ID, parentID); err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(t.Context()); err != nil {
				t.Fatal(err)
			}
			requested, username := episodes, owner
			switch tc.mode {
			case "owner":
				username = "tv-submit-other"
			case "current":
				other := createDownloadIntentAcceptanceIntent(t, store, owner, key+"-other")
				if _, err := store.db.Pool.Exec(t.Context(), `UPDATE subscriptions SET current_download_intent_id=$2 WHERE id=$1`, parentID, other.ID); err != nil {
					t.Fatal(err)
				}
			case "completed":
				if _, err := store.db.Pool.Exec(t.Context(), `UPDATE subscriptions SET internal_state='completed',v2_state='',completed_at=now() WHERE id=$1`, parentID); err != nil {
					t.Fatal(err)
				}
			case "requested":
				requested = []subscription.Episode{{Season: 1, Episode: 2}}
			}
			tx = beginTVSubmissionIntegrationTx(t, store)
			if best, err := store.VerifyTVBestVersionSubmissionTx(t.Context(), tx, username, parentID, intent.ID, requested); !errors.Is(err, ErrSubscriptionEpisodeBindingConflict) || best {
				t.Fatalf("越界请求 best=%v err=%v", best, err)
			}
		})
	}
}

func TestTVBestVersionSubmissionPostgresPReplayAndOrdinaryBinding(t *testing.T) {
	store := newDownloadIntentPlanIntegrationStore(t)
	const owner = "tv-submit-p"
	insertDownloadIntentIntegrationUsers(t, store, owner)
	site := insertDownloadIntentPlanIntegrationSite(t, store, "tv-submit-p-site")
	parentID := createTVSubmissionIntegrationParent(t, store, owner, "p", subscription.PendingIdentification, true)
	intent := mustDatabaseDownloadIntent(t, owner, "tv-submit-empty-p")
	tx := beginTVSubmissionIntegrationTx(t, store)
	createTVSubmissionIntegrationFactsTx(t, store, tx, owner, intent, site, nil, r08MoviePriority(0))
	if err := store.BindDownloadIntentSubscriptionTx(t.Context(), tx, owner, intent.ID, parentID); err != nil {
		t.Fatal(err)
	}
	if best, err := store.VerifyTVBestVersionSubmissionTx(t.Context(), tx, owner, parentID, intent.ID, nil); err != nil || !best {
		t.Fatalf("已知空覆盖验证 best=%v err=%v", best, err)
	}
	if err := store.CompareAndSwapSubscriptionRSSStateTx(t.Context(), tx, owner, parentID, subscription.PendingIdentification); err != nil {
		t.Fatal(err)
	}
	if err := store.VerifyTVBestVersionSubmissionReplayTx(t.Context(), tx, owner, parentID, intent.ID, nil, subscription.PendingIdentification); err != nil {
		t.Fatalf("RSS P 重放: %v", err)
	}
	for _, expected := range []subscription.State{subscription.PendingSearch, subscription.Active} {
		if err := store.VerifyTVBestVersionSubmissionReplayTx(t.Context(), tx, owner, parentID, intent.ID, nil, expected); !errors.Is(err, ErrSubscriptionEpisodeBindingConflict) {
			t.Fatalf("P 不能按 %s 重放: %v", expected, err)
		}
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if parent := readDownloadIntentAcceptanceParent(t, store, parentID); parent.state != subscription.PendingIdentification || parent.completed {
		t.Fatalf("P claim 后订阅=%+v", parent)
	}

	episodes := []subscription.Episode{{Season: 1, Episode: 1}}
	ordinaryID := createTVSubmissionIntegrationParent(t, store, owner, "ordinary", subscription.PendingSearch, false)
	if err := store.UpsertSubscriptionEpisodes(t.Context(), owner, ordinaryID, episodes); err != nil {
		t.Fatal(err)
	}
	ordinary := mustDatabaseDownloadIntent(t, owner, "tv-submit-ordinary")
	tx = beginTVSubmissionIntegrationTx(t, store)
	createTVSubmissionIntegrationFactsTx(t, store, tx, owner, ordinary, site, episodes, r08MoviePriority(100))
	if err := store.BindDownloadIntentSubscriptionTx(t.Context(), tx, owner, ordinary.ID, ordinaryID); err != nil {
		t.Fatal(err)
	}
	if best, err := store.VerifyTVBestVersionSubmissionTx(t.Context(), tx, owner, ordinaryID, ordinary.ID, episodes); err != nil || best {
		t.Fatalf("普通 TV 必须继续逐集绑定 best=%v err=%v", best, err)
	}
	if err := store.CompareAndSwapSubscriptionPreciseSearchStateTx(t.Context(), tx, owner, ordinaryID, subscription.Acquired); err != nil {
		t.Fatal(err)
	}
	if err := store.BindSubscriptionEpisodesTx(t.Context(), tx, owner, ordinaryID, ordinary.ID, episodes); err != nil {
		t.Fatal(err)
	}
	if err := store.VerifySubscriptionEpisodesBoundToIntentTx(t.Context(), tx, owner, ordinaryID, ordinary.ID, episodes); err != nil {
		t.Fatalf("普通 TV 重放验证: %v", err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	var state subscription.SubscriptionEpisodeState
	var bound uuid.UUID
	if err := store.db.Pool.QueryRow(t.Context(), `SELECT state,download_intent_id FROM subscription_episodes WHERE subscription_id=$1`, ordinaryID).Scan(&state, &bound); err != nil {
		t.Fatal(err)
	}
	if state != subscription.SubscriptionEpisodeAcquired || bound != ordinary.ID {
		t.Fatalf("普通逐集绑定 state=%s intent=%s", state, bound)
	}
}

func createTVSubmissionIntegrationParent(t *testing.T, store *Store, owner, key string, state subscription.State, best bool) int64 {
	t.Helper()
	season := 1
	initialState := state
	if initialState != subscription.PendingIdentification && initialState != subscription.PendingSearch && initialState != subscription.Paused {
		initialState = subscription.PendingSearch
	}
	id, err := store.CreateSubscription(t.Context(), NewSubscription{
		Username: owner, Name: key, MediaType: "电视剧", Year: "2026", Season: &season,
		MediaSource: "tmdb", MediaID: key, State: initialState,
		Config: map[string]any{"best_version": best, "total_episode": 2, "start_episode": 1, "lack_episode": 2, "fixture": key},
	})
	if err != nil {
		t.Fatal(err)
	}
	if initialState != state {
		projected, err := subscription.V2State(state)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.Pool.Exec(t.Context(), `UPDATE subscriptions SET internal_state=$2,v2_state=$3 WHERE id=$1`, id, state, projected); err != nil {
			t.Fatal(err)
		}
	}
	return id
}

func createTVSubmissionIntegrationFactsTx(t *testing.T, store *Store, tx pgx.Tx, owner string, intent downloader.Intent, site int64, episodes []subscription.Episode, priority *int, successOverride ...[]subscription.Episode) {
	t.Helper()
	if _, err := store.CreateDownloadIntentTx(t.Context(), tx, owner, intent); err != nil {
		t.Fatal(err)
	}
	plan := downloadIntentPlanIntegrationFixture(intent.ID, site, "tv-submit-"+intent.ID.String())
	plan.TVPriority = priority
	plan.TVCoverage = append([]subscription.Episode{}, episodes...)
	if _, err := store.CreateDownloadIntentPlanTx(t.Context(), tx, owner, plan); err != nil {
		t.Fatal(err)
	}
	successCoverage := episodes
	if len(successOverride) > 0 {
		successCoverage = successOverride[0]
	}
	successEpisodes := make([]organization.Episode, 0, len(successCoverage))
	for _, episode := range successCoverage {
		successEpisodes = append(successEpisodes, organization.Episode{Season: episode.Season, Episode: episode.Episode})
	}
	input := organization.SuccessInput{
		IntentID: intent.ID, Version: organization.CurrentSuccessVersion, Username: owner,
		Media:    organization.MediaContext{Source: "tmdb", MediaID: "42", TitleYear: "TV (2026)", MediaType: "电视剧"},
		Episodes: successEpisodes,
	}
	if _, err := store.CreateDownloadIntentSuccessInputTx(t.Context(), tx, owner, input); err != nil {
		t.Fatal(err)
	}
}

func beginTVSubmissionIntegrationTx(t *testing.T, store *Store) pgx.Tx {
	t.Helper()
	tx, err := store.db.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	return tx
}

func assertTVSubmissionIntegrationOldRows(t *testing.T, store *Store, parentID int64, old uuid.UUID, count int) {
	t.Helper()
	var matched int
	if err := store.db.Pool.QueryRow(t.Context(), `
		SELECT count(*) FROM subscription_episodes WHERE subscription_id=$1 AND state='indexed' AND download_intent_id=$2`,
		parentID, old).Scan(&matched); err != nil {
		t.Fatal(err)
	}
	if matched != count {
		t.Fatalf("旧逐集事实被覆盖: 保留=%d want=%d", matched, count)
	}
}

func setTVQualityIntegrationConfig(t *testing.T, store *Store, id int64, config map[string]any) {
	t.Helper()
	encoded, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Pool.Exec(t.Context(), `UPDATE subscriptions SET config=$2::jsonb WHERE id=$1`, id, encoded); err != nil {
		t.Fatal(err)
	}
}

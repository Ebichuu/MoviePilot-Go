//go:build integration

package database

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

func TestSubscriptionEpisodesPostgresUpsertListAndSummary(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	insertSubscriptionIntegrationUsers(t, store, "episode-owner", "episode-other")
	subscriptionID := createSubscriptionIntegrationFixture(t, store, "episode-owner", "episode-upsert")

	episodes := []subscription.Episode{
		{Season: 1, Episode: 3},
		{Season: 1, Episode: 1},
		{Season: 1, Episode: 2},
		{Season: 1, Episode: 2},
	}
	if err := store.UpsertSubscriptionEpisodes(t.Context(), " episode-owner ", subscriptionID, episodes); err != nil {
		t.Fatalf("初始化逐集状态: %v", err)
	}

	limited, err := store.ListSubscriptionEpisodes(
		t.Context(), "episode-owner", subscriptionID,
		[]SubscriptionEpisodeState{SubscriptionEpisodeMissing}, 2,
	)
	if err != nil {
		t.Fatalf("有界读取缺失集: %v", err)
	}
	assertSubscriptionEpisodeCoordinates(t, limited, []subscription.Episode{
		{Season: 1, Episode: 1},
		{Season: 1, Episode: 2},
	})

	first := subscription.Episode{Season: 1, Episode: 1}
	if err := store.CompareAndSwapSubscriptionEpisodeState(
		t.Context(), "episode-owner", subscriptionID, first,
		SubscriptionEpisodeMissing, SubscriptionEpisodeAcquired,
	); err != nil {
		t.Fatalf("推进首集到 acquired: %v", err)
	}
	if err := store.UpsertSubscriptionEpisodes(t.Context(), "episode-owner", subscriptionID, episodes); err != nil {
		t.Fatalf("幂等重放逐集初始化: %v", err)
	}

	all, err := store.ListSubscriptionEpisodes(
		t.Context(), "episode-owner", subscriptionID,
		[]SubscriptionEpisodeState{SubscriptionEpisodeMissing, SubscriptionEpisodeAcquired},
		100,
	)
	if err != nil {
		t.Fatalf("读取所有初始化集: %v", err)
	}
	assertSubscriptionEpisodeCoordinates(t, all, []subscription.Episode{
		{Season: 1, Episode: 1},
		{Season: 1, Episode: 2},
		{Season: 1, Episode: 3},
	})
	if all[0].State != SubscriptionEpisodeAcquired || all[1].State != SubscriptionEpisodeMissing || all[2].State != SubscriptionEpisodeMissing {
		t.Fatalf("幂等初始化回退了已有进度: %#v", all)
	}

	summary, err := store.SummarizeSubscriptionEpisodes(t.Context(), "episode-owner", subscriptionID)
	if err != nil {
		t.Fatalf("汇总逐集状态: %v", err)
	}
	wantSummary := SubscriptionEpisodeSummary{Total: 3, Missing: 2, Acquired: 1}
	if summary != wantSummary {
		t.Fatalf("逐集汇总 = %#v, want %#v", summary, wantSummary)
	}

	otherVisible, err := store.ListSubscriptionEpisodes(
		t.Context(), "episode-other", subscriptionID,
		[]SubscriptionEpisodeState{SubscriptionEpisodeMissing, SubscriptionEpisodeAcquired}, 100,
	)
	if err != nil {
		t.Fatalf("其他 owner 有界读取: %v", err)
	}
	if len(otherVisible) != 0 {
		t.Fatalf("其他 owner 看到逐集状态: %#v", otherVisible)
	}
	if _, err := store.SummarizeSubscriptionEpisodes(t.Context(), "episode-other", subscriptionID); !errors.Is(err, ErrSubscriptionEpisodeNotFound) {
		t.Fatalf("其他 owner 汇总 error = %v, want ErrSubscriptionEpisodeNotFound", err)
	}
	if err := store.UpsertSubscriptionEpisodes(t.Context(), "episode-other", subscriptionID, []subscription.Episode{{Season: 1, Episode: 4}}); !errors.Is(err, ErrSubscriptionEpisodeNotFound) {
		t.Fatalf("其他 owner upsert error = %v, want ErrSubscriptionEpisodeNotFound", err)
	}
}

func TestSubscriptionEpisodesPostgresCASAndTransaction(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	insertSubscriptionIntegrationUsers(t, store, "episode-cas-owner", "episode-cas-other")
	subscriptionID := createSubscriptionIntegrationFixture(t, store, "episode-cas-owner", "episode-cas")
	episode := subscription.Episode{Season: 2, Episode: 5}
	if err := store.UpsertSubscriptionEpisodes(t.Context(), "episode-cas-owner", subscriptionID, []subscription.Episode{episode}); err != nil {
		t.Fatalf("初始化 CAS 集: %v", err)
	}

	if err := store.CompareAndSwapSubscriptionEpisodeState(
		t.Context(), "episode-cas-other", subscriptionID, episode,
		SubscriptionEpisodeMissing, SubscriptionEpisodeAcquired,
	); !errors.Is(err, ErrSubscriptionEpisodeNotFound) {
		t.Fatalf("其他 owner CAS error = %v, want ErrSubscriptionEpisodeNotFound", err)
	}
	if err := store.CompareAndSwapSubscriptionEpisodeState(
		t.Context(), "episode-cas-owner", subscriptionID, episode,
		SubscriptionEpisodeMissing, SubscriptionEpisodeAcquired,
	); err != nil {
		t.Fatalf("missing -> acquired: %v", err)
	}
	if err := store.CompareAndSwapSubscriptionEpisodeState(
		t.Context(), "episode-cas-owner", subscriptionID, episode,
		SubscriptionEpisodeMissing, SubscriptionEpisodeAcquired,
	); !errors.Is(err, ErrSubscriptionEpisodeStateConflict) {
		t.Fatalf("过期逐集 CAS error = %v, want ErrSubscriptionEpisodeStateConflict", err)
	}

	rollbackTx, err := store.db.Pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("开始逐集 CAS 回滚事务: %v", err)
	}
	defer func() { _ = rollbackTx.Rollback(context.Background()) }()
	if err := store.CompareAndSwapSubscriptionEpisodeStateTx(
		t.Context(), rollbackTx, "episode-cas-owner", subscriptionID, episode,
		SubscriptionEpisodeAcquired, SubscriptionEpisodeDownloading,
	); err != nil {
		t.Fatalf("事务内 acquired -> downloading: %v", err)
	}
	if err := rollbackTx.Rollback(t.Context()); err != nil {
		t.Fatalf("回滚逐集 CAS: %v", err)
	}
	assertSubscriptionEpisodeState(t, store, "episode-cas-owner", subscriptionID, episode, SubscriptionEpisodeAcquired)

	commitTx, err := store.db.Pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("开始逐集 CAS 提交事务: %v", err)
	}
	defer func() { _ = commitTx.Rollback(context.Background()) }()
	if err := store.CompareAndSwapSubscriptionEpisodeStateTx(
		t.Context(), commitTx, "episode-cas-owner", subscriptionID, episode,
		SubscriptionEpisodeAcquired, SubscriptionEpisodeDownloading,
	); err != nil {
		t.Fatalf("提交事务内 acquired -> downloading: %v", err)
	}
	if err := commitTx.Commit(t.Context()); err != nil {
		t.Fatalf("提交逐集 CAS: %v", err)
	}
	assertSubscriptionEpisodeState(t, store, "episode-cas-owner", subscriptionID, episode, SubscriptionEpisodeDownloading)
}

func TestSubscriptionEpisodesPostgresCASRejectsHistoricalIntent(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	const username = "episode-cas-current-intent-owner"
	insertSubscriptionIntegrationUsers(t, store, username)
	subscriptionID := createSubscriptionIntegrationFixture(
		t, store, username, "episode-cas-current-intent",
	)
	episode := subscription.Episode{Season: 9, Episode: 1}
	if err := store.UpsertSubscriptionEpisodes(t.Context(), username, subscriptionID, []subscription.Episode{episode}); err != nil {
		t.Fatalf("initialize current intent CAS episode: %v", err)
	}
	oldIntentID := createBoundSubscriptionEpisodeIntent(
		t, store, username, subscriptionID, "episode-cas-current-intent-old",
	)
	if err := store.BindSubscriptionEpisodes(
		t.Context(), username, subscriptionID, oldIntentID, []subscription.Episode{episode},
	); err != nil {
		t.Fatalf("bind historical CAS episode: %v", err)
	}
	newIntentID := createHistoricalSubscriptionEpisodeIntent(
		t, store, username, subscriptionID, "episode-cas-current-intent-new",
	)
	setSubscriptionCurrentIntent(t, store, username, subscriptionID, newIntentID)

	if err := store.CompareAndSwapSubscriptionEpisodeState(
		t.Context(), username, subscriptionID, episode,
		SubscriptionEpisodeAcquired, SubscriptionEpisodeDownloading,
	); !errors.Is(err, ErrSubscriptionEpisodeNotFound) {
		t.Fatalf("historical intent CAS error = %v, want ErrSubscriptionEpisodeNotFound", err)
	}
	assertSubscriptionEpisodeState(
		t, store, username, subscriptionID, episode, SubscriptionEpisodeAcquired,
	)
}

func TestSubscriptionEpisodesPostgresBindingAndIntentTransition(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	insertSubscriptionIntegrationUsers(t, store, "episode-bind-owner", "episode-bind-other")
	subscriptionID := createSubscriptionIntegrationFixture(t, store, "episode-bind-owner", "episode-bind")
	episodes := []subscription.Episode{
		{Season: 1, Episode: 1},
		{Season: 1, Episode: 2},
		{Season: 1, Episode: 3},
	}
	if err := store.UpsertSubscriptionEpisodes(t.Context(), "episode-bind-owner", subscriptionID, episodes); err != nil {
		t.Fatalf("初始化绑定集: %v", err)
	}
	primaryIntentID := createBoundSubscriptionEpisodeIntent(t, store, "episode-bind-owner", subscriptionID, "episode-bind-primary")

	rollbackTx, err := store.db.Pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("开始逐集绑定回滚事务: %v", err)
	}
	defer func() { _ = rollbackTx.Rollback(context.Background()) }()
	if err := store.BindSubscriptionEpisodesTx(
		t.Context(), rollbackTx, "episode-bind-owner", subscriptionID, primaryIntentID, episodes[:2],
	); err != nil {
		t.Fatalf("事务内绑定逐集: %v", err)
	}
	if err := rollbackTx.Rollback(t.Context()); err != nil {
		t.Fatalf("回滚逐集绑定: %v", err)
	}
	assertSubscriptionEpisodeIntentIDs(t, store, "episode-bind-owner", subscriptionID, map[int]uuid.UUID{})

	commitTx, err := store.db.Pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("开始逐集绑定提交事务: %v", err)
	}
	defer func() { _ = commitTx.Rollback(context.Background()) }()
	if err := store.BindSubscriptionEpisodesTx(
		t.Context(), commitTx, "episode-bind-owner", subscriptionID, primaryIntentID, episodes[:2],
	); err != nil {
		t.Fatalf("提交事务内绑定逐集: %v", err)
	}
	if err := commitTx.Commit(t.Context()); err != nil {
		t.Fatalf("提交逐集绑定: %v", err)
	}

	for _, episode := range episodes[:2] {
		assertSubscriptionEpisodeState(t, store, "episode-bind-owner", subscriptionID, episode, SubscriptionEpisodeAcquired)
	}
	if err := store.BindSubscriptionEpisodes(
		t.Context(), "episode-bind-owner", subscriptionID, primaryIntentID, episodes[:2],
	); err != nil {
		t.Fatalf("幂等重放相同逐集绑定: %v", err)
	}
	if err := store.CompareAndSwapSubscriptionPreciseSearchState(
		t.Context(), "episode-bind-owner", subscriptionID, subscription.Acquired,
	); err != nil {
		t.Fatalf("准备逐集提交重放父状态: %v", err)
	}
	replayTx, err := store.db.Pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("开始逐集重放验证事务: %v", err)
	}
	defer func() { _ = replayTx.Rollback(context.Background()) }()
	if err := store.VerifySubscriptionEpisodesBoundToIntentTx(
		t.Context(), replayTx, "episode-bind-owner", subscriptionID, primaryIntentID, episodes[:2],
	); err != nil {
		t.Fatalf("验证相同逐集提交重放: %v", err)
	}
	if err := store.VerifySubscriptionEpisodesBoundToIntentTx(
		t.Context(), replayTx, "episode-bind-other", subscriptionID, primaryIntentID, episodes[:2],
	); !errors.Is(err, ErrSubscriptionEpisodeNotFound) {
		t.Fatalf("其他 owner 重放验证 error = %v, want ErrSubscriptionEpisodeNotFound", err)
	}
	if err := replayTx.Rollback(t.Context()); err != nil {
		t.Fatalf("回滚逐集重放验证事务: %v", err)
	}

	updated, err := store.TransitionSubscriptionEpisodesByIntent(
		t.Context(), "episode-bind-owner", primaryIntentID,
		[]SubscriptionEpisodeState{SubscriptionEpisodeAcquired},
		SubscriptionEpisodeDownloading,
	)
	if err != nil {
		t.Fatalf("按 intent 推进到 downloading: %v", err)
	}
	if updated != 2 {
		t.Fatalf("按 intent 更新行数 = %d, want 2", updated)
	}
	for _, episode := range episodes[:2] {
		assertSubscriptionEpisodeState(t, store, "episode-bind-owner", subscriptionID, episode, SubscriptionEpisodeDownloading)
	}
	progressedReplayTx, err := store.db.Pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("开始已推进逐集重放验证事务: %v", err)
	}
	defer func() { _ = progressedReplayTx.Rollback(context.Background()) }()
	if err := store.VerifySubscriptionEpisodesBoundToIntentTx(
		t.Context(), progressedReplayTx, "episode-bind-owner", subscriptionID, primaryIntentID, episodes[:2],
	); err != nil {
		t.Fatalf("验证 downloading 逐集重放: %v", err)
	}
	if err := progressedReplayTx.Rollback(t.Context()); err != nil {
		t.Fatalf("回滚已推进逐集重放验证事务: %v", err)
	}

	if _, err := store.TransitionSubscriptionEpisodesByIntent(
		t.Context(), "episode-bind-other", primaryIntentID,
		[]SubscriptionEpisodeState{SubscriptionEpisodeDownloading},
		SubscriptionEpisodePendingOrganization,
	); !errors.Is(err, ErrSubscriptionEpisodeNotFound) {
		t.Fatalf("其他 owner intent 推进 error = %v, want ErrSubscriptionEpisodeNotFound", err)
	}

	conflictIntentID := createHistoricalSubscriptionEpisodeIntent(
		t, store, "episode-bind-owner", subscriptionID, "episode-bind-conflict",
	)
	setSubscriptionCurrentIntent(
		t, store, "episode-bind-owner", subscriptionID, conflictIntentID,
	)
	if err := store.BindSubscriptionEpisodes(
		t.Context(), "episode-bind-owner", subscriptionID, conflictIntentID,
		[]subscription.Episode{episodes[0], episodes[2]},
	); !errors.Is(err, ErrSubscriptionEpisodeBindingConflict) {
		t.Fatalf("部分冲突绑定 error = %v, want ErrSubscriptionEpisodeBindingConflict", err)
	}
	assertSubscriptionEpisodeIntentIDs(t, store, "episode-bind-owner", subscriptionID, map[int]uuid.UUID{
		1: primaryIntentID,
		2: primaryIntentID,
	})
	setSubscriptionCurrentIntent(
		t, store, "episode-bind-owner", subscriptionID, primaryIntentID,
	)

	updated, err = store.TransitionSubscriptionEpisodesByIntent(
		t.Context(), "episode-bind-owner", primaryIntentID,
		[]SubscriptionEpisodeState{SubscriptionEpisodeDownloading},
		SubscriptionEpisodeMissing,
	)
	if err != nil || updated != 2 {
		t.Fatalf("按 intent 回退缺失 = updated %d err %v", updated, err)
	}
	assertSubscriptionEpisodeIntentIDs(t, store, "episode-bind-owner", subscriptionID, map[int]uuid.UUID{})
}

func TestSubscriptionEpisodesPostgresErrorsAreSanitized(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	if _, err := store.db.Pool.Exec(t.Context(), `DROP TABLE subscription_episodes`); err != nil {
		t.Fatalf("准备逐集数据库错误测试: %v", err)
	}

	const sensitiveOwner = "episode-sensitive-owner"
	_, err := store.ListSubscriptionEpisodes(
		t.Context(), sensitiveOwner, 989898,
		[]SubscriptionEpisodeState{SubscriptionEpisodeMissing}, 1,
	)
	if !errors.Is(err, ErrSubscriptionEpisodePersistence) {
		t.Fatalf("逐集数据库错误 = %v, want ErrSubscriptionEpisodePersistence", err)
	}
	for _, sensitive := range []string{sensitiveOwner, "989898", "subscription_episodes", "SELECT"} {
		if strings.Contains(err.Error(), sensitive) {
			t.Fatalf("逐集数据库错误泄露 %q", sensitive)
		}
	}
}

func createBoundSubscriptionEpisodeIntent(
	t *testing.T,
	store *Store,
	username string,
	subscriptionID int64,
	candidateKey string,
) uuid.UUID {
	t.Helper()
	intent := mustDatabaseDownloadIntent(t, username, candidateKey)
	stored, err := store.CreateDownloadIntent(t.Context(), username, intent)
	if err != nil {
		t.Fatalf("创建逐集下载意图: %v", err)
	}
	tx, err := store.db.Pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("开始下载意图订阅绑定事务: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err := store.BindDownloadIntentSubscriptionTx(t.Context(), tx, username, stored.ID, subscriptionID); err != nil {
		t.Fatalf("绑定下载意图到订阅: %v", err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatalf("提交下载意图订阅绑定: %v", err)
	}
	return stored.ID
}

func createHistoricalSubscriptionEpisodeIntent(
	t *testing.T,
	store *Store,
	username string,
	subscriptionID int64,
	candidateKey string,
) uuid.UUID {
	t.Helper()
	intent := mustDatabaseDownloadIntent(t, username, candidateKey)
	stored, err := store.CreateDownloadIntent(t.Context(), username, intent)
	if err != nil {
		t.Fatalf("创建历史逐集下载意图: %v", err)
	}
	result, err := store.db.Pool.Exec(t.Context(), `
		UPDATE download_intents
		SET subscription_id=$1, updated_at=now()
		WHERE id=$2 AND username=$3`, subscriptionID, stored.ID, username)
	if err != nil {
		t.Fatalf("关联历史下载意图到订阅: %v", err)
	}
	if result.RowsAffected() != 1 {
		t.Fatalf("关联历史下载意图行数 = %d, want 1", result.RowsAffected())
	}
	return stored.ID
}

func assertSubscriptionEpisodeCoordinates(
	t *testing.T,
	items []SubscriptionEpisode,
	want []subscription.Episode,
) {
	t.Helper()
	if len(items) != len(want) {
		t.Fatalf("逐集记录数 = %d, want %d; items=%#v", len(items), len(want), items)
	}
	for index, episode := range want {
		if items[index].Season != episode.Season || items[index].Episode != episode.Episode {
			t.Fatalf("逐集坐标[%d] = S%dE%d, want S%dE%d", index, items[index].Season, items[index].Episode, episode.Season, episode.Episode)
		}
	}
}

func assertSubscriptionEpisodeState(
	t *testing.T,
	store *Store,
	username string,
	subscriptionID int64,
	episode subscription.Episode,
	want SubscriptionEpisodeState,
) {
	t.Helper()
	items, err := store.ListSubscriptionEpisodes(t.Context(), username, subscriptionID, []SubscriptionEpisodeState{want}, maxSubscriptionEpisodes)
	if err != nil {
		t.Fatalf("读取逐集状态: %v", err)
	}
	for _, item := range items {
		if item.Season == episode.Season && item.Episode == episode.Episode {
			return
		}
	}
	t.Fatalf("逐集 S%dE%d 未处于状态 %q; items=%#v", episode.Season, episode.Episode, want, items)
}

func assertSubscriptionEpisodeIntentIDs(
	t *testing.T,
	store *Store,
	username string,
	subscriptionID int64,
	want map[int]uuid.UUID,
) {
	t.Helper()
	items, err := store.ListSubscriptionEpisodes(
		t.Context(), username, subscriptionID,
		[]SubscriptionEpisodeState{
			SubscriptionEpisodeMissing,
			SubscriptionEpisodeAcquired,
			SubscriptionEpisodeDownloading,
			SubscriptionEpisodePendingOrganization,
			SubscriptionEpisodeOrganizing,
			SubscriptionEpisodePendingIndex,
			SubscriptionEpisodeIndexed,
		},
		maxSubscriptionEpisodes,
	)
	if err != nil {
		t.Fatalf("读取逐集意图绑定: %v", err)
	}
	for _, item := range items {
		wantID, shouldBind := want[item.Episode]
		if !shouldBind {
			if item.DownloadIntentID != nil {
				t.Fatalf("S%dE%d 意外绑定 intent %s", item.Season, item.Episode, *item.DownloadIntentID)
			}
			continue
		}
		if item.DownloadIntentID == nil || *item.DownloadIntentID != wantID {
			t.Fatalf("S%dE%d intent = %v, want %s", item.Season, item.Episode, item.DownloadIntentID, wantID)
		}
	}
}

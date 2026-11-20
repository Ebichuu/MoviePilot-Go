//go:build integration

package database

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/moviepilot-go/moviepilot-go/internal/downloader"
	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

func TestAcceptDownloadIntentPostgresConsumesSubscription(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	const username = "acceptance-consume-owner"
	insertDownloadIntentIntegrationUsers(t, store, username)

	tests := []struct {
		name          string
		mediaType     string
		allEpisodes   []subscription.Episode
		boundEpisodes []subscription.Episode
		wantParent    subscription.State
		wantCompleted bool
		wantNote      []int
		wantLack      int
	}{
		{
			name:          "电影接受后完成",
			mediaType:     "电影",
			wantParent:    subscription.Completed,
			wantCompleted: true,
			wantNote:      []int{1},
		},
		{
			name:      "TV 无缺集后完成",
			mediaType: "电视剧",
			allEpisodes: []subscription.Episode{
				{Season: 1, Episode: 1},
				{Season: 1, Episode: 2},
			},
			boundEpisodes: []subscription.Episode{
				{Season: 1, Episode: 1},
				{Season: 1, Episode: 2},
			},
			wantParent:    subscription.Completed,
			wantCompleted: true,
			wantNote:      []int{1, 2},
		},
		{
			name:      "TV 仍缺集时继续搜索",
			mediaType: "电视剧",
			allEpisodes: []subscription.Episode{
				{Season: 2, Episode: 1},
				{Season: 2, Episode: 2},
				{Season: 2, Episode: 3},
			},
			boundEpisodes: []subscription.Episode{
				{Season: 2, Episode: 1},
				{Season: 2, Episode: 2},
			},
			wantParent: subscription.PendingSearch,
			wantNote:   []int{1, 2},
			wantLack:   1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := prepareDownloadIntentAcceptanceFixture(
				t,
				store,
				username,
				uuid.NewString(),
				tt.mediaType,
				tt.allEpisodes,
				tt.boundEpisodes,
				"",
			)
			accepted, released, err := store.AcceptDownloadIntent(
				t.Context(),
				"  "+username+"\t",
				fixture.intent.ID,
				stringsLower(databaseIntentV1Hash),
			)
			if err != nil {
				t.Fatalf("AcceptDownloadIntent() error = %v", err)
			}
			if accepted.State != downloader.IntentDownloading || accepted.TorrentHash != databaseIntentV1Hash {
				t.Fatalf("AcceptDownloadIntent() = %#v", accepted)
			}
			if !released {
				t.Fatal("AcceptDownloadIntent() did not release subscription")
			}

			parent := readDownloadIntentAcceptanceParent(t, store, fixture.subscriptionID)
			if parent.state != tt.wantParent || parent.completed != tt.wantCompleted || parent.currentIntent != nil {
				t.Fatalf("接受后的 parent = %#v, want state=%q completed=%v current=nil", parent, tt.wantParent, tt.wantCompleted)
			}
			wantV2 := "N"
			if tt.wantCompleted {
				wantV2 = ""
			}
			if parent.v2State != wantV2 {
				t.Fatalf("接受后的 parent v2_state = %q, want %q", parent.v2State, wantV2)
			}
			note, lack := readDownloadIntentAcceptanceProgress(t, store, fixture.subscriptionID)
			if !reflect.DeepEqual(note, tt.wantNote) || lack != tt.wantLack {
				t.Fatalf("接受后的订阅进度 = note:%v lack:%d, want note:%v lack:%d", note, lack, tt.wantNote, tt.wantLack)
			}

			states := readDownloadIntentAcceptanceEpisodeStates(t, store, fixture.subscriptionID)
			for _, episode := range tt.allEpisodes {
				want := subscription.SubscriptionEpisodeMissing
				if containsDownloadIntentAcceptanceEpisode(tt.boundEpisodes, episode) {
					want = subscription.SubscriptionEpisodeIndexed
				}
				if states[episode] != want {
					t.Fatalf("episode %#v state = %q, want %q", episode, states[episode], want)
				}
			}

			replayed, replayReleased, err := store.AcceptDownloadIntent(
				t.Context(),
				username,
				fixture.intent.ID,
				databaseIntentV1Hash,
			)
			if err != nil || replayed.State != downloader.IntentDownloading || replayed.TorrentHash != databaseIntentV1Hash {
				t.Fatalf("相同 hash 重放 = (%#v, %v)", replayed, err)
			}
			if !replayReleased {
				t.Fatal("相同 hash 重放未识别已释放订阅")
			}
			replayedParent := readDownloadIntentAcceptanceParent(t, store, fixture.subscriptionID)
			if !replayedParent.updatedAt.Equal(parent.updatedAt) ||
				!sameDownloadIntentAcceptanceTime(replayedParent.completedAt, parent.completedAt) {
				t.Fatalf("相同 hash 重放修改 parent: before=%#v after=%#v", parent, replayedParent)
			}
		})
	}
}

func TestAcceptDownloadIntentPostgresCountsAbsentCurrentTargetRowsAsMissing(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	const username = "acceptance-current-target-owner"
	insertDownloadIntentIntegrationUsers(t, store, username)

	fixture := prepareDownloadIntentAcceptanceFixture(
		t,
		store,
		username,
		"absent-current-target-row",
		"电视剧",
		[]subscription.Episode{
			{Season: 1, Episode: 1},
			{Season: 1, Episode: 2},
			{Season: 1, Episode: 3},
			{Season: 2, Episode: 1},
		},
		[]subscription.Episode{
			{Season: 1, Episode: 1},
			{Season: 1, Episode: 2},
		},
		"",
	)
	if _, err := store.db.Pool.Exec(t.Context(), `
		UPDATE subscriptions
		SET season=1, config=config || '{"start_episode":1,"total_episode":3,"manual_total_episode":true}'::jsonb
		WHERE id=$1`, fixture.subscriptionID); err != nil {
		t.Fatalf("设置当前目标: %v", err)
	}
	if result, err := store.db.Pool.Exec(t.Context(), `
		DELETE FROM subscription_episodes
		WHERE subscription_id=$1 AND season=1 AND episode=3`, fixture.subscriptionID,
	); err != nil || result.RowsAffected() != 1 {
		t.Fatalf("删除当前目标行: rows=%d err=%v", result.RowsAffected(), err)
	}

	_, released, err := store.AcceptDownloadIntent(
		t.Context(), username, fixture.intent.ID, databaseIntentV1Hash,
	)
	if err != nil {
		t.Fatalf("AcceptDownloadIntent() error = %v", err)
	}
	parent := readDownloadIntentAcceptanceParent(t, store, fixture.subscriptionID)
	if !released || parent.state != subscription.PendingSearch || parent.completed {
		t.Fatalf("接受后的 parent = %#v, released=%v", parent, released)
	}
	note, lack := readDownloadIntentAcceptanceProgress(t, store, fixture.subscriptionID)
	if !reflect.DeepEqual(note, []int{1, 2}) || lack != 1 {
		t.Fatalf("当前目标进度 = note:%v lack:%d, want note:[1 2] lack:1", note, lack)
	}
}

func TestAcceptDownloadIntentPostgresPreservesPendingIdentification(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	const username = "acceptance-pending-identification-owner"
	insertDownloadIntentIntegrationUsers(t, store, username)
	episodes := []subscription.Episode{
		{Season: 1, Episode: 1},
		{Season: 1, Episode: 2},
	}
	fixture := prepareDownloadIntentAcceptanceFixture(
		t,
		store,
		username,
		"pending-identification",
		"电视剧",
		episodes,
		episodes[:1],
		"",
	)
	if _, err := store.db.Pool.Exec(t.Context(), `
		UPDATE subscriptions
		SET internal_state=$1, v2_state='P', updated_at=now()
		WHERE id=$2`, subscription.PendingIdentification, fixture.subscriptionID); err != nil {
		t.Fatalf("prepare pending-identification parent: %v", err)
	}

	accepted, released, err := store.AcceptDownloadIntent(
		t.Context(), username, fixture.intent.ID, databaseIntentV1Hash,
	)
	if err != nil || accepted.State != downloader.IntentDownloading || !released {
		t.Fatalf("pending-identification acceptance = (%#v, %v)", accepted, err)
	}
	parent := readDownloadIntentAcceptanceParent(t, store, fixture.subscriptionID)
	if parent.state != subscription.PendingIdentification || parent.v2State != "P" ||
		parent.completed || parent.completedAt != nil || parent.currentIntent != nil {
		t.Fatalf("pending-identification parent = %#v", parent)
	}
	note, lack := readDownloadIntentAcceptanceProgress(t, store, fixture.subscriptionID)
	if !reflect.DeepEqual(note, []int{1}) || lack != 1 {
		t.Fatalf("pending-identification progress = note:%v lack:%d, want note:[1] lack:1", note, lack)
	}
	states := readDownloadIntentAcceptanceEpisodeStates(t, store, fixture.subscriptionID)
	if states[episodes[0]] != subscription.SubscriptionEpisodeIndexed ||
		states[episodes[1]] != subscription.SubscriptionEpisodeMissing {
		t.Fatalf("pending-identification episode states = %#v", states)
	}

	beforeReplay := parent.updatedAt
	replayed, replayReleased, err := store.AcceptDownloadIntent(
		t.Context(), username, fixture.intent.ID, databaseIntentV1Hash,
	)
	if err != nil || replayed.State != downloader.IntentDownloading || !replayReleased {
		t.Fatalf("pending-identification acceptance replay = (%#v, %v)", replayed, err)
	}
	afterReplay := readDownloadIntentAcceptanceParent(t, store, fixture.subscriptionID)
	if !afterReplay.updatedAt.Equal(beforeReplay) || afterReplay.state != subscription.PendingIdentification ||
		afterReplay.v2State != "P" || afterReplay.currentIntent != nil || afterReplay.completed {
		t.Fatalf("pending-identification replay changed parent: before=%#v after=%#v", parent, afterReplay)
	}
}

func TestAcceptDownloadIntentPostgresRepairsMalformedLegacyProgress(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	const username = "acceptance-repair-owner"
	insertDownloadIntentIntegrationUsers(t, store, username)
	episodes := []subscription.Episode{
		{Season: 1, Episode: 1},
		{Season: 1, Episode: 2},
	}
	fixture := prepareDownloadIntentAcceptanceFixture(
		t,
		store,
		username,
		"repair-malformed-progress",
		"电视剧",
		episodes,
		episodes[:1],
		"",
	)
	if _, err := store.db.Pool.Exec(t.Context(), `
		UPDATE subscriptions
		SET config=config || '{"note":"corrupt","lack_episode":999}'::jsonb
		WHERE id=$1`, fixture.subscriptionID); err != nil {
		t.Fatalf("准备损坏旧进度: %v", err)
	}

	accepted, released, err := store.AcceptDownloadIntent(
		t.Context(), username, fixture.intent.ID, databaseIntentV1Hash,
	)
	if err != nil || accepted.State != downloader.IntentDownloading || !released {
		t.Fatalf("接受并修复旧进度 = (%#v, %v)", accepted, err)
	}
	parent := readDownloadIntentAcceptanceParent(t, store, fixture.subscriptionID)
	if parent.state != subscription.PendingSearch || parent.completed || parent.currentIntent != nil {
		t.Fatalf("修复后的 parent = %#v", parent)
	}
	note, lack := readDownloadIntentAcceptanceProgress(t, store, fixture.subscriptionID)
	if !reflect.DeepEqual(note, []int{1}) || lack != 1 {
		t.Fatalf("修复后的订阅进度 = note:%v lack:%d, want note:[1] lack:1", note, lack)
	}
}

func TestAcceptDownloadIntentPostgresPersistsAcceptanceWhenConfigShapeIsInvalid(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	const username = "acceptance-invalid-config-owner"
	insertDownloadIntentIntegrationUsers(t, store, username)
	episode := subscription.Episode{Season: 2, Episode: 1}
	fixture := prepareDownloadIntentAcceptanceFixture(
		t,
		store,
		username,
		"invalid-config-shape",
		"电视剧",
		[]subscription.Episode{episode},
		[]subscription.Episode{episode},
		"",
	)
	if _, err := store.db.Pool.Exec(t.Context(), `
		UPDATE subscriptions SET config='[]'::jsonb WHERE id=$1`, fixture.subscriptionID); err != nil {
		t.Fatalf("准备非法 config 形状: %v", err)
	}

	accepted, released, err := store.AcceptDownloadIntent(
		t.Context(), username, fixture.intent.ID, databaseIntentV1Hash,
	)
	if err != nil || accepted.State != downloader.IntentDownloading {
		t.Fatalf("非法 config 形状下接受 = (%#v, %v)", accepted, err)
	}
	if released {
		t.Fatal("非法 config 形状下错误报告订阅已释放")
	}
	parent := readDownloadIntentAcceptanceParent(t, store, fixture.subscriptionID)
	if parent.state != subscription.Acquired || parent.completed || parent.currentIntent == nil || *parent.currentIntent != fixture.intent.ID {
		t.Fatalf("非法 config 形状修改 parent: %#v", parent)
	}
	states := readDownloadIntentAcceptanceEpisodeStates(t, store, fixture.subscriptionID)
	if states[episode] != subscription.SubscriptionEpisodeAcquired {
		t.Fatalf("非法 config 形状消费 episode: %q", states[episode])
	}

	replayed, replayReleased, err := store.AcceptDownloadIntent(
		t.Context(), username, fixture.intent.ID, databaseIntentV1Hash,
	)
	if err != nil || replayed.State != downloader.IntentDownloading {
		t.Fatalf("非法 config 形状下重复接受 = (%#v, %v)", replayed, err)
	}
	if replayReleased {
		t.Fatal("非法 config 形状重放错误报告订阅已释放")
	}
	replayedParent := readDownloadIntentAcceptanceParent(t, store, fixture.subscriptionID)
	if !replayedParent.updatedAt.Equal(parent.updatedAt) {
		t.Fatalf("重复接受修改非法 config parent: before=%#v after=%#v", parent, replayedParent)
	}
}

func TestAcceptDownloadIntentPostgresRejectsHashConflictWithoutPartialWrites(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	const username = "acceptance-hash-owner"
	insertDownloadIntentIntegrationUsers(t, store, username, "acceptance-hash-other")
	episodes := []subscription.Episode{{Season: 3, Episode: 1}}
	fixture := prepareDownloadIntentAcceptanceFixture(
		t,
		store,
		username,
		"hash-conflict",
		"电视剧",
		episodes,
		episodes,
		databaseIntentV1Hash,
	)

	_, _, err := store.AcceptDownloadIntent(
		t.Context(),
		username,
		fixture.intent.ID,
		databaseIntentDifferentV1Hash,
	)
	if !errors.Is(err, ErrDownloadIntentStateConflict) {
		t.Fatalf("不同 hash acceptance error = %v, want ErrDownloadIntentStateConflict", err)
	}
	stored, err := store.GetDownloadIntent(t.Context(), username, fixture.intent.ID)
	if err != nil {
		t.Fatalf("读取 hash 冲突后的 intent: %v", err)
	}
	if stored.State != downloader.IntentSubmitting || stored.TorrentHash != databaseIntentV1Hash {
		t.Fatalf("hash 冲突修改 intent: %#v", stored)
	}
	parent := readDownloadIntentAcceptanceParent(t, store, fixture.subscriptionID)
	if parent.state != subscription.Acquired || parent.currentIntent == nil || *parent.currentIntent != fixture.intent.ID || parent.completed {
		t.Fatalf("hash 冲突修改 parent: %#v", parent)
	}
	states := readDownloadIntentAcceptanceEpisodeStates(t, store, fixture.subscriptionID)
	if states[episodes[0]] != subscription.SubscriptionEpisodeAcquired {
		t.Fatalf("hash 冲突消费 episode: %q", states[episodes[0]])
	}

	for _, tt := range []struct {
		name     string
		username string
		intentID uuid.UUID
		wantErr  error
	}{
		{name: "其他 owner", username: "acceptance-hash-other", intentID: fixture.intent.ID, wantErr: ErrDownloadIntentNotFound},
		{name: "不存在 intent", username: username, intentID: uuid.New(), wantErr: ErrDownloadIntentNotFound},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, _, err := store.AcceptDownloadIntent(
				t.Context(), tt.username, tt.intentID, databaseIntentV1Hash,
			); !errors.Is(err, tt.wantErr) {
				t.Fatalf("AcceptDownloadIntent() error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestAcceptDownloadIntentPostgresOldIntentOnlyConsumesOwnEpisodes(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	const username = "acceptance-old-intent-owner"
	insertDownloadIntentIntegrationUsers(t, store, username)
	episodes := []subscription.Episode{
		{Season: 4, Episode: 1},
		{Season: 4, Episode: 2},
		{Season: 4, Episode: 3},
	}
	fixture := prepareDownloadIntentAcceptanceFixture(
		t,
		store,
		username,
		"old-intent",
		"电视剧",
		episodes,
		episodes[:1],
		"",
	)
	newIntent := createDownloadIntentAcceptanceIntent(t, store, username, "new-current")
	if _, err := store.db.Pool.Exec(t.Context(), `
		UPDATE download_intents SET subscription_id=$2 WHERE id=$1`,
		newIntent.ID, fixture.subscriptionID,
	); err != nil {
		t.Fatalf("绑定新 intent: %v", err)
	}
	if _, err := store.db.Pool.Exec(t.Context(), `
		UPDATE subscriptions SET current_download_intent_id=$1 WHERE id=$2`,
		newIntent.ID, fixture.subscriptionID,
	); err != nil {
		t.Fatalf("切换到新 current intent: %v", err)
	}
	if _, err := store.db.Pool.Exec(t.Context(), `
		UPDATE subscription_episodes
		SET state='acquired', download_intent_id=$1
		WHERE subscription_id=$2 AND season=$3 AND episode=$4`,
		newIntent.ID,
		fixture.subscriptionID,
		episodes[1].Season,
		episodes[1].Episode,
	); err != nil {
		t.Fatalf("绑定新 intent episode: %v", err)
	}

	accepted, released, err := store.AcceptDownloadIntent(
		t.Context(), username, fixture.intent.ID, databaseIntentV1Hash,
	)
	if err != nil || accepted.State != downloader.IntentDownloading {
		t.Fatalf("接受旧 intent = (%#v, %v)", accepted, err)
	}
	if released {
		t.Fatal("旧 intent acceptance 错误报告当前订阅已释放")
	}
	states := readDownloadIntentAcceptanceEpisodeStates(t, store, fixture.subscriptionID)
	if states[episodes[0]] != subscription.SubscriptionEpisodeIndexed ||
		states[episodes[1]] != subscription.SubscriptionEpisodeAcquired ||
		states[episodes[2]] != subscription.SubscriptionEpisodeMissing {
		t.Fatalf("旧 intent acceptance 越界消费: %#v", states)
	}
	parent := readDownloadIntentAcceptanceParent(t, store, fixture.subscriptionID)
	if parent.state != subscription.Acquired || parent.currentIntent == nil || *parent.currentIntent != newIntent.ID || parent.completed {
		t.Fatalf("旧 intent acceptance 修改新 current parent: %#v", parent)
	}
}

func TestAcceptDownloadIntentPostgresPreservesLegacyPendingIndex(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	const username = "acceptance-legacy-owner"
	insertDownloadIntentIntegrationUsers(t, store, username)
	episodes := []subscription.Episode{{Season: 5, Episode: 1}}
	fixture := prepareDownloadIntentAcceptanceFixture(
		t,
		store,
		username,
		"legacy-pending-index",
		"电视剧",
		episodes,
		episodes,
		"",
	)
	if _, err := store.db.Pool.Exec(t.Context(), `
		UPDATE subscriptions SET internal_state='pending_index', v2_state='R' WHERE id=$1`,
		fixture.subscriptionID,
	); err != nil {
		t.Fatalf("准备 legacy pending_index parent: %v", err)
	}
	if _, err := store.db.Pool.Exec(t.Context(), `
		UPDATE subscription_episodes SET state='pending_index' WHERE subscription_id=$1`,
		fixture.subscriptionID,
	); err != nil {
		t.Fatalf("准备 legacy pending_index episode: %v", err)
	}

	accepted, released, err := store.AcceptDownloadIntent(
		t.Context(), username, fixture.intent.ID, databaseIntentV1Hash,
	)
	if err != nil || accepted.State != downloader.IntentDownloading {
		t.Fatalf("legacy acceptance = (%#v, %v)", accepted, err)
	}
	if released {
		t.Fatal("legacy pending_index acceptance 错误报告订阅已释放")
	}
	parent := readDownloadIntentAcceptanceParent(t, store, fixture.subscriptionID)
	if parent.state != subscription.PendingIndex || parent.currentIntent == nil || *parent.currentIntent != fixture.intent.ID || parent.completed {
		t.Fatalf("legacy pending_index parent 被前移: %#v", parent)
	}
	states := readDownloadIntentAcceptanceEpisodeStates(t, store, fixture.subscriptionID)
	if states[episodes[0]] != subscription.SubscriptionEpisodePendingIndex {
		t.Fatalf("legacy pending_index episode 被前移: %q", states[episodes[0]])
	}
}

func TestAcceptDownloadIntentPostgresLocksIntentThenParentThenEpisodes(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	const username = "acceptance-lock-order-owner"
	insertDownloadIntentIntegrationUsers(t, store, username)

	t.Run("intent before parent and episodes", func(t *testing.T) {
		episodes := []subscription.Episode{{Season: 6, Episode: 1}}
		fixture := prepareDownloadIntentAcceptanceFixture(
			t, store, username, "intent-lock", "电视剧", episodes, episodes, "",
		)
		lockTx, err := store.db.Pool.Begin(t.Context())
		if err != nil {
			t.Fatalf("开始 intent lock 事务: %v", err)
		}
		defer func() { _ = lockTx.Rollback(context.Background()) }()
		var lockedIntent uuid.UUID
		if err := lockTx.QueryRow(t.Context(), `
			SELECT id FROM download_intents WHERE id=$1 FOR UPDATE`, fixture.intent.ID,
		).Scan(&lockedIntent); err != nil {
			t.Fatalf("锁定 intent: %v", err)
		}

		result, operationCtx := startDownloadIntentAcceptanceLockOrderOperation(
			t, store, username, fixture.intent.ID, "accept-intent-"+uuid.NewString(),
		)
		var lockedSubscription int64
		if err := lockTx.QueryRow(t.Context(), `
			SELECT id FROM subscriptions WHERE id=$1 FOR UPDATE NOWAIT`, fixture.subscriptionID,
		).Scan(&lockedSubscription); err != nil {
			t.Fatalf("acceptance 在 intent 之前锁了 parent: %v", err)
		}
		var lockedEpisode int64
		if err := lockTx.QueryRow(t.Context(), `
			SELECT id FROM subscription_episodes
			WHERE subscription_id=$1 FOR UPDATE NOWAIT`, fixture.subscriptionID,
		).Scan(&lockedEpisode); err != nil {
			t.Fatalf("acceptance 在 intent 之前锁了 episode: %v", err)
		}
		if err := lockTx.Commit(t.Context()); err != nil {
			t.Fatalf("释放 intent lock: %v", err)
		}
		assertDownloadIntentAcceptanceLockOrderResult(t, operationCtx, result)
	})

	t.Run("parent before episodes", func(t *testing.T) {
		episodes := []subscription.Episode{{Season: 7, Episode: 1}}
		fixture := prepareDownloadIntentAcceptanceFixture(
			t, store, username, "parent-lock", "电视剧", episodes, episodes, "",
		)
		lockTx, err := store.db.Pool.Begin(t.Context())
		if err != nil {
			t.Fatalf("开始 parent lock 事务: %v", err)
		}
		defer func() { _ = lockTx.Rollback(context.Background()) }()
		var lockedSubscription int64
		if err := lockTx.QueryRow(t.Context(), `
			SELECT id FROM subscriptions WHERE id=$1 FOR UPDATE`, fixture.subscriptionID,
		).Scan(&lockedSubscription); err != nil {
			t.Fatalf("锁定 parent: %v", err)
		}

		result, operationCtx := startDownloadIntentAcceptanceLockOrderOperation(
			t, store, username, fixture.intent.ID, "accept-parent-"+uuid.NewString(),
		)
		var lockedEpisode int64
		if err := lockTx.QueryRow(t.Context(), `
			SELECT id FROM subscription_episodes
			WHERE subscription_id=$1 FOR UPDATE NOWAIT`, fixture.subscriptionID,
		).Scan(&lockedEpisode); err != nil {
			t.Fatalf("acceptance 在 parent 之前锁了 episode: %v", err)
		}
		if err := lockTx.Commit(t.Context()); err != nil {
			t.Fatalf("释放 parent lock: %v", err)
		}
		assertDownloadIntentAcceptanceLockOrderResult(t, operationCtx, result)
	})
}

type downloadIntentAcceptanceFixture struct {
	subscriptionID int64
	intent         downloader.Intent
}

func prepareDownloadIntentAcceptanceFixture(
	t *testing.T,
	store *Store,
	username string,
	key string,
	mediaType string,
	allEpisodes []subscription.Episode,
	boundEpisodes []subscription.Episode,
	storedHash string,
) downloadIntentAcceptanceFixture {
	t.Helper()
	// 电视剧的目标事实来自订阅配置；没有 total_episode 时新版受理会判定
	// 「事实不可读」而只写 lack_episode，所以 fixture 必须给出有效目标。
	config := map[string]any{"fixture": key}
	var season *int
	if isDownloadIntentAcceptanceTV(mediaType) && len(allEpisodes) > 0 {
		season = &allEpisodes[0].Season
		start, total := allEpisodes[0].Episode, allEpisodes[0].Episode
		for _, episode := range allEpisodes {
			if episode.Season != *season {
				continue
			}
			if episode.Episode < start {
				start = episode.Episode
			}
			if episode.Episode > total {
				total = episode.Episode
			}
		}
		config["start_episode"] = start
		config["total_episode"] = total - start + 1
		config["note"] = []int{}
	}
	subscriptionID, err := store.CreateSubscription(t.Context(), NewSubscription{
		Username:    username,
		Name:        "acceptance-" + key,
		MediaType:   mediaType,
		Year:        "2026",
		MediaSource: "themoviedb",
		MediaID:     "acceptance-" + key,
		Season:      season,
		State:       subscription.PendingSearch,
		Config:      config,
	})
	if err != nil {
		t.Fatalf("创建 acceptance subscription: %v", err)
	}
	if len(allEpisodes) > 0 {
		if err := store.UpsertSubscriptionEpisodes(
			t.Context(), username, subscriptionID, allEpisodes,
		); err != nil {
			t.Fatalf("初始化 acceptance episodes: %v", err)
		}
	}
	intent := createDownloadIntentAcceptanceIntent(t, store, username, key)
	if _, err := store.db.Pool.Exec(t.Context(), `
		UPDATE download_intents SET subscription_id=$2 WHERE id=$1`,
		intent.ID, subscriptionID,
	); err != nil {
		t.Fatalf("绑定 acceptance intent: %v", err)
	}
	if _, err := store.db.Pool.Exec(t.Context(), `
		UPDATE subscriptions
		SET current_download_intent_id=$1, internal_state='acquired', v2_state='R'
		WHERE id=$2`, intent.ID, subscriptionID); err != nil {
		t.Fatalf("绑定 acceptance parent: %v", err)
	}
	for _, episode := range boundEpisodes {
		result, err := store.db.Pool.Exec(t.Context(), `
			UPDATE subscription_episodes
			SET state='acquired', download_intent_id=$4
			WHERE subscription_id=$1 AND season=$2 AND episode=$3`,
			subscriptionID,
			episode.Season,
			episode.Episode,
			intent.ID,
		)
		if err != nil || result.RowsAffected() != 1 {
			t.Fatalf("绑定 acceptance episode %#v: rows=%d err=%v", episode, result.RowsAffected(), err)
		}
	}
	stored, err := store.CompareAndSwapDownloadIntent(
		t.Context(),
		username,
		intent.ID,
		[]downloader.IntentState{downloader.IntentPlanned},
		downloader.IntentSubmitting,
		storedHash,
	)
	if err != nil {
		t.Fatalf("准备 submitting acceptance intent: %v", err)
	}
	return downloadIntentAcceptanceFixture{subscriptionID: subscriptionID, intent: stored}
}

func createDownloadIntentAcceptanceIntent(
	t *testing.T,
	store *Store,
	username string,
	key string,
) downloader.Intent {
	t.Helper()
	intent := mustDatabaseDownloadIntent(t, username, "acceptance-"+key)
	stored, err := store.CreateDownloadIntent(t.Context(), username, intent)
	if err != nil {
		t.Fatalf("创建 acceptance intent: %v", err)
	}
	return stored
}

type downloadIntentAcceptanceParentSnapshot struct {
	state         subscription.State
	v2State       string
	currentIntent *uuid.UUID
	completed     bool
	completedAt   *time.Time
	updatedAt     time.Time
}

func readDownloadIntentAcceptanceParent(
	t *testing.T,
	store *Store,
	subscriptionID int64,
) downloadIntentAcceptanceParentSnapshot {
	t.Helper()
	var snapshot downloadIntentAcceptanceParentSnapshot
	if err := store.db.Pool.QueryRow(t.Context(), `
		SELECT internal_state, v2_state, current_download_intent_id,
			completed_at IS NOT NULL, completed_at, updated_at
		FROM subscriptions WHERE id=$1`, subscriptionID).Scan(
		&snapshot.state,
		&snapshot.v2State,
		&snapshot.currentIntent,
		&snapshot.completed,
		&snapshot.completedAt,
		&snapshot.updatedAt,
	); err != nil {
		t.Fatalf("读取 acceptance parent: %v", err)
	}
	return snapshot
}

func readDownloadIntentAcceptanceEpisodeStates(
	t *testing.T,
	store *Store,
	subscriptionID int64,
) map[subscription.Episode]subscription.SubscriptionEpisodeState {
	t.Helper()
	rows, err := store.db.Pool.Query(t.Context(), `
		SELECT season, episode, state
		FROM subscription_episodes
		WHERE subscription_id=$1
		ORDER BY season, episode`, subscriptionID)
	if err != nil {
		t.Fatalf("读取 acceptance episodes: %v", err)
	}
	defer rows.Close()
	states := make(map[subscription.Episode]subscription.SubscriptionEpisodeState)
	for rows.Next() {
		var episode subscription.Episode
		var state subscription.SubscriptionEpisodeState
		if err := rows.Scan(&episode.Season, &episode.Episode, &state); err != nil {
			t.Fatalf("扫描 acceptance episode: %v", err)
		}
		states[episode] = state
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("遍历 acceptance episodes: %v", err)
	}
	return states
}

func readDownloadIntentAcceptanceProgress(
	t *testing.T,
	store *Store,
	subscriptionID int64,
) ([]int, int) {
	t.Helper()
	var rawConfig []byte
	if err := store.db.Pool.QueryRow(t.Context(), `
		SELECT config FROM subscriptions WHERE id=$1`, subscriptionID,
	).Scan(&rawConfig); err != nil {
		t.Fatalf("读取 acceptance progress: %v", err)
	}
	var config struct {
		Note        []int `json:"note"`
		LackEpisode int   `json:"lack_episode"`
	}
	if err := json.Unmarshal(rawConfig, &config); err != nil {
		t.Fatalf("解析 acceptance progress: %v", err)
	}
	return config.Note, config.LackEpisode
}

func containsDownloadIntentAcceptanceEpisode(
	episodes []subscription.Episode,
	target subscription.Episode,
) bool {
	for _, episode := range episodes {
		if episode == target {
			return true
		}
	}
	return false
}

func sameDownloadIntentAcceptanceTime(left, right *time.Time) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return left.Equal(*right)
}

func stringsLower(value string) string {
	result := []byte(value)
	for index, character := range result {
		if character >= 'A' && character <= 'F' {
			result[index] = character + ('a' - 'A')
		}
	}
	return string(result)
}

type downloadIntentAcceptanceLockOrderResult struct {
	intent   downloader.Intent
	released bool
	err      error
}

func startDownloadIntentAcceptanceLockOrderOperation(
	t *testing.T,
	store *Store,
	username string,
	intentID uuid.UUID,
	applicationName string,
) (<-chan downloadIntentAcceptanceLockOrderResult, context.Context) {
	t.Helper()
	racingStore := newSubscriptionEpisodeRaceStore(t, store, applicationName)
	operationCtx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	t.Cleanup(cancel)
	result := make(chan downloadIntentAcceptanceLockOrderResult, 1)
	go func() {
		intent, released, err := racingStore.AcceptDownloadIntent(
			operationCtx,
			username,
			intentID,
			databaseIntentV1Hash,
		)
		result <- downloadIntentAcceptanceLockOrderResult{intent: intent, released: released, err: err}
	}()
	waitForSubscriptionEpisodeLockWait(t, store, applicationName)
	return result, operationCtx
}

func assertDownloadIntentAcceptanceLockOrderResult(
	t *testing.T,
	ctx context.Context,
	result <-chan downloadIntentAcceptanceLockOrderResult,
) {
	t.Helper()
	select {
	case received := <-result:
		if received.err != nil || received.intent.State != downloader.IntentDownloading || !received.released {
			t.Fatalf("锁释放后的 acceptance = (%#v, %v)", received.intent, received.err)
		}
	case <-ctx.Done():
		t.Fatalf("锁释放后的 acceptance 未完成: %v", ctx.Err())
	}
}

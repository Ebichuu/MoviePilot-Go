//go:build integration

package database

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/moviepilot-go/moviepilot-go/internal/media"
	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

func TestSubscriptionPreciseSearchPostgresScanAndOwnerRead(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	insertSubscriptionIntegrationUsers(t, store, "precise-scan-owner", "precise-scan-other")

	firstID := createSubscriptionIntegrationFixture(t, store, "precise-scan-owner", "precise-scan-first")
	secondID := createSubscriptionIntegrationFixture(t, store, "precise-scan-owner", "precise-scan-second")
	otherID := createSubscriptionIntegrationFixture(t, store, "precise-scan-other", "precise-scan-other")
	afterCutoffID := createSubscriptionIntegrationFixture(t, store, "precise-scan-owner", "precise-scan-after")
	activeID := createSubscriptionIntegrationFixture(t, store, "precise-scan-owner", "precise-scan-active")
	completedID := createSubscriptionIntegrationFixture(t, store, "precise-scan-owner", "precise-scan-completed")

	base := time.Date(2026, time.August, 11, 1, 0, 0, 0, time.UTC)
	cutoff := base.Add(time.Hour)
	updates := []struct {
		id          int64
		state       subscription.State
		v2State     string
		updatedAt   time.Time
		completedAt *time.Time
	}{
		{id: firstID, state: subscription.PendingSearch, v2State: "N", updatedAt: base},
		{id: secondID, state: subscription.PendingSearch, v2State: "N", updatedAt: base},
		{id: otherID, state: subscription.PendingSearch, v2State: "N", updatedAt: cutoff},
		{id: afterCutoffID, state: subscription.PendingSearch, v2State: "N", updatedAt: cutoff.Add(time.Millisecond)},
		{id: activeID, state: subscription.Active, v2State: "R", updatedAt: base},
		{id: completedID, state: subscription.Completed, v2State: "", updatedAt: base, completedAt: &base},
	}
	for _, update := range updates {
		if _, err := store.db.Pool.Exec(t.Context(), `
			UPDATE subscriptions
			SET internal_state=$1, v2_state=$2, updated_at=$3, completed_at=$4
			WHERE id=$5`, update.state, update.v2State, update.updatedAt, update.completedAt, update.id); err != nil {
			t.Fatalf("准备精准搜索扫描订阅 id=%d: %v", update.id, err)
		}
	}

	limited, err := store.ListSubscriptionsForPreciseSearch(t.Context(), cutoff, time.Time{}, 0, 2)
	if err != nil {
		t.Fatalf("有界扫描精准搜索订阅: %v", err)
	}
	assertOrderedSubscriptionIDs(t, limited, []int64{firstID, secondID})

	pageTwo, err := store.ListSubscriptionsForPreciseSearch(
		t.Context(),
		cutoff,
		limited[1].UpdatedAt,
		limited[1].ID,
		2,
	)
	if err != nil {
		t.Fatalf("按 keyset 游标继续扫描精准搜索订阅: %v", err)
	}
	assertOrderedSubscriptionIDs(t, pageTwo, []int64{otherID})

	all, err := store.ListSubscriptionsForPreciseSearch(t.Context(), cutoff, time.Time{}, 0, 100)
	if err != nil {
		t.Fatalf("扫描截止时间内精准搜索订阅: %v", err)
	}
	assertOrderedSubscriptionIDs(t, all, []int64{firstID, secondID, otherID})
	if all[2].Username != "precise-scan-other" {
		t.Fatalf("系统扫描未保留订阅 owner: %#v", all[2])
	}

	stored, err := store.GetActiveSubscription(t.Context(), " precise-scan-owner ", firstID)
	if err != nil {
		t.Fatalf("owner 读取活动订阅: %v", err)
	}
	if stored.ID != firstID || stored.Username != "precise-scan-owner" || stored.Config["fixture"] != "precise-scan-first" {
		t.Fatalf("活动订阅读取结果 = %#v", stored)
	}

	tests := []struct {
		name     string
		username string
		id       int64
	}{
		{name: "其他 owner", username: "precise-scan-other", id: firstID},
		{name: "已完成订阅", username: "precise-scan-owner", id: completedID},
		{name: "不存在订阅", username: "precise-scan-owner", id: completedID + 10_000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := store.GetActiveSubscription(t.Context(), tt.username, tt.id); !errors.Is(err, ErrSubscriptionNotFound) {
				t.Fatalf("GetActiveSubscription() error = %v, want ErrSubscriptionNotFound", err)
			}
		})
	}
}

func TestSubscriptionPreciseSearchPostgresCASAndTransaction(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	insertSubscriptionIntegrationUsers(t, store, "precise-cas-owner", "precise-cas-other")

	activeID := createSubscriptionIntegrationFixture(t, store, "precise-cas-owner", "precise-cas-active")
	if err := store.CompareAndSwapSubscriptionPreciseSearchState(
		t.Context(),
		"precise-cas-other",
		activeID,
		subscription.Active,
	); !errors.Is(err, ErrSubscriptionNotFound) {
		t.Fatalf("其他 owner CAS error = %v, want ErrSubscriptionNotFound", err)
	}
	if err := store.CompareAndSwapSubscriptionPreciseSearchState(
		t.Context(),
		" precise-cas-owner ",
		activeID,
		subscription.Active,
	); err != nil {
		t.Fatalf("pending_search 推进到 active: %v", err)
	}
	if err := store.CompareAndSwapSubscriptionPreciseSearchState(
		t.Context(),
		"precise-cas-owner",
		activeID,
		subscription.Acquired,
	); !errors.Is(err, ErrSubscriptionPreciseSearchStateConflict) {
		t.Fatalf("过期 CAS error = %v, want ErrSubscriptionPreciseSearchStateConflict", err)
	}
	active := mustActiveSubscription(t, store, "precise-cas-owner", activeID)
	if active.InternalState != subscription.Active || active.V2State != "R" {
		t.Fatalf("CAS 后状态 = (%q, %q), want (%q, R)", active.InternalState, active.V2State, subscription.Active)
	}

	acquiredID := createSubscriptionIntegrationFixture(t, store, "precise-cas-owner", "precise-cas-acquired")
	rollbackTx, err := store.db.Pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("开始回滚 CAS 事务: %v", err)
	}
	defer func() { _ = rollbackTx.Rollback(context.Background()) }()
	if err := store.CompareAndSwapSubscriptionPreciseSearchStateTx(
		t.Context(),
		rollbackTx,
		"precise-cas-owner",
		acquiredID,
		subscription.Acquired,
	); err != nil {
		t.Fatalf("事务内推进到 acquired: %v", err)
	}
	if err := rollbackTx.Rollback(t.Context()); err != nil {
		t.Fatalf("回滚精准搜索 CAS: %v", err)
	}
	rolledBack := mustActiveSubscription(t, store, "precise-cas-owner", acquiredID)
	if rolledBack.InternalState != subscription.PendingSearch {
		t.Fatalf("回滚后状态 = %q, want %q", rolledBack.InternalState, subscription.PendingSearch)
	}

	commitTx, err := store.db.Pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("开始提交 CAS 事务: %v", err)
	}
	defer func() { _ = commitTx.Rollback(context.Background()) }()
	if err := store.CompareAndSwapSubscriptionPreciseSearchStateTx(
		t.Context(),
		commitTx,
		"precise-cas-owner",
		acquiredID,
		subscription.Acquired,
	); err != nil {
		t.Fatalf("提交事务内推进到 acquired: %v", err)
	}
	if err := commitTx.Commit(t.Context()); err != nil {
		t.Fatalf("提交精准搜索 CAS: %v", err)
	}
	acquired := mustActiveSubscription(t, store, "precise-cas-owner", acquiredID)
	if acquired.InternalState != subscription.Acquired || acquired.V2State != "R" {
		t.Fatalf("提交后状态 = (%q, %q), want (%q, R)", acquired.InternalState, acquired.V2State, subscription.Acquired)
	}
}

func TestCompleteSatisfiedTVSubscriptionPostgresStateAndOwnership(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	insertSubscriptionIntegrationUsers(t, store, "satisfied-owner", "satisfied-other")

	pendingID := createSubscriptionIntegrationFixture(t, store, "satisfied-owner", "satisfied-pending")
	activeID := createSubscriptionIntegrationFixture(t, store, "satisfied-owner", "satisfied-active")
	acquiredID := createSubscriptionIntegrationFixture(t, store, "satisfied-owner", "satisfied-acquired")
	indexedID := createSubscriptionIntegrationFixture(t, store, "satisfied-owner", "satisfied-indexed")
	invalidConfigID := createSubscriptionIntegrationFixture(t, store, "satisfied-owner", "satisfied-invalid-config")
	currentIntentID := createSubscriptionIntegrationFixture(t, store, "satisfied-owner", "satisfied-current-intent")
	bestVersionID := createSubscriptionIntegrationFixture(t, store, "satisfied-owner", "satisfied-best-version")
	bestVersionBoolID := createSubscriptionIntegrationFixture(t, store, "satisfied-owner", "satisfied-best-version-bool")
	bestVersionFalseID := createSubscriptionIntegrationFixture(t, store, "satisfied-owner", "satisfied-best-version-false")
	bestVersionZeroID := createSubscriptionIntegrationFixture(t, store, "satisfied-owner", "satisfied-best-version-zero")
	staleSnapshotID := createSubscriptionIntegrationFixture(t, store, "satisfied-owner", "satisfied-stale-snapshot")
	movieID := createSubscriptionIntegrationFixture(t, store, "satisfied-owner", "satisfied-movie")
	for _, id := range []int64{
		pendingID, activeID, acquiredID, indexedID, invalidConfigID, currentIntentID, bestVersionID,
		bestVersionBoolID, bestVersionFalseID, bestVersionZeroID, staleSnapshotID,
	} {
		if _, err := store.db.Pool.Exec(t.Context(), `
			UPDATE subscriptions
			SET media_type=$1
			WHERE id=$2`, media.TypeTV, id); err != nil {
			t.Fatalf("准备 TV 满足订阅 id=%d: %v", id, err)
		}
	}
	if _, err := store.db.Pool.Exec(t.Context(), `
		UPDATE subscriptions
		SET internal_state=$1, v2_state='R', updated_at=now()
		WHERE id=$2`, subscription.Active, activeID); err != nil {
		t.Fatalf("准备 active 满足订阅: %v", err)
	}
	if _, err := store.db.Pool.Exec(t.Context(), `
		UPDATE subscriptions
		SET internal_state=$1, v2_state='R', updated_at=now()
		WHERE id=$2`, subscription.Acquired, acquiredID); err != nil {
		t.Fatalf("准备 acquired 满足订阅: %v", err)
	}
	if _, err := store.db.Pool.Exec(t.Context(), `
		UPDATE subscriptions
		SET internal_state=$1, v2_state='R', updated_at=now()
		WHERE id=$2`, subscription.Indexed, indexedID); err != nil {
		t.Fatalf("准备 indexed 满足订阅: %v", err)
	}
	if _, err := store.db.Pool.Exec(t.Context(), `
		UPDATE subscriptions
		SET config='[]'::jsonb
		WHERE id=$1`, invalidConfigID); err != nil {
		t.Fatalf("准备非法顶层配置订阅: %v", err)
	}
	if _, err := store.db.Pool.Exec(t.Context(), `
		UPDATE subscriptions
		SET config=config || '{"best_version":1}'::jsonb
		WHERE id=$1`, bestVersionID); err != nil {
		t.Fatalf("准备洗版订阅: %v", err)
	}
	for _, fixture := range []struct {
		id    int64
		value string
	}{
		{id: bestVersionBoolID, value: "true"},
		{id: bestVersionFalseID, value: "false"},
		{id: bestVersionZeroID, value: "0"},
	} {
		if _, err := store.db.Pool.Exec(t.Context(), `
			UPDATE subscriptions
			SET config=config || jsonb_build_object('best_version', $1::jsonb)
			WHERE id=$2`, fixture.value, fixture.id); err != nil {
			t.Fatalf("准备 best_version=%s 订阅: %v", fixture.value, err)
		}
	}
	intent := mustDatabaseDownloadIntent(t, "satisfied-owner", "satisfied-current-intent")
	if _, err := store.CreateDownloadIntent(t.Context(), "satisfied-owner", intent); err != nil {
		t.Fatalf("创建当前下载意图: %v", err)
	}
	if _, err := store.db.Pool.Exec(t.Context(), `
		UPDATE subscriptions
		SET internal_state=$1, v2_state='R', current_download_intent_id=$2
		WHERE id=$3`, subscription.Active, intent.ID, currentIntentID); err != nil {
		t.Fatalf("准备带当前意图订阅: %v", err)
	}
	updatedAt := func(id int64) time.Time {
		t.Helper()
		var result time.Time
		if err := store.db.Pool.QueryRow(t.Context(), `
			SELECT updated_at
			FROM subscriptions
			WHERE id=$1`, id).Scan(&result); err != nil {
			t.Fatalf("读取订阅快照时间 id=%d: %v", id, err)
		}
		return result
	}
	pendingUpdatedAt := updatedAt(pendingID)
	staleUpdatedAt := updatedAt(staleSnapshotID)
	if _, err := store.db.Pool.Exec(t.Context(), `
		UPDATE subscriptions
		SET config=config || '{"start_episode":2}'::jsonb,
			updated_at=updated_at + interval '1 second'
		WHERE id=$1`, staleSnapshotID); err != nil {
		t.Fatalf("推进过期目标快照: %v", err)
	}

	if err := store.CompleteSatisfiedTVSubscription(
		t.Context(), "satisfied-other", pendingID, subscription.PendingSearch, pendingUpdatedAt,
	); !errors.Is(err, ErrSubscriptionNotFound) {
		t.Fatalf("其他 owner 完成 error = %v, want not found", err)
	}
	if err := store.CompleteSatisfiedTVSubscription(
		t.Context(), "satisfied-owner", pendingID, subscription.Active, pendingUpdatedAt,
	); !errors.Is(err, ErrSubscriptionPreciseSearchStateConflict) {
		t.Fatalf("RSS 越界完成 pending_search error = %v, want state conflict", err)
	}
	for _, target := range []struct {
		id    int64
		state subscription.State
	}{
		{id: pendingID, state: subscription.PendingSearch},
		{id: activeID, state: subscription.Active},
		{id: indexedID, state: subscription.Indexed},
		{id: bestVersionFalseID, state: subscription.PendingSearch},
		{id: bestVersionZeroID, state: subscription.PendingSearch},
	} {
		if err := store.CompleteSatisfiedTVSubscription(
			t.Context(), " satisfied-owner ", target.id, target.state, updatedAt(target.id),
		); err != nil {
			t.Fatalf("完成已满足订阅 id=%d: %v", target.id, err)
		}
		var (
			state       subscription.State
			v2State     string
			completed   bool
			lackEpisode int
			fixture     string
		)
		if err := store.db.Pool.QueryRow(t.Context(), `
			SELECT internal_state, v2_state, completed_at IS NOT NULL,
				(config->>'lack_episode')::integer, config->>'fixture'
			FROM subscriptions
			WHERE id=$1`, target.id).Scan(&state, &v2State, &completed, &lackEpisode, &fixture); err != nil {
			t.Fatalf("读取已满足订阅 id=%d: %v", target.id, err)
		}
		if state != subscription.Completed || v2State != "" || !completed ||
			lackEpisode != 0 || fixture == "" {
			t.Fatalf(
				"已满足订阅 id=%d = state:%q v2:%q completed:%v lack:%d fixture:%q",
				target.id, state, v2State, completed, lackEpisode, fixture,
			)
		}
	}
	if err := store.CompleteSatisfiedTVSubscription(
		t.Context(), "satisfied-owner", acquiredID, subscription.PendingSearch, updatedAt(acquiredID),
	); !errors.Is(err, ErrSubscriptionPreciseSearchStateConflict) {
		t.Fatalf("acquired 订阅完成 error = %v, want state conflict", err)
	}
	if err := store.CompleteSatisfiedTVSubscription(
		t.Context(), "satisfied-owner", invalidConfigID, subscription.PendingSearch, updatedAt(invalidConfigID),
	); !errors.Is(err, ErrSubscriptionPreciseSearchStateConflict) {
		t.Fatalf("非法顶层配置订阅完成 error = %v, want state conflict", err)
	}
	var invalidConfigState subscription.State
	var invalidConfig string
	if err := store.db.Pool.QueryRow(t.Context(), `
		SELECT internal_state, config::text
		FROM subscriptions
		WHERE id=$1`, invalidConfigID).Scan(&invalidConfigState, &invalidConfig); err != nil {
		t.Fatalf("读取非法顶层配置订阅: %v", err)
	}
	if invalidConfigState != subscription.PendingSearch || invalidConfig != "[]" {
		t.Fatalf("非法顶层配置订阅被改写为 state:%q config:%s", invalidConfigState, invalidConfig)
	}
	if err := store.CompleteSatisfiedTVSubscription(
		t.Context(), "satisfied-owner", currentIntentID, subscription.Active, updatedAt(currentIntentID),
	); !errors.Is(err, ErrSubscriptionPreciseSearchStateConflict) {
		t.Fatalf("带当前意图订阅完成 error = %v, want state conflict", err)
	}
	var currentState subscription.State
	var storedIntent uuid.UUID
	if err := store.db.Pool.QueryRow(t.Context(), `
		SELECT internal_state, current_download_intent_id
		FROM subscriptions
		WHERE id=$1`, currentIntentID).Scan(&currentState, &storedIntent); err != nil {
		t.Fatalf("读取带当前意图订阅: %v", err)
	}
	if currentState != subscription.Active || storedIntent != intent.ID {
		t.Fatalf("带当前意图订阅被改写为 state:%q intent:%s", currentState, storedIntent)
	}
	for _, target := range []struct {
		name      string
		id        int64
		state     subscription.State
		updatedAt time.Time
	}{
		{name: "数值洗版", id: bestVersionID, state: subscription.PendingSearch, updatedAt: updatedAt(bestVersionID)},
		{name: "布尔洗版", id: bestVersionBoolID, state: subscription.PendingSearch, updatedAt: updatedAt(bestVersionBoolID)},
		{name: "过期目标快照", id: staleSnapshotID, state: subscription.PendingSearch, updatedAt: staleUpdatedAt},
		{name: "电影", id: movieID, state: subscription.PendingSearch, updatedAt: updatedAt(movieID)},
	} {
		if err := store.CompleteSatisfiedTVSubscription(
			t.Context(), "satisfied-owner", target.id, target.state, target.updatedAt,
		); !errors.Is(err, ErrSubscriptionPreciseSearchStateConflict) {
			t.Fatalf("%s订阅完成 error = %v, want state conflict", target.name, err)
		}
	}
	if err := store.CompleteSatisfiedTVSubscription(
		t.Context(), "satisfied-owner", pendingID, subscription.PendingSearch, pendingUpdatedAt,
	); !errors.Is(err, ErrSubscriptionNotFound) {
		t.Fatalf("完成重放 error = %v, want not found", err)
	}
}

func TestCompleteSatisfiedMovieSubscriptionPostgresStateAndOwnership(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	insertSubscriptionIntegrationUsers(t, store, "movie-satisfied-owner", "movie-satisfied-other")

	completedID := createSubscriptionIntegrationFixture(t, store, "movie-satisfied-owner", "movie-satisfied")
	bestVersionID := createSubscriptionIntegrationFixture(t, store, "movie-satisfied-owner", "movie-best-version")
	tvID := createSubscriptionIntegrationFixture(t, store, "movie-satisfied-owner", "movie-tv")
	if _, err := store.db.Pool.Exec(t.Context(), `
		UPDATE subscriptions
		SET media_type=$1
		WHERE id IN ($2, $3)`, media.TypeMovie, completedID, bestVersionID); err != nil {
		t.Fatalf("准备电影订阅: %v", err)
	}
	if _, err := store.db.Pool.Exec(t.Context(), `
		UPDATE subscriptions
		SET media_type=$1
		WHERE id=$2`, media.TypeTV, tvID); err != nil {
		t.Fatalf("准备 TV 对照订阅: %v", err)
	}
	if _, err := store.db.Pool.Exec(t.Context(), `
		UPDATE subscriptions
		SET config=config || '{"best_version":true}'::jsonb
		WHERE id=$1`, bestVersionID); err != nil {
		t.Fatalf("准备电影洗版订阅: %v", err)
	}
	updatedAt := func(id int64) time.Time {
		t.Helper()
		var result time.Time
		if err := store.db.Pool.QueryRow(t.Context(), `SELECT updated_at FROM subscriptions WHERE id=$1`, id).Scan(&result); err != nil {
			t.Fatalf("读取订阅快照时间 id=%d: %v", id, err)
		}
		return result
	}

	if err := store.CompleteSatisfiedMovieSubscription(
		t.Context(), "movie-satisfied-other", completedID, subscription.PendingSearch, updatedAt(completedID),
	); !errors.Is(err, ErrSubscriptionNotFound) {
		t.Fatalf("其他 owner 完成 error = %v, want not found", err)
	}
	if err := store.CompleteSatisfiedMovieSubscription(
		t.Context(), "movie-satisfied-owner", completedID, subscription.PendingSearch, updatedAt(completedID),
	); err != nil {
		t.Fatalf("完成媒体库已存在电影: %v", err)
	}
	var state subscription.State
	var v2State string
	var completed bool
	var fixture string
	if err := store.db.Pool.QueryRow(t.Context(), `
		SELECT internal_state, v2_state, completed_at IS NOT NULL, config->>'fixture'
		FROM subscriptions WHERE id=$1`, completedID).Scan(&state, &v2State, &completed, &fixture); err != nil {
		t.Fatalf("读取已完成电影订阅: %v", err)
	}
	if state != subscription.Completed || v2State != "" || !completed || fixture == "" {
		t.Fatalf("已完成电影订阅 = state:%q v2:%q completed:%v fixture:%q", state, v2State, completed, fixture)
	}
	for _, target := range []struct {
		name string
		id   int64
	}{
		{name: "洗版电影", id: bestVersionID},
		{name: "TV", id: tvID},
	} {
		if err := store.CompleteSatisfiedMovieSubscription(
			t.Context(), "movie-satisfied-owner", target.id, subscription.PendingSearch, updatedAt(target.id),
		); !errors.Is(err, ErrSubscriptionPreciseSearchStateConflict) {
			t.Fatalf("%s完成 error = %v, want state conflict", target.name, err)
		}
	}
	if err := store.CompleteSatisfiedMovieSubscription(
		t.Context(), "movie-satisfied-owner", completedID, subscription.PendingSearch, updatedAt(completedID),
	); !errors.Is(err, ErrSubscriptionNotFound) {
		t.Fatalf("完成重放 error = %v, want not found", err)
	}
}

func TestDownloadIntentSubscriptionBindingPostgresTransactionAndOwnership(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	insertSubscriptionIntegrationUsers(t, store, "binding-owner", "binding-other")

	primaryID := createSubscriptionIntegrationFixture(t, store, "binding-owner", "binding-primary")
	secondaryID := createSubscriptionIntegrationFixture(t, store, "binding-owner", "binding-secondary")
	otherID := createSubscriptionIntegrationFixture(t, store, "binding-other", "binding-other")
	completedID := createSubscriptionIntegrationFixture(t, store, "binding-owner", "binding-completed")
	if _, err := store.db.Pool.Exec(t.Context(), `
		UPDATE subscriptions
		SET internal_state=$1, v2_state='', completed_at=now(), updated_at=now()
		WHERE id=$2`, subscription.Completed, completedID); err != nil {
		t.Fatalf("准备已完成绑定目标: %v", err)
	}

	intent := mustDatabaseDownloadIntent(t, "binding-owner", "binding-transaction")
	rollbackTx, err := store.db.Pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("开始绑定回滚事务: %v", err)
	}
	defer func() { _ = rollbackTx.Rollback(context.Background()) }()
	if _, err := store.CreateDownloadIntentTx(t.Context(), rollbackTx, "binding-owner", intent); err != nil {
		t.Fatalf("回滚事务创建下载意图: %v", err)
	}
	if err := store.BindDownloadIntentSubscriptionTx(t.Context(), rollbackTx, "binding-owner", intent.ID, primaryID); err != nil {
		t.Fatalf("回滚事务绑定订阅: %v", err)
	}
	if err := store.CompareAndSwapSubscriptionPreciseSearchStateTx(
		t.Context(), rollbackTx, "binding-owner", primaryID, subscription.Acquired,
	); err != nil {
		t.Fatalf("回滚事务推进订阅状态: %v", err)
	}
	if err := rollbackTx.Rollback(t.Context()); err != nil {
		t.Fatalf("回滚意图、绑定和状态事务: %v", err)
	}
	if _, err := store.GetDownloadIntent(t.Context(), "binding-owner", intent.ID); !errors.Is(err, ErrDownloadIntentNotFound) {
		t.Fatalf("回滚后下载意图 error = %v, want ErrDownloadIntentNotFound", err)
	}
	if current := mustActiveSubscription(t, store, "binding-owner", primaryID).InternalState; current != subscription.PendingSearch {
		t.Fatalf("回滚后订阅状态 = %q, want %q", current, subscription.PendingSearch)
	}

	commitTx, err := store.db.Pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("开始绑定提交事务: %v", err)
	}
	defer func() { _ = commitTx.Rollback(context.Background()) }()
	if _, err := store.CreateDownloadIntentTx(t.Context(), commitTx, "binding-owner", intent); err != nil {
		t.Fatalf("提交事务创建下载意图: %v", err)
	}
	if err := store.BindDownloadIntentSubscriptionTx(t.Context(), commitTx, "binding-owner", intent.ID, primaryID); err != nil {
		t.Fatalf("提交事务绑定订阅: %v", err)
	}
	if err := store.CompareAndSwapSubscriptionPreciseSearchStateTx(
		t.Context(), commitTx, "binding-owner", primaryID, subscription.Acquired,
	); err != nil {
		t.Fatalf("提交事务推进订阅状态: %v", err)
	}
	if err := commitTx.Commit(t.Context()); err != nil {
		t.Fatalf("提交意图、绑定和状态事务: %v", err)
	}
	assertDownloadIntentSubscriptionID(t, store, intent.ID, primaryID)

	idempotentTx, err := store.db.Pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("开始幂等绑定事务: %v", err)
	}
	defer func() { _ = idempotentTx.Rollback(context.Background()) }()
	if err := store.BindDownloadIntentSubscriptionTx(t.Context(), idempotentTx, "binding-owner", intent.ID, primaryID); err != nil {
		t.Fatalf("幂等重放相同绑定: %v", err)
	}
	if err := idempotentTx.Commit(t.Context()); err != nil {
		t.Fatalf("提交幂等绑定: %v", err)
	}
	assertDownloadIntentSubscriptionID(t, store, intent.ID, primaryID)

	competing := mustDatabaseDownloadIntent(t, "binding-owner", "binding-competing-current")
	if _, err := store.CreateDownloadIntent(t.Context(), "binding-owner", competing); err != nil {
		t.Fatalf("创建竞争下载意图: %v", err)
	}
	competingTx, err := store.db.Pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("开始竞争绑定事务: %v", err)
	}
	defer func() { _ = competingTx.Rollback(context.Background()) }()
	if err := store.BindDownloadIntentSubscriptionTx(
		t.Context(), competingTx, "binding-owner", competing.ID, primaryID,
	); !errors.Is(err, ErrDownloadIntentSubscriptionBindingConflict) {
		t.Fatalf("竞争当前意图 error = %v, want ErrDownloadIntentSubscriptionBindingConflict", err)
	}
	if err := competingTx.Rollback(t.Context()); err != nil {
		t.Fatalf("回滚竞争绑定事务: %v", err)
	}

	tests := []struct {
		name           string
		username       string
		subscriptionID int64
		wantErr        error
	}{
		{name: "禁止改绑", username: "binding-owner", subscriptionID: secondaryID, wantErr: ErrDownloadIntentSubscriptionBindingConflict},
		{name: "其他 owner 不能操作意图", username: "binding-other", subscriptionID: otherID, wantErr: ErrDownloadIntentSubscriptionBindingNotFound},
		{name: "不能绑定其他 owner 订阅", username: "binding-owner", subscriptionID: otherID, wantErr: ErrDownloadIntentSubscriptionBindingNotFound},
		{name: "不能绑定已完成订阅", username: "binding-owner", subscriptionID: completedID, wantErr: ErrDownloadIntentSubscriptionBindingNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tx, err := store.db.Pool.Begin(t.Context())
			if err != nil {
				t.Fatalf("开始绑定拒绝事务: %v", err)
			}
			defer func() { _ = tx.Rollback(context.Background()) }()
			err = store.BindDownloadIntentSubscriptionTx(t.Context(), tx, tt.username, intent.ID, tt.subscriptionID)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("BindDownloadIntentSubscriptionTx() error = %v, want %v", err, tt.wantErr)
			}
		})
	}
	assertDownloadIntentSubscriptionID(t, store, intent.ID, primaryID)
}

func TestSubscriptionPreciseSearchPostgresErrorsAreSanitized(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	if _, err := store.db.Pool.Exec(t.Context(), `DROP TABLE subscriptions CASCADE`); err != nil {
		t.Fatalf("准备精准搜索数据库错误测试: %v", err)
	}

	const sensitiveOwner = "precise-sensitive-owner"
	_, err := store.GetActiveSubscription(t.Context(), sensitiveOwner, 919191)
	if !errors.Is(err, ErrSubscriptionPreciseSearchPersistence) {
		t.Fatalf("数据库错误 = %v, want ErrSubscriptionPreciseSearchPersistence", err)
	}
	for _, sensitive := range []string{sensitiveOwner, "919191", "subscriptions", "SELECT"} {
		if strings.Contains(err.Error(), sensitive) {
			t.Fatalf("精准搜索数据库错误泄露了 %q", sensitive)
		}
	}
}

func mustActiveSubscription(t *testing.T, store *Store, username string, id int64) Subscription {
	t.Helper()
	item, err := store.GetActiveSubscription(t.Context(), username, id)
	if err != nil {
		t.Fatalf("读取活动订阅 id=%d: %v", id, err)
	}
	return item
}

func assertOrderedSubscriptionIDs(t *testing.T, items []Subscription, want []int64) {
	t.Helper()
	if len(items) != len(want) {
		t.Fatalf("订阅数 = %d, want %d; items=%#v", len(items), len(want), items)
	}
	for index, id := range want {
		if items[index].ID != id {
			t.Fatalf("订阅 ID[%d] = %d, want %d", index, items[index].ID, id)
		}
	}
}

func assertDownloadIntentSubscriptionID(t *testing.T, store *Store, intentID uuid.UUID, want int64) {
	t.Helper()
	var got *int64
	if err := store.db.Pool.QueryRow(t.Context(), `
		SELECT subscription_id
		FROM download_intents
		WHERE id=$1`, intentID).Scan(&got); err != nil {
		t.Fatalf("读取下载意图订阅绑定: %v", err)
	}
	if got == nil || *got != want {
		t.Fatalf("下载意图订阅绑定 = %v, want %d", got, want)
	}
	var current *uuid.UUID
	if err := store.db.Pool.QueryRow(t.Context(), `
		SELECT current_download_intent_id
		FROM subscriptions
		WHERE id=$1`, want).Scan(&current); err != nil {
		t.Fatalf("读取订阅当前下载意图: %v", err)
	}
	if current == nil || *current != intentID {
		t.Fatalf("订阅当前下载意图 = %v, want %s", current, intentID)
	}
}

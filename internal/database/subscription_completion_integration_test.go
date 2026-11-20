//go:build integration

package database

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

func TestSubscriptionStorePostgresCompletionAllowsResubscribe(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	insertSubscriptionIntegrationUsers(t, store, "completion-owner")

	firstID := createSubscriptionIntegrationFixture(t, store, "completion-owner", "completion-media")
	if _, err := store.db.Pool.Exec(t.Context(), `UPDATE subscriptions SET internal_state=$1, v2_state=$2 WHERE id=$3`, subscription.Indexed, "R", firstID); err != nil {
		t.Fatalf("准备 Indexed 订阅: %v", err)
	}
	if err := store.CompleteSubscription(t.Context(), firstID, "completion-owner", false); err != nil {
		t.Fatalf("完成订阅: %v", err)
	}

	active, err := store.ListSubscriptions(t.Context(), "completion-owner", false)
	if err != nil {
		t.Fatalf("读取活动订阅: %v", err)
	}
	if _, found := findSubscriptionByID(active, firstID); found {
		t.Fatalf("完成订阅 id=%d 仍出现在活动列表", firstID)
	}
	history, err := store.ListCompletedSubscriptions(t.Context(), "completion-owner", false)
	if err != nil {
		t.Fatalf("读取完成历史: %v", err)
	}
	completed, found := findSubscriptionByID(history, firstID)
	if !found || completed.InternalState != subscription.Completed || completed.CompletedAt == nil || completed.V2State != "" {
		t.Fatalf("完成历史记录无效: found=%v item=%#v", found, completed)
	}

	secondID := createSubscriptionIntegrationFixture(t, store, "completion-owner", "completion-media")
	if secondID == firstID {
		t.Fatalf("重新订阅复用了完成历史 id=%d", firstID)
	}
	active, err = store.ListSubscriptions(t.Context(), "completion-owner", false)
	if err != nil {
		t.Fatalf("读取重新订阅后的活动列表: %v", err)
	}
	assertSubscriptionIDs(t, active, []int64{secondID})
}

func TestSubscriptionStorePostgresCompleteRequiresIndexedAndOwner(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	insertSubscriptionIntegrationUsers(t, store, "complete-owner", "complete-other")
	id := createSubscriptionIntegrationFixture(t, store, "complete-owner", "not-ready")

	if err := store.CompleteSubscription(t.Context(), id, "complete-other", false); !errors.Is(err, ErrSubscriptionNotFound) {
		t.Fatalf("非所有者完成错误 = %v, want ErrSubscriptionNotFound", err)
	}
	if err := store.CompleteSubscription(t.Context(), id, "complete-owner", false); !errors.Is(err, ErrSubscriptionNotReady) {
		t.Fatalf("未入库订阅完成错误 = %v, want ErrSubscriptionNotReady", err)
	}
}

func TestSubscriptionStorePostgresHistoryPaginationAndDeleteBoundaries(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	insertSubscriptionIntegrationUsers(t, store, "history-owner", "history-other")
	base := time.Date(2026, time.August, 11, 8, 0, 0, 0, time.UTC)
	oldMovieID := createCompletedHistoryFixture(t, store, "history-owner", "电影", "movie-old", base)
	newMovieID := createCompletedHistoryFixture(t, store, "history-owner", "电影", "movie-new", base.Add(time.Minute))
	_ = createCompletedHistoryFixture(t, store, "history-owner", "电视剧", "tv-owner", base.Add(2*time.Minute))
	otherMovieID := createCompletedHistoryFixture(t, store, "history-other", "电影", "movie-other", base.Add(3*time.Minute))
	activeID := createSubscriptionIntegrationFixture(t, store, "history-owner", "active-boundary")

	firstPage, err := store.ListCompletedSubscriptionsByType(t.Context(), "history-owner", false, "电影", 1, 1)
	if err != nil {
		t.Fatalf("读取 owner 历史第一页: %v", err)
	}
	assertSubscriptionIDs(t, firstPage, []int64{newMovieID})
	secondPage, err := store.ListCompletedSubscriptionsByType(t.Context(), "history-owner", false, "电影", 2, 1)
	if err != nil {
		t.Fatalf("读取 owner 历史第二页: %v", err)
	}
	assertSubscriptionIDs(t, secondPage, []int64{oldMovieID})
	global, err := store.ListCompletedSubscriptionsByType(t.Context(), "admin", true, "电影", 1, 3)
	if err != nil {
		t.Fatalf("读取管理员电影历史: %v", err)
	}
	assertSubscriptionIDs(t, global, []int64{otherMovieID, newMovieID, oldMovieID})

	if deleted, err := store.DeleteCompletedSubscription(t.Context(), activeID, "history-owner", false); err != nil || deleted {
		t.Fatalf("历史删除越过活动边界: deleted=%v err=%v", deleted, err)
	}
	if deleted, err := store.DeleteSubscription(t.Context(), newMovieID, "history-owner", false); err != nil || deleted {
		t.Fatalf("活动删除越过历史边界: deleted=%v err=%v", deleted, err)
	}
	if deleted, err := store.DeleteCompletedSubscription(t.Context(), otherMovieID, "history-owner", false); err != nil || deleted {
		t.Fatalf("普通用户删除了他人历史: deleted=%v err=%v", deleted, err)
	}
	if deleted, err := store.DeleteCompletedSubscription(t.Context(), newMovieID, "history-owner", false); err != nil || !deleted {
		t.Fatalf("删除 owner 历史失败: deleted=%v err=%v", deleted, err)
	}
}

func TestSubscriptionStorePostgresConcurrentCreateIsIdempotent(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	insertSubscriptionIntegrationUsers(t, store, "concurrent-subscription-owner")

	const workers = 32
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	start := make(chan struct{})
	ids := make(chan int64, workers)
	errs := make(chan error, workers)
	var group sync.WaitGroup
	for range workers {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			id, err := store.CreateSubscription(ctx, NewSubscription{
				Username:    "concurrent-subscription-owner",
				Name:        "Concurrent Fixture",
				MediaType:   "movie",
				Year:        "2026",
				MediaSource: "tmdb",
				MediaID:     "concurrent-identity",
				State:       subscription.PendingSearch,
				Config:      map[string]any{"fixture": true},
			})
			if err != nil {
				errs <- err
				return
			}
			ids <- id
		}()
	}
	close(start)
	group.Wait()
	close(ids)
	close(errs)
	for err := range errs {
		t.Fatalf("并发创建订阅: %v", err)
	}
	var stableID int64
	for id := range ids {
		if stableID == 0 {
			stableID = id
		}
		if id != stableID {
			t.Fatalf("并发幂等返回多个 ID: first=%d got=%d", stableID, id)
		}
	}
	if stableID == 0 {
		t.Fatal("并发创建未返回订阅 ID")
	}
	items, err := store.ListSubscriptions(t.Context(), "concurrent-subscription-owner", false)
	if err != nil {
		t.Fatalf("读取并发创建结果: %v", err)
	}
	assertSubscriptionIDs(t, items, []int64{stableID})
}

func createCompletedHistoryFixture(
	t *testing.T,
	store *Store,
	username string,
	mediaType string,
	mediaID string,
	completedAt time.Time,
) int64 {
	t.Helper()
	id, err := store.CreateSubscription(t.Context(), NewSubscription{
		Username: username, Name: mediaID, MediaType: mediaType, Year: "2026",
		MediaSource: "themoviedb", MediaID: mediaID, State: subscription.PendingSearch,
		Config: map[string]any{"fixture": true},
	})
	if err != nil {
		t.Fatalf("创建历史 fixture: %v", err)
	}
	if _, err := store.db.Pool.Exec(t.Context(), `UPDATE subscriptions SET internal_state=$1, v2_state=$2 WHERE id=$3`, subscription.Indexed, "R", id); err != nil {
		t.Fatalf("准备历史 fixture: %v", err)
	}
	if err := store.CompleteSubscription(t.Context(), id, username, false); err != nil {
		t.Fatalf("完成历史 fixture: %v", err)
	}
	if _, err := store.db.Pool.Exec(t.Context(), `UPDATE subscriptions SET completed_at=$1, updated_at=$1 WHERE id=$2`, completedAt, id); err != nil {
		t.Fatalf("设置历史完成时间: %v", err)
	}
	return id
}

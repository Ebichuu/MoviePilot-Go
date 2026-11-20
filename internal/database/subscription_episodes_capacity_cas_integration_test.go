//go:build integration

package database

import (
	"errors"
	"testing"

	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

func TestSubscriptionEpisodesPostgresCapacityIsAtomicAndPreservesProgress(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	insertSubscriptionIntegrationUsers(t, store, "episode-capacity-owner")
	subscriptionID := createSubscriptionIntegrationFixture(t, store, "episode-capacity-owner", "episode-capacity")

	episodes := make([]subscription.Episode, 0, maxSubscriptionEpisodes)
	for episode := 1; episode <= maxSubscriptionEpisodeNumber; episode++ {
		episodes = append(episodes, subscription.Episode{Season: 0, Episode: episode})
	}
	for episode := 1; episode < maxSubscriptionEpisodeNumber; episode++ {
		episodes = append(episodes, subscription.Episode{Season: 1, Episode: episode})
	}
	if len(episodes) != maxSubscriptionEpisodes-1 {
		t.Fatalf("容量 fixture 集数 = %d, want %d", len(episodes), maxSubscriptionEpisodes-1)
	}
	if err := store.UpsertSubscriptionEpisodes(t.Context(), "episode-capacity-owner", subscriptionID, episodes); err != nil {
		t.Fatalf("初始化容量边界前逐集: %v", err)
	}

	progressed := episodes[0]
	if err := store.CompareAndSwapSubscriptionEpisodeState(
		t.Context(), "episode-capacity-owner", subscriptionID, progressed,
		SubscriptionEpisodeMissing, SubscriptionEpisodeAcquired,
	); err != nil {
		t.Fatalf("推进容量 fixture 首集: %v", err)
	}

	boundary := subscription.Episode{Season: 1, Episode: maxSubscriptionEpisodeNumber}
	overflow := subscription.Episode{Season: 2, Episode: 1}
	if err := store.UpsertSubscriptionEpisodes(
		t.Context(), "episode-capacity-owner", subscriptionID,
		[]subscription.Episode{boundary, overflow},
	); !errors.Is(err, ErrInvalidSubscriptionEpisode) {
		t.Fatalf("跨越容量上限的批次 error = %v, want ErrInvalidSubscriptionEpisode", err)
	}

	summary, err := store.SummarizeSubscriptionEpisodes(t.Context(), "episode-capacity-owner", subscriptionID)
	if err != nil {
		t.Fatalf("汇总被拒绝的超限批次: %v", err)
	}
	if summary.Total != maxSubscriptionEpisodes-1 || summary.Missing != maxSubscriptionEpisodes-2 || summary.Acquired != 1 {
		t.Fatalf("超限批次发生部分写入或回退进度: %#v", summary)
	}

	if err := store.UpsertSubscriptionEpisodes(
		t.Context(), "episode-capacity-owner", subscriptionID,
		[]subscription.Episode{boundary},
	); err != nil {
		t.Fatalf("补齐第 %d 集: %v", maxSubscriptionEpisodes, err)
	}
	allEpisodes := append(append([]subscription.Episode(nil), episodes...), boundary)
	if err := store.UpsertSubscriptionEpisodes(t.Context(), "episode-capacity-owner", subscriptionID, allEpisodes); err != nil {
		t.Fatalf("容量边界完整重放: %v", err)
	}

	items, err := store.ListSubscriptionEpisodes(
		t.Context(), "episode-capacity-owner", subscriptionID,
		[]SubscriptionEpisodeState{SubscriptionEpisodeMissing, SubscriptionEpisodeAcquired},
		maxSubscriptionEpisodes,
	)
	if err != nil {
		t.Fatalf("读取容量边界逐集: %v", err)
	}
	if len(items) != maxSubscriptionEpisodes {
		t.Fatalf("容量边界逐集数 = %d, want %d", len(items), maxSubscriptionEpisodes)
	}
	if items[0].Season != progressed.Season || items[0].Episode != progressed.Episode || items[0].State != SubscriptionEpisodeAcquired {
		t.Fatalf("完整重放回退已有进度: first=%#v", items[0])
	}
	for _, item := range items {
		if item.Season == overflow.Season && item.Episode == overflow.Episode {
			t.Fatalf("超限批次发生部分写入: %#v", item)
		}
	}
}

func TestSubscriptionEpisodesPostgresCASClassifiesFailuresAndClearsIntent(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	insertSubscriptionIntegrationUsers(t, store, "episode-cas-contract-owner", "episode-cas-contract-other")
	subscriptionID := createSubscriptionIntegrationFixture(t, store, "episode-cas-contract-owner", "episode-cas-contract")
	episode := subscription.Episode{Season: 3, Episode: 7}
	if err := store.UpsertSubscriptionEpisodes(
		t.Context(), "episode-cas-contract-owner", subscriptionID, []subscription.Episode{episode},
	); err != nil {
		t.Fatalf("初始化 CAS 契约逐集: %v", err)
	}

	intentID := createBoundSubscriptionEpisodeIntent(
		t, store, "episode-cas-contract-owner", subscriptionID, "episode-cas-contract-intent",
	)
	if err := store.BindSubscriptionEpisodes(
		t.Context(), "episode-cas-contract-owner", subscriptionID, intentID, []subscription.Episode{episode},
	); err != nil {
		t.Fatalf("绑定 CAS 契约逐集: %v", err)
	}

	if err := store.CompareAndSwapSubscriptionEpisodeState(
		t.Context(), "episode-cas-contract-owner", subscriptionID, episode,
		SubscriptionEpisodeMissing, SubscriptionEpisodeAcquired,
	); !errors.Is(err, ErrSubscriptionEpisodeStateConflict) {
		t.Fatalf("过期状态 CAS error = %v, want ErrSubscriptionEpisodeStateConflict", err)
	}

	notFoundCases := []struct {
		name           string
		username       string
		subscriptionID int64
		episode        subscription.Episode
	}{
		{
			name:           "错误 owner",
			username:       "episode-cas-contract-other",
			subscriptionID: subscriptionID,
			episode:        episode,
		},
		{
			name:           "不存在坐标",
			username:       "episode-cas-contract-owner",
			subscriptionID: subscriptionID,
			episode:        subscription.Episode{Season: 3, Episode: 8},
		},
	}
	for _, tt := range notFoundCases {
		t.Run(tt.name, func(t *testing.T) {
			if err := store.CompareAndSwapSubscriptionEpisodeState(
				t.Context(), tt.username, tt.subscriptionID, tt.episode,
				SubscriptionEpisodeAcquired, SubscriptionEpisodeDownloading,
			); !errors.Is(err, ErrSubscriptionEpisodeNotFound) {
				t.Fatalf("CAS error = %v, want ErrSubscriptionEpisodeNotFound", err)
			}
		})
	}

	if err := store.CompareAndSwapSubscriptionEpisodeState(
		t.Context(), "episode-cas-contract-owner", subscriptionID, episode,
		SubscriptionEpisodeAcquired, SubscriptionEpisodeMissing,
	); err != nil {
		t.Fatalf("回退逐集到 missing: %v", err)
	}
	items, err := store.ListSubscriptionEpisodes(
		t.Context(), "episode-cas-contract-owner", subscriptionID,
		[]SubscriptionEpisodeState{SubscriptionEpisodeMissing}, 1,
	)
	if err != nil {
		t.Fatalf("读取回退后的逐集: %v", err)
	}
	if len(items) != 1 || items[0].DownloadIntentID != nil {
		t.Fatalf("回退 missing 未清空下载意图: %#v", items)
	}

	completedSubscriptionID := createSubscriptionIntegrationFixture(
		t, store, "episode-cas-contract-owner", "episode-cas-contract-completed",
	)
	if err := store.UpsertSubscriptionEpisodes(
		t.Context(), "episode-cas-contract-owner", completedSubscriptionID, []subscription.Episode{episode},
	); err != nil {
		t.Fatalf("初始化已完成订阅逐集: %v", err)
	}
	if _, err := store.db.Pool.Exec(
		t.Context(), `UPDATE subscriptions
			SET internal_state=$1, v2_state='', completed_at=now(), updated_at=now()
			WHERE id=$2`, subscription.Completed, completedSubscriptionID,
	); err != nil {
		t.Fatalf("标记订阅已完成: %v", err)
	}
	if err := store.CompareAndSwapSubscriptionEpisodeState(
		t.Context(), "episode-cas-contract-owner", completedSubscriptionID, episode,
		SubscriptionEpisodeMissing, SubscriptionEpisodeAcquired,
	); !errors.Is(err, ErrSubscriptionEpisodeNotFound) {
		t.Fatalf("已完成订阅 CAS error = %v, want ErrSubscriptionEpisodeNotFound", err)
	}
}

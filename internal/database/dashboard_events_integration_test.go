//go:build integration

package database

import (
	"errors"
	"testing"
	"time"

	"github.com/moviepilot-go/moviepilot-go/internal/mediaserver"
)

// TestEventStatisticsIntegration 覆盖事件统计的累加、跨用户汇总与入参校验。
func TestEventStatisticsIntegration(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	ctx := t.Context()
	day := time.Date(2026, 11, 18, 15, 0, 0, 0, time.UTC)

	for range 2 {
		if err := store.RecordEventStatistic(ctx, day, "download", "owner-a"); err != nil {
			t.Fatalf("RecordEventStatistic() error = %v", err)
		}
	}
	if err := store.RecordEventStatistic(ctx, day, "download", "owner-b"); err != nil {
		t.Fatalf("RecordEventStatistic(owner-b) error = %v", err)
	}
	if err := store.RecordEventStatistic(ctx, day.AddDate(0, 0, -30), "organize", ""); err != nil {
		t.Fatalf("RecordEventStatistic(历史) error = %v", err)
	}

	stats, err := store.ListEventStatistics(ctx, day)
	if err != nil {
		t.Fatalf("ListEventStatistics() error = %v", err)
	}
	if len(stats) != 1 || stats[0].EventType != "download" || stats[0].Count != 3 {
		t.Fatalf("ListEventStatistics() = %#v", stats)
	}
	// day 列是 date，读回只有日期部分。
	if want := time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC); !stats[0].Day.Equal(want) {
		t.Fatalf("统计日期 = %v, want %v", stats[0].Day, want)
	}

	all, err := store.ListEventStatistics(ctx, day.AddDate(0, 0, -30))
	if err != nil {
		t.Fatalf("ListEventStatistics(全量) error = %v", err)
	}
	if len(all) != 2 || all[0].EventType != "download" || all[1].EventType != "organize" {
		t.Fatalf("全量统计 = %#v", all)
	}

	if err := store.RecordEventStatistic(ctx, time.Time{}, "download", "owner"); !errors.Is(err, ErrInvalidEventStatistic) {
		t.Fatalf("零值日期 error = %v", err)
	}
	if err := store.RecordEventStatistic(ctx, day, "  ", "owner"); !errors.Is(err, ErrInvalidEventStatistic) {
		t.Fatalf("空事件类型 error = %v", err)
	}
	longType := make([]byte, 65)
	for index := range longType {
		longType[index] = 'a'
	}
	if err := store.RecordEventStatistic(ctx, day, string(longType), "owner"); !errors.Is(err, ErrInvalidEventStatistic) {
		t.Fatalf("超长事件类型 error = %v", err)
	}
	if _, err := store.ListEventStatistics(ctx, time.Time{}); !errors.Is(err, ErrInvalidEventStatistic) {
		t.Fatalf("零值 since error = %v", err)
	}
}

// TestMediaServerItemCacheIntegration 覆盖媒体条目缓存的 upsert、查找、过期清理
// 与仪表盘计数（剧集数按 seasoninfo 累加）。
func TestMediaServerItemCacheIntegration(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	ctx := t.Context()
	insertSubscriptionIntegrationUsers(t, store, "dashboard-owner")

	movie := mediaserver.Item{
		Server: "emby", Library: "movies", ItemID: "m-1",
		ItemType: mediaserver.ItemTypeMovie, Title: "电影一", Year: "2026",
		TMDBID: 101, LstModDate: "sync-1",
	}
	tv := mediaserver.Item{
		Server: "emby", Library: "tv", ItemID: "t-1",
		ItemType: mediaserver.ItemTypeTV, Title: "剧集一", Year: "2025",
		TMDBID: 202, SeasonInfo: map[int][]int{1: {1, 2, 3}, 2: {1}}, LstModDate: "sync-1",
	}
	for _, item := range []mediaserver.Item{movie, tv} {
		if err := store.UpsertMediaServerItem(ctx, item); err != nil {
			t.Fatalf("UpsertMediaServerItem(%s) error = %v", item.ItemID, err)
		}
	}
	if err := store.UpsertMediaServerItem(ctx, mediaserver.Item{ItemID: "bad", LstModDate: "x"}); !errors.Is(err, mediaserver.ErrInvalidItem) {
		t.Fatalf("非法条目 error = %v, want ErrInvalidItem", err)
	}
	// 同 server + item_id 覆盖更新，不产生新行。
	movie.Title = "电影一（改名）"
	if err := store.UpsertMediaServerItem(ctx, movie); err != nil {
		t.Fatalf("覆盖 UpsertMediaServerItem() error = %v", err)
	}

	counts, err := store.DashboardMediaCounts(ctx)
	if err != nil {
		t.Fatalf("DashboardMediaCounts() error = %v", err)
	}
	if counts.MovieCount != 1 || counts.TVCount != 1 || counts.EpisodeCount != 4 || counts.UserCount != 1 {
		t.Fatalf("DashboardMediaCounts() = %#v", counts)
	}
	if counts.MovieCountMonth != 0 || counts.TVCountMonth != 0 {
		t.Fatalf("本月新增应为 0: %#v", counts)
	}

	itemID, found, err := store.FindMediaServerItemID(ctx, mediaserver.Query{TMDBID: 101, ItemType: mediaserver.ItemTypeMovie})
	if err != nil || !found || itemID != "m-1" {
		t.Fatalf("按 TMDB 查找 = (%q, %v, %v)", itemID, found, err)
	}
	itemID, found, err = store.FindMediaServerItemID(ctx, mediaserver.Query{
		Title: "剧集一", ItemType: mediaserver.ItemTypeTV, Year: "2025",
	})
	if err != nil || !found || itemID != "t-1" {
		t.Fatalf("按标题查找 = (%q, %v, %v)", itemID, found, err)
	}
	if _, found, err := store.FindMediaServerItemID(ctx, mediaserver.Query{Title: "不存在"}); err != nil || found {
		t.Fatalf("缺失条目 = (found=%v, err=%v)", found, err)
	}
	if _, found, err := store.FindMediaServerItemID(ctx, mediaserver.Query{}); err != nil || found {
		t.Fatalf("空查询 = (found=%v, err=%v)", found, err)
	}

	// 换一个同步批次后，旧 lst_mod_date 的行属于上一轮，应被清掉。
	if err := store.UpsertMediaServerItem(ctx, mediaserver.Item{
		Server: "emby", ItemID: "m-2", ItemType: mediaserver.ItemTypeMovie,
		Title: "电影二", LstModDate: "sync-2",
	}); err != nil {
		t.Fatalf("UpsertMediaServerItem(m-2) error = %v", err)
	}
	if err := store.UpsertMediaServerItem(ctx, mediaserver.Item{
		Server: "jellyfin", ItemID: "j-1", ItemType: mediaserver.ItemTypeMovie,
		Title: "电影三", LstModDate: "sync-2",
	}); err != nil {
		t.Fatalf("UpsertMediaServerItem(j-1) error = %v", err)
	}
	removed, err := store.DeleteStaleMediaServerItems(ctx, "emby", "sync-2")
	if err != nil || removed != 2 {
		t.Fatalf("DeleteStaleMediaServerItems() = (%d, %v), want 2", removed, err)
	}
	if _, found, err := store.FindMediaServerItemID(ctx, mediaserver.Query{TMDBID: 202, ItemType: mediaserver.ItemTypeTV}); err != nil || found {
		t.Fatalf("过期条目仍可查 = (found=%v, err=%v)", found, err)
	}

	// 只保留 emby：jellyfin 的行被清掉。
	removed, err = store.DeleteExcludedMediaServerItems(ctx, []string{"emby"})
	if err != nil || removed != 1 {
		t.Fatalf("DeleteExcludedMediaServerItems(emby) = (%d, %v), want 1", removed, err)
	}
	// 反向再切一次：保留 jellyfin 时清掉 emby 的剩余行。
	removed, err = store.DeleteExcludedMediaServerItems(ctx, []string{"jellyfin"})
	if err != nil || removed != 1 {
		t.Fatalf("DeleteExcludedMediaServerItems(jellyfin) = (%d, %v), want 1", removed, err)
	}
	if err := store.UpsertMediaServerItem(ctx, mediaserver.Item{
		Server: "emby", ItemID: "m-3", ItemType: mediaserver.ItemTypeMovie,
		Title: "电影三", LstModDate: "sync-3",
	}); err != nil {
		t.Fatalf("UpsertMediaServerItem(m-3) error = %v", err)
	}
	if removed, err = store.DeleteExcludedMediaServerItems(ctx, nil); err != nil || removed != 1 {
		t.Fatalf("空列表清理 = (%d, %v), want 1", removed, err)
	}
	counts, err = store.DashboardMediaCounts(ctx)
	if err != nil || counts.MovieCount != 0 || counts.TVCount != 0 || counts.EpisodeCount != 0 {
		t.Fatalf("清空后 DashboardMediaCounts() = (%#v, %v)", counts, err)
	}
	if removed, err := store.DeleteStaleMediaServerItems(ctx, "", "sync-2"); !errors.Is(err, mediaserver.ErrInvalidItem) || removed != 0 {
		t.Fatalf("空 server 清理 = (%d, %v)", removed, err)
	}
}

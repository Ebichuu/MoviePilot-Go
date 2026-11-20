package database

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestSubscriptionV2HistoryBranches 覆盖订阅历史投影里「未完成不发日期」
// 「电视剧按目标数补齐 completed_episode」两条分支。
func TestSubscriptionV2HistoryBranches(t *testing.T) {
	created := time.Date(2026, 11, 20, 8, 0, 0, 0, time.UTC)
	updated := created.Add(time.Minute)

	t.Run("未完成", func(t *testing.T) {
		history := Subscription{
			ID: 1, Username: "alice", Name: "示例电影", MediaType: "电影",
			V2State: "P", CreatedAt: created, UpdatedAt: updated,
		}.V2History()
		if history["date"] != nil {
			t.Fatalf("未完成订阅的 date 应为 nil，实际 %v", history["date"])
		}
	})

	t.Run("已完成", func(t *testing.T) {
		completedAt := updated.Add(time.Hour)
		history := Subscription{
			ID: 2, Username: "alice", Name: "示例电影", MediaType: "电影",
			V2State: "C", CreatedAt: created, UpdatedAt: updated, CompletedAt: &completedAt,
		}.V2History()
		if history["date"] == nil {
			t.Fatal("已完成订阅应带完成时间")
		}
	})

	t.Run("电视剧补齐完成集数", func(t *testing.T) {
		history := Subscription{
			ID: 3, Username: "alice", Name: "示例剧集", MediaType: "电视剧",
			V2State: "P", CreatedAt: created, UpdatedAt: updated,
			Config: map[string]any{"total_episode": 12, "start_episode": 1, "lack_episode": 3},
		}.V2History()
		if got := history["completed_episode"]; got != 9 {
			t.Fatalf("completed_episode = %v，期望 9", got)
		}
		if history["total_episode"] != 0 || history["lack_episode"] != 0 {
			t.Fatalf("电视剧应清零总集/缺集: total=%v lack=%v", history["total_episode"], history["lack_episode"])
		}
	})
}

// TestListCompletedSubscriptionsByTypeRejectsInvalidPaging 覆盖分页参数校验
// 在触库之前失败的路径。
func TestListCompletedSubscriptionsByTypeRejectsInvalidPaging(t *testing.T) {
	store := &Store{}
	ctx := context.Background()

	cases := []struct {
		name      string
		username  string
		admin     bool
		mediaType string
		page      int
		count     int
	}{
		{name: "缺少媒体类型", username: "alice", mediaType: "", page: 1, count: 10},
		{name: "页码为零", username: "alice", mediaType: "电影", page: 0, count: 10},
		{name: "每页为零", username: "alice", mediaType: "电影", page: 1, count: 0},
		{name: "非管理员缺少用户名", username: "  ", mediaType: "电影", page: 1, count: 10},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := store.ListCompletedSubscriptionsByType(ctx, tc.username, tc.admin, tc.mediaType, tc.page, tc.count); !errors.Is(err, ErrInvalidSubscriptionHistoryPage) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

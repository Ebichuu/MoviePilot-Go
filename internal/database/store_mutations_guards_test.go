package database

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

// 本文件覆盖订阅写路径的参数与连接错误分支：
// 创建订阅的规范化/编码/事务失败、状态码解析失败、列表查询失败、
// 完成与删除订阅的事务/执行失败。

func storeMutationUnitStore(t *testing.T) *Store {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), "postgres://invalid:invalid@127.0.0.1:1/none?sslmode=disable")
	if err != nil {
		t.Fatalf("创建不可用连接池: %v", err)
	}
	t.Cleanup(pool.Close)
	return &Store{db: &DB{Pool: pool}}
}

func validNewSubscription() NewSubscription {
	season := 1
	return NewSubscription{
		Username:  "alice",
		Name:      "示例剧集",
		MediaType: "tv",
		Year:      "2025",
		Season:    &season,
		State:     subscription.PendingSearch,
		Config:    map[string]any{"keyword": "示例"},
	}
}

func TestCreateSubscriptionRejectsInvalidInput(t *testing.T) {
	ctx := context.Background()

	t.Run("空输入", func(t *testing.T) {
		if _, err := storeMutationUnitStore(t).CreateSubscription(ctx, NewSubscription{}); err == nil {
			t.Fatal("空输入应返回错误")
		}
	})

	t.Run("配置无法编码", func(t *testing.T) {
		input := validNewSubscription()
		input.Config = map[string]any{"keyword": make(chan int)}
		_, err := storeMutationUnitStore(t).CreateSubscription(ctx, input)
		if err == nil || !strings.Contains(err.Error(), "编码订阅配置") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("连接不可用", func(t *testing.T) {
		_, err := storeMutationUnitStore(t).CreateSubscription(ctx, validNewSubscription())
		if err == nil || !strings.Contains(err.Error(), "开始创建订阅事务") {
			t.Fatalf("error = %v", err)
		}
	})
}

func TestSubscriptionMutationPathsReportUnavailablePool(t *testing.T) {
	store := storeMutationUnitStore(t)
	ctx := context.Background()

	if _, err := store.ListSubscriptions(ctx, "alice", false); err == nil || !strings.Contains(err.Error(), "查询订阅") {
		t.Fatalf("ListSubscriptions error = %v", err)
	}
	if _, err := store.ListCompletedSubscriptions(ctx, "alice", true); err == nil || !strings.Contains(err.Error(), "查询订阅") {
		t.Fatalf("ListCompletedSubscriptions error = %v", err)
	}
	if err := store.SetSubscriptionV2State(ctx, 1, "alice", false, "P"); err == nil || !strings.Contains(err.Error(), "更新订阅状态") {
		t.Fatalf("SetSubscriptionV2State error = %v", err)
	}
	if err := store.CompleteSubscription(ctx, 1, "alice", false); err == nil || !strings.Contains(err.Error(), "开始完成订阅事务") {
		t.Fatalf("CompleteSubscription error = %v", err)
	}
	if _, err := store.DeleteSubscription(ctx, 1, "alice", false); err == nil || !strings.Contains(err.Error(), "删除订阅") {
		t.Fatalf("DeleteSubscription error = %v", err)
	}
	if _, err := store.DeleteCompletedSubscription(ctx, 1, "alice", true); err == nil || !strings.Contains(err.Error(), "删除订阅历史") {
		t.Fatalf("DeleteCompletedSubscription error = %v", err)
	}
}

func TestSetSubscriptionV2StateRejectsInvalidState(t *testing.T) {
	store := storeMutationUnitStore(t)
	if err := store.SetSubscriptionV2State(context.Background(), 1, "alice", false, "不存在的状态"); !errors.Is(err, ErrInvalidSubscriptionState) {
		t.Fatalf("error = %v, want ErrInvalidSubscriptionState", err)
	}
}

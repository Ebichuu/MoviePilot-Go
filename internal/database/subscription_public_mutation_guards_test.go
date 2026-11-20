package database

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// 本文件覆盖公开变更存储层的闸门与非法输入：
// 可见性校验、缺失进度越界、配置无法编码、重置与替换冲突、重置入参校验。

func publicMutationUnitStore(t *testing.T) *Store {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), "postgres://invalid:invalid@127.0.0.1:1/none?sslmode=disable")
	if err != nil {
		t.Fatalf("创建不可用连接池: %v", err)
	}
	t.Cleanup(pool.Close)
	return &Store{db: &DB{Pool: pool}}
}

func validPublicMutationInput() SubscriptionPublicMutation {
	return SubscriptionPublicMutation{
		Username:          "alice",
		SubscriptionID:    5,
		ExpectedUpdatedAt: time.Now().UTC(),
		Config:            map[string]any{"keyword": "测试"},
	}
}

func TestGetAccessibleActiveSubscriptionRejectsBadAccess(t *testing.T) {
	store := publicMutationUnitStore(t)
	ctx := context.Background()

	cases := []struct {
		name     string
		username string
		id       int64
		admin    bool
	}{
		{name: "缺订阅 ID", username: "alice", id: 0},
		{name: "普通用户缺用户名", username: "  ", id: 5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := store.GetAccessibleActiveSubscription(ctx, tc.username, tc.id, tc.admin)
			if !errors.Is(err, ErrSubscriptionNotFound) {
				t.Fatalf("error = %v", err)
			}
		})
	}

	t.Run("连接不可用", func(t *testing.T) {
		_, err := store.GetAccessibleActiveSubscription(ctx, "alice", 5, false)
		if !errors.Is(err, ErrSubscriptionPublicMutationPersistence) {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("nil 上下文", func(t *testing.T) {
		_, err := store.GetAccessibleActiveSubscription(nil, "alice", 5, true)
		if !errors.Is(err, ErrSubscriptionNotFound) {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("store 未就绪", func(t *testing.T) {
		_, err := (&Store{}).GetAccessibleActiveSubscription(ctx, "alice", 5, true)
		if !errors.Is(err, ErrSubscriptionNotFound) {
			t.Fatalf("error = %v", err)
		}
	})
}

func TestApplySubscriptionPublicMutationRejectsInvalidInput(t *testing.T) {
	store := publicMutationUnitStore(t)
	ctx := context.Background()

	t.Run("空输入", func(t *testing.T) {
		if _, err := store.ApplySubscriptionPublicMutation(ctx, SubscriptionPublicMutation{}); !errors.Is(err, ErrInvalidSubscriptionPublicMutation) {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("缺失进度为负", func(t *testing.T) {
		input := validPublicMutationInput()
		input.Progress = &SubscriptionPublicProgress{LackEpisode: -1}
		if _, err := store.ApplySubscriptionPublicMutation(ctx, input); !errors.Is(err, ErrInvalidSubscriptionPublicMutation) {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("缺失进度超上限", func(t *testing.T) {
		input := validPublicMutationInput()
		input.Progress = &SubscriptionPublicProgress{LackEpisode: 10001}
		if _, err := store.ApplySubscriptionPublicMutation(ctx, input); !errors.Is(err, ErrInvalidSubscriptionPublicMutation) {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("重置与替换冲突", func(t *testing.T) {
		input := validPublicMutationInput()
		input.Reset = true
		input.ReplacePublic = true
		if _, err := store.ApplySubscriptionPublicMutation(ctx, input); !errors.Is(err, ErrInvalidSubscriptionPublicMutation) {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("配置无法编码", func(t *testing.T) {
		input := validPublicMutationInput()
		input.Config = map[string]any{"keyword": make(chan int)}
		if _, err := store.ApplySubscriptionPublicMutation(ctx, input); !errors.Is(err, ErrInvalidSubscriptionPublicMutation) {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("连接不可用", func(t *testing.T) {
		if _, err := store.ApplySubscriptionPublicMutation(ctx, validPublicMutationInput()); !errors.Is(err, ErrSubscriptionPublicMutationPersistence) {
			t.Fatalf("error = %v", err)
		}
	})
}

func TestResetActiveSubscriptionRejectsInvalidObservation(t *testing.T) {
	store := publicMutationUnitStore(t)
	cases := []struct {
		name     string
		observed Subscription
	}{
		{name: "缺订阅 ID", observed: Subscription{Username: "alice", UpdatedAt: time.Now().UTC()}},
		{name: "缺用户名", observed: Subscription{ID: 5, UpdatedAt: time.Now().UTC()}},
		{name: "缺版本时间", observed: Subscription{ID: 5, Username: "alice"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := store.ResetActiveSubscription(context.Background(), tc.observed); !errors.Is(err, ErrInvalidSubscriptionPublicMutation) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

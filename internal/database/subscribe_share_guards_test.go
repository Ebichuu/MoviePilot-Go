package database

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// 本文件覆盖订阅分享存储层的未就绪与连接不可用闸门：
// 每个方法在 store 未初始化时返回的哨兵值，以及真实连不上数据库时的错误传播。

func shareGuardUnitStore(t *testing.T) *Store {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), "postgres://invalid:invalid@127.0.0.1:1/none?sslmode=disable")
	if err != nil {
		t.Fatalf("创建不可用连接池: %v", err)
	}
	t.Cleanup(pool.Close)
	return &Store{db: &DB{Pool: pool}}
}

func TestSubscribeShareStoreGuards(t *testing.T) {
	ctx := context.Background()
	notReady := &Store{}
	broken := shareGuardUnitStore(t)
	season := 1
	share := SubscribeShare{
		ShareKey: "share-key", Username: "alice", Title: "示例", MediaType: "tv",
		Year: "2026", Season: &season, MediaSource: "tmdb", MediaID: "42", Sites: []int{1, 2},
	}

	t.Run("CreateSubscribeShare", func(t *testing.T) {
		if _, err := notReady.CreateSubscribeShare(ctx, share); !errors.Is(err, ErrSubscribeShareNotFound) {
			t.Fatalf("未就绪 error = %v", err)
		}
		if _, err := broken.CreateSubscribeShare(ctx, share); err == nil {
			t.Fatal("连接不可用时应返回错误")
		}
	})

	t.Run("ListSubscribeShares", func(t *testing.T) {
		shares, err := notReady.ListSubscribeShares(ctx, "alice")
		if err != nil || len(shares) != 0 {
			t.Fatalf("未就绪结果 = %v（%v）", shares, err)
		}
		if _, err := broken.ListSubscribeShares(ctx, "alice"); err == nil {
			t.Fatal("连接不可用时应返回错误")
		}
	})

	t.Run("GetSubscribeShare", func(t *testing.T) {
		if _, err := notReady.GetSubscribeShare(ctx, 1); !errors.Is(err, ErrSubscribeShareNotFound) {
			t.Fatalf("未就绪 error = %v", err)
		}
		if _, err := broken.GetSubscribeShare(ctx, 1); !errors.Is(err, ErrSubscribeShareNotFound) {
			t.Fatalf("连接不可用 error = %v", err)
		}
	})

	t.Run("GetSubscribeShareByKey", func(t *testing.T) {
		if _, err := notReady.GetSubscribeShareByKey(ctx, "share-key"); !errors.Is(err, ErrSubscribeShareNotFound) {
			t.Fatalf("未就绪 error = %v", err)
		}
		if _, err := broken.GetSubscribeShareByKey(ctx, "share-key"); !errors.Is(err, ErrSubscribeShareNotFound) {
			t.Fatalf("连接不可用 error = %v", err)
		}
	})

	t.Run("DeleteSubscribeShare", func(t *testing.T) {
		removed, err := notReady.DeleteSubscribeShare(ctx, "alice", 1)
		if err != nil || removed {
			t.Fatalf("未就绪结果 = %v（%v）", removed, err)
		}
		if _, err := broken.DeleteSubscribeShare(ctx, "alice", 1); err == nil {
			t.Fatal("连接不可用时应返回错误")
		}
	})

	t.Run("FollowSubscribeShare", func(t *testing.T) {
		if _, err := notReady.FollowSubscribeShare(ctx, "alice", 1); !errors.Is(err, ErrSubscribeShareNotFound) {
			t.Fatalf("未就绪 error = %v", err)
		}
		if _, err := broken.FollowSubscribeShare(ctx, "alice", 1); !errors.Is(err, ErrSubscribeShareNotFound) {
			t.Fatalf("连接不可用 error = %v", err)
		}
	})

	t.Run("UnfollowSubscribeShare", func(t *testing.T) {
		removed, err := notReady.UnfollowSubscribeShare(ctx, "alice", 1)
		if err != nil || removed {
			t.Fatalf("未就绪结果 = %v（%v）", removed, err)
		}
		if _, err := broken.UnfollowSubscribeShare(ctx, "alice", 1); err == nil {
			t.Fatal("连接不可用时应返回错误")
		}
	})

	t.Run("ListSubscribeFollows", func(t *testing.T) {
		shares, err := notReady.ListSubscribeFollows(ctx, "alice")
		if err != nil || len(shares) != 0 {
			t.Fatalf("未就绪结果 = %v（%v）", shares, err)
		}
		if _, err := broken.ListSubscribeFollows(ctx, "alice"); err == nil {
			t.Fatal("连接不可用时应返回错误")
		}
	})
}

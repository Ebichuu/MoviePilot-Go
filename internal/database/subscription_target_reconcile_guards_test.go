package database

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

// 本文件覆盖订阅目标对账/校验/按目标读取逐集的参数闸门：
// 非法参数、非法逐集状态、重复状态去重、nil 上下文、未就绪 store 与连接不可用。

func targetReconcileUnitStore(t *testing.T) *Store {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), "postgres://invalid:invalid@127.0.0.1:1/none?sslmode=disable")
	if err != nil {
		t.Fatalf("创建不可用连接池: %v", err)
	}
	t.Cleanup(pool.Close)
	return &Store{db: &DB{Pool: pool}}
}

func TestListSubscriptionEpisodesForTargetRejectsInvalidArguments(t *testing.T) {
	store := targetReconcileUnitStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	states := []subscription.SubscriptionEpisodeState{subscription.SubscriptionEpisodeMissing}
	tooManyStates := []subscription.SubscriptionEpisodeState{
		subscription.SubscriptionEpisodeMissing, subscription.SubscriptionEpisodeAcquired,
		subscription.SubscriptionEpisodeDownloading, subscription.SubscriptionEpisodePendingOrganization,
		subscription.SubscriptionEpisodeOrganizing, subscription.SubscriptionEpisodePendingIndex,
		subscription.SubscriptionEpisodeIndexed, subscription.SubscriptionEpisodeMissing,
	}

	cases := []struct {
		name string
		call func() error
	}{
		{
			name: "缺用户名",
			call: func() error {
				_, err := store.ListSubscriptionEpisodesForTarget(ctx, "  ", 5, now, nil, states, 10)
				return err
			},
		},
		{
			name: "缺订阅 ID",
			call: func() error {
				_, err := store.ListSubscriptionEpisodesForTarget(ctx, "alice", 0, now, nil, states, 10)
				return err
			},
		},
		{
			name: "缺版本时间",
			call: func() error {
				_, err := store.ListSubscriptionEpisodesForTarget(ctx, "alice", 5, time.Time{}, nil, states, 10)
				return err
			},
		},
		{
			name: "缺状态集合",
			call: func() error {
				_, err := store.ListSubscriptionEpisodesForTarget(ctx, "alice", 5, now, nil, nil, 10)
				return err
			},
		},
		{
			name: "状态集合过大",
			call: func() error {
				_, err := store.ListSubscriptionEpisodesForTarget(ctx, "alice", 5, now, nil, tooManyStates, 10)
				return err
			},
		},
		{
			name: "批次越界",
			call: func() error {
				_, err := store.ListSubscriptionEpisodesForTarget(ctx, "alice", 5, now, nil, states, 0)
				return err
			},
		},
		{
			name: "非法逐集状态",
			call: func() error {
				_, err := store.ListSubscriptionEpisodesForTarget(ctx, "alice", 5, now, nil,
					[]subscription.SubscriptionEpisodeState{subscription.SubscriptionEpisodeState("bogus")}, 10)
				return err
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.call(); !errors.Is(err, ErrInvalidSubscriptionTarget) {
				t.Fatalf("error = %v, want ErrInvalidSubscriptionTarget", err)
			}
		})
	}
}

func TestListSubscriptionEpisodesForTargetGuards(t *testing.T) {
	now := time.Now().UTC()
	states := []subscription.SubscriptionEpisodeState{
		subscription.SubscriptionEpisodeMissing,
		subscription.SubscriptionEpisodeMissing, // 重复状态会被去重
	}

	t.Run("nil 上下文", func(t *testing.T) {
		_, err := targetReconcileUnitStore(t).ListSubscriptionEpisodesForTarget(nil, "alice", 5, now, nil, states, 10)
		if !errors.Is(err, ErrSubscriptionTargetPersistence) {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("store 未就绪", func(t *testing.T) {
		_, err := (&Store{}).ListSubscriptionEpisodesForTarget(context.Background(), "alice", 5, now, nil, states, 10)
		if !errors.Is(err, ErrSubscriptionTargetPersistence) {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("已取消上下文", func(t *testing.T) {
		canceled, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := targetReconcileUnitStore(t).ListSubscriptionEpisodesForTarget(canceled, "alice", 5, now, nil, states, 10)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("连接不可用", func(t *testing.T) {
		_, err := targetReconcileUnitStore(t).ListSubscriptionEpisodesForTarget(context.Background(), "alice", 5, now, nil, states, 10)
		if !errors.Is(err, ErrSubscriptionTargetPersistence) {
			t.Fatalf("error = %v", err)
		}
	})
}

func TestReconcileSubscriptionTargetRejectsInvalidInput(t *testing.T) {
	store := targetReconcileUnitStore(t)
	cases := []struct {
		name  string
		input SubscriptionTargetReconcile
	}{
		{name: "全空输入", input: SubscriptionTargetReconcile{}},
		{name: "缺订阅 ID", input: SubscriptionTargetReconcile{Username: strings.Repeat("a", 8), ExpectedUpdatedAt: time.Now().UTC()}},
		{name: "缺版本时间", input: SubscriptionTargetReconcile{Username: strings.Repeat("a", 8), SubscriptionID: 5}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := store.ReconcileSubscriptionTarget(context.Background(), tc.input); !errors.Is(err, ErrInvalidSubscriptionTarget) {
				t.Fatalf("error = %v, want ErrInvalidSubscriptionTarget", err)
			}
		})
	}
}

func TestVerifySubscriptionTargetVersionTxRejectsInvalidArguments(t *testing.T) {
	store := targetReconcileUnitStore(t)
	now := time.Now().UTC()
	cases := []struct {
		name string
		call func() error
	}{
		{
			name: "缺事务",
			call: func() error {
				return store.VerifySubscriptionTargetVersionTx(context.Background(), pgx.Tx(nil), "alice", 5, now)
			},
		},
		{
			name: "缺用户名",
			call: func() error {
				return store.VerifySubscriptionTargetVersionTx(context.Background(), pgx.Tx(nil), " ", 5, now)
			},
		},
		{
			name: "缺订阅 ID",
			call: func() error {
				return store.VerifySubscriptionTargetVersionTx(context.Background(), pgx.Tx(nil), "alice", 0, now)
			},
		},
		{
			name: "缺版本时间",
			call: func() error {
				return store.VerifySubscriptionTargetVersionTx(context.Background(), pgx.Tx(nil), "alice", 5, time.Time{})
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.call(); !errors.Is(err, ErrInvalidSubscriptionTarget) {
				t.Fatalf("error = %v, want ErrInvalidSubscriptionTarget", err)
			}
		})
	}
}

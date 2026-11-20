package database

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

// 本文件覆盖逐集状态存储层的参数与上下文闸门：
// 非法参数、nil 上下文、已取消上下文、未就绪 store，以及连接不可用时的持久化错误。

func episodeUnitStore(t *testing.T) *Store {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), "postgres://invalid:invalid@127.0.0.1:1/none?sslmode=disable")
	if err != nil {
		t.Fatalf("创建不可用连接池: %v", err)
	}
	t.Cleanup(pool.Close)
	return &Store{db: &DB{Pool: pool}}
}

func episodeUnitFixture() []subscription.Episode {
	return []subscription.Episode{{Season: 1, Episode: 1}}
}

type episodeGuardCase struct {
	name string
	call func(ctx context.Context, store *Store) error
	// txMethods 传 nil 事务，事务闸门先于上下文检查触发。
	txMethods bool
}

func episodeGuardCases() []episodeGuardCase {
	return []episodeGuardCase{
		{
			name: "UpsertSubscriptionEpisodes",
			call: func(ctx context.Context, store *Store) error {
				return store.UpsertSubscriptionEpisodes(ctx, "alice", 5, episodeUnitFixture())
			},
		},
		{
			name: "ListSubscriptionEpisodes",
			call: func(ctx context.Context, store *Store) error {
				_, err := store.ListSubscriptionEpisodes(ctx, "alice", 5,
					[]subscription.SubscriptionEpisodeState{subscription.SubscriptionEpisodeMissing}, 10)
				return err
			},
		},
		{
			name: "CompareAndSwapSubscriptionEpisodeState",
			call: func(ctx context.Context, store *Store) error {
				return store.CompareAndSwapSubscriptionEpisodeState(ctx, "alice", 5,
					subscription.Episode{Season: 1, Episode: 1},
					subscription.SubscriptionEpisodeMissing, subscription.SubscriptionEpisodeAcquired)
			},
		},
		{
			name: "CompareAndSwapSubscriptionEpisodeStateTx",
			call: func(ctx context.Context, store *Store) error {
				return store.CompareAndSwapSubscriptionEpisodeStateTx(ctx, nil, "alice", 5,
					subscription.Episode{Season: 1, Episode: 1},
					subscription.SubscriptionEpisodeMissing, subscription.SubscriptionEpisodeAcquired)
			},
			txMethods: true,
		},
		{
			name: "BindSubscriptionEpisodes",
			call: func(ctx context.Context, store *Store) error {
				return store.BindSubscriptionEpisodes(ctx, "alice", 5, uuid.New(), episodeUnitFixture())
			},
		},
		{
			name: "BindSubscriptionEpisodesTx",
			call: func(ctx context.Context, store *Store) error {
				return store.BindSubscriptionEpisodesTx(ctx, nil, "alice", 5, uuid.New(), episodeUnitFixture())
			},
			txMethods: true,
		},
		{
			name: "VerifySubscriptionEpisodesBoundToIntentTx",
			call: func(ctx context.Context, store *Store) error {
				return store.VerifySubscriptionEpisodesBoundToIntentTx(ctx, nil, "alice", 5, uuid.New(), episodeUnitFixture())
			},
			txMethods: true,
		},
		{
			name: "TransitionSubscriptionEpisodesByIntent",
			call: func(ctx context.Context, store *Store) error {
				_, err := store.TransitionSubscriptionEpisodesByIntent(ctx, "alice", uuid.New(),
					[]subscription.SubscriptionEpisodeState{subscription.SubscriptionEpisodeMissing},
					subscription.SubscriptionEpisodeAcquired)
				return err
			},
		},
		{
			name: "SummarizeSubscriptionEpisodes",
			call: func(ctx context.Context, store *Store) error {
				_, err := store.SummarizeSubscriptionEpisodes(ctx, "alice", 5)
				return err
			},
		},
	}
}

func TestSubscriptionEpisodeGuardsRejectBadContextAndStore(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	for _, tc := range episodeGuardCases() {
		t.Run(tc.name+"/nil 上下文", func(t *testing.T) {
			if err := tc.call(nil, episodeUnitStore(t)); !errors.Is(err, ErrSubscriptionEpisodePersistence) {
				t.Fatalf("nil 上下文 error = %v", err)
			}
		})
		t.Run(tc.name+"/已取消上下文", func(t *testing.T) {
			err := tc.call(canceled, episodeUnitStore(t))
			if tc.txMethods {
				if !errors.Is(err, ErrSubscriptionEpisodePersistence) {
					t.Fatalf("事务方法已取消上下文 error = %v", err)
				}
				return
			}
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("已取消上下文 error = %v", err)
			}
		})
		t.Run(tc.name+"/store 未就绪", func(t *testing.T) {
			if err := tc.call(context.Background(), &Store{}); !errors.Is(err, ErrSubscriptionEpisodePersistence) {
				t.Fatalf("store 未就绪 error = %v", err)
			}
		})
		t.Run(tc.name+"/连接不可用", func(t *testing.T) {
			if err := tc.call(context.Background(), episodeUnitStore(t)); !errors.Is(err, ErrSubscriptionEpisodePersistence) {
				t.Fatalf("连接不可用 error = %v", err)
			}
		})
	}
}

func TestSubscriptionEpisodeGuardsRejectInvalidArguments(t *testing.T) {
	store := episodeUnitStore(t)

	cases := []struct {
		name string
		call func() error
		want error
	}{
		{
			name: "初始化缺用户名",
			call: func() error {
				return store.UpsertSubscriptionEpisodes(context.Background(), "  ", 5, episodeUnitFixture())
			},
			want: ErrInvalidSubscriptionEpisode,
		},
		{
			name: "初始化缺订阅 ID",
			call: func() error {
				return store.UpsertSubscriptionEpisodes(context.Background(), "alice", 0, episodeUnitFixture())
			},
			want: ErrInvalidSubscriptionEpisode,
		},
		{
			name: "初始化空集",
			call: func() error {
				return store.UpsertSubscriptionEpisodes(context.Background(), "alice", 5, nil)
			},
			want: ErrInvalidSubscriptionEpisode,
		},
		{
			name: "读取批次越界",
			call: func() error {
				_, err := store.ListSubscriptionEpisodes(context.Background(), "alice", 5, nil, 0)
				return err
			},
			want: ErrInvalidSubscriptionEpisode,
		},
		{
			name: "切换状态非法",
			call: func() error {
				return store.CompareAndSwapSubscriptionEpisodeState(context.Background(), "alice", 5,
					subscription.Episode{Season: 1, Episode: 1},
					subscription.SubscriptionEpisodeState("bogus"), subscription.SubscriptionEpisodeAcquired)
			},
			want: ErrInvalidSubscriptionEpisode,
		},
		{
			name: "绑定缺意图",
			call: func() error {
				return store.BindSubscriptionEpisodes(context.Background(), "alice", 5, uuid.Nil, episodeUnitFixture())
			},
			want: ErrInvalidSubscriptionEpisode,
		},
		{
			name: "绑定事务缺事务",
			call: func() error {
				return store.BindSubscriptionEpisodesTx(context.Background(), pgx.Tx(nil), "alice", 5, uuid.New(), episodeUnitFixture())
			},
			want: ErrSubscriptionEpisodePersistence,
		},
		{
			name: "验证事务缺事务",
			call: func() error {
				return store.VerifySubscriptionEpisodesBoundToIntentTx(context.Background(), pgx.Tx(nil), "alice", 5, uuid.New(), episodeUnitFixture())
			},
			want: ErrSubscriptionEpisodePersistence,
		},
		{
			name: "批量推进缺意图",
			call: func() error {
				_, err := store.TransitionSubscriptionEpisodesByIntent(context.Background(), "alice", uuid.Nil,
					[]subscription.SubscriptionEpisodeState{subscription.SubscriptionEpisodeMissing},
					subscription.SubscriptionEpisodeAcquired)
				return err
			},
			want: ErrInvalidSubscriptionEpisode,
		},
		{
			name: "汇总缺订阅 ID",
			call: func() error {
				_, err := store.SummarizeSubscriptionEpisodes(context.Background(), "alice", 0)
				return err
			},
			want: ErrInvalidSubscriptionEpisode,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.call(); !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
		})
	}
}

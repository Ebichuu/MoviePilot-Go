package database

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

// 本文件覆盖精准搜索存储层的参数与上下文闸门：
// 非法参数、nil 上下文、已取消上下文、未就绪 store，以及连接不可用时的持久化错误。

func preciseSearchUnitStore(t *testing.T) *Store {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), "postgres://invalid:invalid@127.0.0.1:1/none?sslmode=disable")
	if err != nil {
		t.Fatalf("创建不可用连接池: %v", err)
	}
	t.Cleanup(pool.Close)
	return &Store{db: &DB{Pool: pool}}
}

type preciseSearchGuardCase struct {
	name string
	call func(ctx context.Context, store *Store) error
	// txMethods 传 nil 事务，事务闸门先于上下文检查触发，
	// 因此「已取消上下文」子用例改为断言持久化错误。
	txMethods bool
}

func preciseSearchGuardCases() []preciseSearchGuardCase {
	now := time.Now().UTC()
	validFor := func() time.Time { return now }
	return []preciseSearchGuardCase{
		{
			name: "ListSubscriptionsForPreciseSearch",
			call: func(ctx context.Context, store *Store) error {
				_, err := store.ListSubscriptionsForPreciseSearch(ctx, validFor(), time.Time{}, 0, 10)
				return err
			},
		},
		{
			name: "GetActiveSubscription",
			call: func(ctx context.Context, store *Store) error {
				_, err := store.GetActiveSubscription(ctx, " alice ", 5)
				return err
			},
		},
		{
			name: "GetOwnedSubscription",
			call: func(ctx context.Context, store *Store) error {
				_, err := store.GetOwnedSubscription(ctx, "alice", 5)
				return err
			},
		},
		{
			name: "CompareAndSwapSubscriptionPreciseSearchState",
			call: func(ctx context.Context, store *Store) error {
				return store.CompareAndSwapSubscriptionPreciseSearchState(ctx, "alice", 5, subscription.Active)
			},
		},
		{
			name: "CompareAndSwapSubscriptionPreciseSearchStateAt",
			call: func(ctx context.Context, store *Store) error {
				return store.CompareAndSwapSubscriptionPreciseSearchStateAt(ctx, "alice", 5, subscription.Active, validFor())
			},
		},
		{
			name: "CompleteSatisfiedTVSubscription",
			call: func(ctx context.Context, store *Store) error {
				return store.CompleteSatisfiedTVSubscription(ctx, "alice", 5, subscription.Active, validFor())
			},
		},
		{
			name: "CompleteSatisfiedBestVersionTVSubscription",
			call: func(ctx context.Context, store *Store) error {
				return store.CompleteSatisfiedBestVersionTVSubscription(ctx, "alice", 5, subscription.Active, validFor())
			},
		},
		{
			name: "CompleteSatisfiedMovieSubscription",
			call: func(ctx context.Context, store *Store) error {
				return store.CompleteSatisfiedMovieSubscription(ctx, "alice", 5, subscription.Active, validFor())
			},
		},
		{
			name: "CompareAndSwapSubscriptionPreciseSearchStateTx",
			call: func(ctx context.Context, store *Store) error {
				return store.CompareAndSwapSubscriptionPreciseSearchStateTx(ctx, nil, "alice", 5, subscription.Active)
			},
			txMethods: true,
		},
		{
			name: "BindDownloadIntentSubscriptionTx",
			call: func(ctx context.Context, store *Store) error {
				return store.BindDownloadIntentSubscriptionTx(ctx, nil, "alice", uuid.New(), 5)
			},
			txMethods: true,
		},
	}
}

func TestSubscriptionPreciseSearchGuardsRejectBadContextAndStore(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	for _, tc := range preciseSearchGuardCases() {
		t.Run(tc.name+"/nil 上下文", func(t *testing.T) {
			if err := tc.call(nil, preciseSearchUnitStore(t)); !errors.Is(err, ErrSubscriptionPreciseSearchPersistence) &&
				!errors.Is(err, ErrDownloadIntentSubscriptionBindingPersistence) {
				t.Fatalf("nil 上下文 error = %v", err)
			}
		})
		t.Run(tc.name+"/已取消上下文", func(t *testing.T) {
			err := tc.call(canceled, preciseSearchUnitStore(t))
			if tc.txMethods {
				if !errors.Is(err, ErrSubscriptionPreciseSearchPersistence) &&
					!errors.Is(err, ErrDownloadIntentSubscriptionBindingPersistence) {
					t.Fatalf("事务方法已取消上下文 error = %v", err)
				}
				return
			}
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("已取消上下文 error = %v", err)
			}
		})
		t.Run(tc.name+"/store 未就绪", func(t *testing.T) {
			if err := tc.call(context.Background(), &Store{}); !errors.Is(err, ErrSubscriptionPreciseSearchPersistence) &&
				!errors.Is(err, ErrDownloadIntentSubscriptionBindingPersistence) {
				t.Fatalf("store 未就绪 error = %v", err)
			}
		})
		t.Run(tc.name+"/连接不可用", func(t *testing.T) {
			if err := tc.call(context.Background(), preciseSearchUnitStore(t)); !errors.Is(err, ErrSubscriptionPreciseSearchPersistence) &&
				!errors.Is(err, ErrDownloadIntentSubscriptionBindingPersistence) {
				t.Fatalf("连接不可用 error = %v", err)
			}
		})
	}
}

func TestSubscriptionPreciseSearchGuardsRejectInvalidArguments(t *testing.T) {
	store := preciseSearchUnitStore(t)
	now := time.Now().UTC()

	cases := []struct {
		name string
		call func() error
		want error
	}{
		{
			name: "游标缺失且时间无效",
			call: func() error {
				_, err := store.ListSubscriptionsForPreciseSearch(context.Background(), time.Time{}, time.Time{}, 0, 10)
				return err
			},
			want: ErrInvalidSubscriptionPreciseSearch,
		},
		{
			name: "批次大小越界",
			call: func() error {
				_, err := store.ListSubscriptionsForPreciseSearch(context.Background(), now, time.Time{}, 0, 0)
				return err
			},
			want: ErrInvalidSubscriptionPreciseSearch,
		},
		{
			name: "游标只有一半",
			call: func() error {
				_, err := store.ListSubscriptionsForPreciseSearch(context.Background(), now, time.Time{}, 7, 10)
				return err
			},
			want: ErrInvalidSubscriptionPreciseSearch,
		},
		{
			name: "查询游标合法",
			call: func() error {
				_, err := store.ListSubscriptionsForPreciseSearch(context.Background(), now, now, 7, 10)
				return err
			},
			want: ErrSubscriptionPreciseSearchPersistence,
		},
		{
			name: "空用户名",
			call: func() error {
				_, err := store.GetActiveSubscription(context.Background(), "  ", 5)
				return err
			},
			want: ErrInvalidSubscriptionPreciseSearch,
		},
		{
			name: "非正订阅 ID",
			call: func() error {
				_, err := store.GetOwnedSubscription(context.Background(), "alice", 0)
				return err
			},
			want: ErrInvalidSubscriptionPreciseSearch,
		},
		{
			name: "终态不允许直接切换",
			call: func() error {
				return store.CompareAndSwapSubscriptionPreciseSearchState(context.Background(), "alice", 5, subscription.Completed)
			},
			want: ErrInvalidSubscriptionPreciseSearch,
		},
		{
			name: "版本时间缺失",
			call: func() error {
				return store.CompareAndSwapSubscriptionPreciseSearchStateAt(context.Background(), "alice", 5, subscription.Active, time.Time{})
			},
			want: ErrInvalidSubscriptionPreciseSearch,
		},
		{
			name: "完成动作缺版本时间",
			call: func() error {
				return store.CompleteSatisfiedTVSubscription(context.Background(), "alice", 5, subscription.Active, time.Time{})
			},
			want: ErrInvalidSubscriptionPreciseSearch,
		},
		{
			name: "最佳版本完成缺用户",
			call: func() error {
				return store.CompleteSatisfiedBestVersionTVSubscription(context.Background(), " ", 5, subscription.Active, now)
			},
			want: ErrInvalidSubscriptionPreciseSearch,
		},
		{
			name: "电影完成缺版本时间",
			call: func() error {
				return store.CompleteSatisfiedMovieSubscription(context.Background(), "alice", 5, subscription.Active, time.Time{})
			},
			want: ErrInvalidSubscriptionPreciseSearch,
		},
		{
			name: "事务版本切换缺事务",
			call: func() error {
				return store.CompareAndSwapSubscriptionPreciseSearchStateTx(context.Background(), pgx.Tx(nil), "alice", 5, subscription.Active)
			},
			want: ErrSubscriptionPreciseSearchPersistence,
		},
		{
			name: "绑定意图缺意图 ID",
			call: func() error {
				return store.BindDownloadIntentSubscriptionTx(context.Background(), pgx.Tx(nil), "alice", uuid.Nil, 5)
			},
			want: ErrInvalidDownloadIntentSubscriptionBinding,
		},
		{
			name: "绑定意图缺订阅 ID",
			call: func() error {
				return store.BindDownloadIntentSubscriptionTx(context.Background(), pgx.Tx(nil), "alice", uuid.New(), 0)
			},
			want: ErrInvalidDownloadIntentSubscriptionBinding,
		},
		{
			name: "绑定意图缺事务",
			call: func() error {
				return store.BindDownloadIntentSubscriptionTx(context.Background(), pgx.Tx(nil), "alice", uuid.New(), 5)
			},
			want: ErrDownloadIntentSubscriptionBindingPersistence,
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

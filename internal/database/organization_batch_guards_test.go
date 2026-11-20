package database

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// 本文件覆盖整理批次存储层的调用闸门与参数校验：
// nil 上下文、已取消上下文、未就绪 store、连接不可用，以及各方法的非法参数。

func organizationUnitStore(t *testing.T) *Store {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), "postgres://invalid:invalid@127.0.0.1:1/none?sslmode=disable")
	if err != nil {
		t.Fatalf("创建不可用连接池: %v", err)
	}
	t.Cleanup(pool.Close)
	return &Store{db: &DB{Pool: pool}}
}

type organizationGuardCase struct {
	name string
	call func(ctx context.Context, store *Store) error
}

func organizationGuardCases() []organizationGuardCase {
	batchID, token := uuid.New(), uuid.New()
	return []organizationGuardCase{
		{
			name: "GetOrganizationBatch",
			call: func(ctx context.Context, store *Store) error {
				_, err := store.GetOrganizationBatch(ctx, "alice", batchID)
				return err
			},
		},
		{
			name: "GetOrganizationBatchByIntent",
			call: func(ctx context.Context, store *Store) error {
				_, err := store.GetOrganizationBatchByIntent(ctx, "alice", batchID)
				return err
			},
		},
		{
			name: "ListDueOrganizationBatchIDs",
			call: func(ctx context.Context, store *Store) error {
				_, err := store.ListDueOrganizationBatchIDs(ctx, time.Now().UTC(), 10)
				return err
			},
		},
		{
			name: "ClaimOrganizationBatches",
			call: func(ctx context.Context, store *Store) error {
				_, err := store.ClaimOrganizationBatches(ctx, time.Now().UTC(), 10, minOrganizationLease)
				return err
			},
		},
		{
			name: "ClaimOrganizationBatch",
			call: func(ctx context.Context, store *Store) error {
				_, _, err := store.ClaimOrganizationBatch(ctx, batchID, time.Now().UTC(), minOrganizationLease)
				return err
			},
		},
		{
			name: "RenewOrganizationBatchLease",
			call: func(ctx context.Context, store *Store) error {
				return store.RenewOrganizationBatchLease(ctx, batchID, token, time.Now().UTC(), minOrganizationLease)
			},
		},
	}
}

func TestOrganizationBatchGuardsRejectBadContextAndStore(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	for _, tc := range organizationGuardCases() {
		t.Run(tc.name+"/nil 上下文", func(t *testing.T) {
			if err := tc.call(nil, organizationUnitStore(t)); !errors.Is(err, ErrOrganizationPersistence) {
				t.Fatalf("nil 上下文 error = %v", err)
			}
		})
		t.Run(tc.name+"/已取消上下文", func(t *testing.T) {
			if err := tc.call(canceled, organizationUnitStore(t)); !errors.Is(err, context.Canceled) {
				t.Fatalf("已取消上下文 error = %v", err)
			}
		})
		t.Run(tc.name+"/store 未就绪", func(t *testing.T) {
			if err := tc.call(context.Background(), &Store{}); !errors.Is(err, ErrOrganizationPersistence) {
				t.Fatalf("store 未就绪 error = %v", err)
			}
		})
		t.Run(tc.name+"/连接不可用", func(t *testing.T) {
			if err := tc.call(context.Background(), organizationUnitStore(t)); !errors.Is(err, ErrOrganizationPersistence) {
				t.Fatalf("连接不可用 error = %v", err)
			}
		})
	}
}

func TestOrganizationBatchGuardsRejectInvalidArguments(t *testing.T) {
	store := organizationUnitStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	batchID, token := uuid.New(), uuid.New()
	longOwner := strings.Repeat("a", maxOrganizationOwnerLength+1)

	cases := []struct {
		name string
		call func() error
	}{
		{
			name: "读取批次缺用户",
			call: func() error {
				_, err := store.GetOrganizationBatch(ctx, "  ", batchID)
				return err
			},
		},
		{
			name: "读取批次超长用户",
			call: func() error {
				_, err := store.GetOrganizationBatch(ctx, longOwner, batchID)
				return err
			},
		},
		{
			name: "读取批次缺批次 ID",
			call: func() error {
				_, err := store.GetOrganizationBatch(ctx, "alice", uuid.Nil)
				return err
			},
		},
		{
			name: "按意图读取缺意图 ID",
			call: func() error {
				_, err := store.GetOrganizationBatchByIntent(ctx, "alice", uuid.Nil)
				return err
			},
		},
		{
			name: "枚举到期批次缺时间",
			call: func() error {
				_, err := store.ListDueOrganizationBatchIDs(ctx, time.Time{}, 10)
				return err
			},
		},
		{
			name: "枚举到期批次越界",
			call: func() error {
				_, err := store.ListDueOrganizationBatchIDs(ctx, now, maxOrganizationClaimSize+1)
				return err
			},
		},
		{
			name: "领取批次租约过短",
			call: func() error {
				_, err := store.ClaimOrganizationBatches(ctx, now, 10, minOrganizationLease/2)
				return err
			},
		},
		{
			name: "领取批次租约过长",
			call: func() error {
				_, err := store.ClaimOrganizationBatches(ctx, now, 10, 2*maxOrganizationLease)
				return err
			},
		},
		{
			name: "领取单批次缺批次 ID",
			call: func() error {
				_, _, err := store.ClaimOrganizationBatch(ctx, uuid.Nil, now, minOrganizationLease)
				return err
			},
		},
		{
			name: "领取单批次缺时间",
			call: func() error {
				_, _, err := store.ClaimOrganizationBatch(ctx, batchID, time.Time{}, minOrganizationLease)
				return err
			},
		},
		{
			name: "续租缺 token",
			call: func() error {
				return store.RenewOrganizationBatchLease(ctx, batchID, uuid.Nil, now, minOrganizationLease)
			},
		},
		{
			name: "续租缺时间",
			call: func() error {
				return store.RenewOrganizationBatchLease(ctx, batchID, token, time.Time{}, minOrganizationLease)
			},
		},
		{
			name: "续租时长越界",
			call: func() error {
				return store.RenewOrganizationBatchLease(ctx, batchID, token, now, 2*maxOrganizationLease)
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.call(); !errors.Is(err, ErrInvalidOrganizationInput) {
				t.Fatalf("error = %v, want ErrInvalidOrganizationInput", err)
			}
		})
	}
}

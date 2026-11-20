package database

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/moviepilot-go/moviepilot-go/internal/mediaindex"
)

// 本文件覆盖媒体入库确认存储层的调用闸门与参数校验：
// nil 上下文、已取消上下文、未就绪 store、连接不可用，以及各方法的非法参数。

func mediaIndexUnitStore(t *testing.T) *Store {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), "postgres://invalid:invalid@127.0.0.1:1/none?sslmode=disable")
	if err != nil {
		t.Fatalf("创建不可用连接池: %v", err)
	}
	t.Cleanup(pool.Close)
	return &Store{db: &DB{Pool: pool}}
}

type mediaIndexGuardCase struct {
	name string
	call func(ctx context.Context, store *Store) error
}

func mediaIndexGuardCases() []mediaIndexGuardCase {
	requestID, token := uuid.New(), uuid.New()
	lease := minMediaIndexLease
	return []mediaIndexGuardCase{
		{
			name: "Claim",
			call: func(ctx context.Context, store *Store) error {
				_, _, err := store.Claim(ctx, requestID, time.Now().UTC(), lease)
				return err
			},
		},
		{
			name: "Renew",
			call: func(ctx context.Context, store *Store) error {
				now := time.Now().UTC()
				return store.Renew(ctx, requestID, token, now, now.Add(lease))
			},
		},
		{
			name: "RenewMediaIndexLease",
			call: func(ctx context.Context, store *Store) error {
				return store.RenewMediaIndexLease(ctx, requestID, token, time.Now().UTC(), lease)
			},
		},
		{
			name: "Reschedule",
			call: func(ctx context.Context, store *Store) error {
				return store.Reschedule(ctx, requestID, token, time.Now().UTC(), "重试")
			},
		},
		{
			name: "RecordRefresh",
			call: func(ctx context.Context, store *Store) error {
				now := time.Now().UTC()
				return store.RecordRefresh(ctx, requestID, token, now, now.Add(time.Minute))
			},
		},
		{
			name: "ListPendingFiles",
			call: func(ctx context.Context, store *Store) error {
				_, err := store.ListPendingFiles(ctx, requestID, token, 10)
				return err
			},
		},
		{
			name: "ConfirmFile",
			call: func(ctx context.Context, store *Store) error {
				return store.ConfirmFile(ctx, requestID, token, 1, "item-1")
			},
		},
		{
			name: "ConfirmRequest",
			call: func(ctx context.Context, store *Store) error {
				_, err := store.ConfirmRequest(ctx, requestID, token, time.Now().UTC())
				return err
			},
		},
		{
			name: "RecoverExpired",
			call: func(ctx context.Context, store *Store) error {
				_, err := store.RecoverExpired(ctx, time.Now().UTC(), 10)
				return err
			},
		},
		{
			name: "ListDue",
			call: func(ctx context.Context, store *Store) error {
				_, err := store.ListDue(ctx, time.Now().UTC(), 10)
				return err
			},
		},
	}
}

func TestMediaIndexGuardsRejectBadContextAndStore(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	for _, tc := range mediaIndexGuardCases() {
		t.Run(tc.name+"/nil 上下文", func(t *testing.T) {
			if err := tc.call(nil, mediaIndexUnitStore(t)); !errors.Is(err, mediaindex.ErrInvalidRequest) {
				t.Fatalf("nil 上下文 error = %v", err)
			}
		})
		t.Run(tc.name+"/已取消上下文", func(t *testing.T) {
			if err := tc.call(canceled, mediaIndexUnitStore(t)); !errors.Is(err, context.Canceled) {
				t.Fatalf("已取消上下文 error = %v", err)
			}
		})
		t.Run(tc.name+"/store 未就绪", func(t *testing.T) {
			if err := tc.call(context.Background(), &Store{}); !errors.Is(err, mediaindex.ErrInvalidRequest) {
				t.Fatalf("store 未就绪 error = %v", err)
			}
		})
		t.Run(tc.name+"/连接不可用", func(t *testing.T) {
			err := tc.call(context.Background(), mediaIndexUnitStore(t))
			if !errors.Is(err, mediaindex.ErrPersistence) {
				t.Fatalf("连接不可用 error = %v", err)
			}
		})
	}
}

func TestMediaIndexGuardsRejectInvalidArguments(t *testing.T) {
	store := mediaIndexUnitStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	requestID, token := uuid.New(), uuid.New()

	cases := []struct {
		name string
		call func() error
	}{
		{
			name: "领取缺请求 ID",
			call: func() error {
				_, _, err := store.Claim(ctx, uuid.Nil, now, minMediaIndexLease)
				return err
			},
		},
		{
			name: "领取租约越界",
			call: func() error {
				_, _, err := store.Claim(ctx, requestID, now, minMediaIndexLease/2)
				return err
			},
		},
		{
			name: "续租缺 token",
			call: func() error {
				return store.Renew(ctx, requestID, uuid.Nil, now, now.Add(time.Minute))
			},
		},
		{
			name: "续租时长为零",
			call: func() error {
				return store.Renew(ctx, requestID, token, now, now)
			},
		},
		{
			name: "重排缺重试时间",
			call: func() error {
				return store.Reschedule(ctx, requestID, token, time.Time{}, "重试")
			},
		},
		{
			name: "记录刷新缺时间",
			call: func() error {
				return store.RecordRefresh(ctx, requestID, token, time.Time{}, now)
			},
		},
		{
			name: "读取待确认文件批次越界",
			call: func() error {
				_, err := store.ListPendingFiles(ctx, requestID, token, 0)
				return err
			},
		},
		{
			name: "确认文件缺文件 ID",
			call: func() error {
				return store.ConfirmFile(ctx, requestID, token, 0, "item-1")
			},
		},
		{
			name: "确认文件条目为空",
			call: func() error {
				return store.ConfirmFile(ctx, requestID, token, 1, "   ")
			},
		},
		{
			name: "确认文件条目含控制字符",
			call: func() error {
				return store.ConfirmFile(ctx, requestID, token, 1, "item\n1")
			},
		},
		{
			name: "确认请求缺时间",
			call: func() error {
				_, err := store.ConfirmRequest(ctx, requestID, token, time.Time{})
				return err
			},
		},
		{
			name: "续租兼容包装缺请求 ID",
			call: func() error {
				return store.RenewMediaIndexLease(ctx, uuid.Nil, token, now, minMediaIndexLease)
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.call(); !errors.Is(err, mediaindex.ErrInvalidRequest) {
				t.Fatalf("error = %v, want mediaindex.ErrInvalidRequest", err)
			}
		})
	}
}

package database

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// 本文件覆盖组织文件存储层的参数与连接闸门：
// 非法所有者/批次/文件 ID、nil 上下文、未就绪 store，以及连接不可用时的持久化错误。

func organizationFileGuardStore(t *testing.T) *Store {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), "postgres://invalid:invalid@127.0.0.1:1/none?sslmode=disable")
	if err != nil {
		t.Fatalf("创建不可用连接池: %v", err)
	}
	t.Cleanup(pool.Close)
	return &Store{db: &DB{Pool: pool}}
}

func TestListOrganizationFilesGuards(t *testing.T) {
	ctx := context.Background()
	store := organizationFileGuardStore(t)
	batchID := uuid.New()

	invalid := []struct {
		name     string
		username string
		batchID  uuid.UUID
		afterID  int64
		limit    int
	}{
		{name: "空所有者", batchID: batchID, limit: 10},
		{name: "所有者超长", username: strings.Repeat("a", maxOrganizationOwnerLength+1), batchID: batchID, limit: 10},
		{name: "nil 批次", username: "alice", limit: 10},
		{name: "游标为负", username: "alice", batchID: batchID, afterID: -1, limit: 10},
		{name: "页大小为零", username: "alice", batchID: batchID},
		{name: "页大小超限", username: "alice", batchID: batchID, limit: maxOrganizationPageSize + 1},
	}
	for _, tc := range invalid {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := store.ListOrganizationFiles(ctx, tc.username, tc.batchID, tc.afterID, tc.limit); !errors.Is(err, ErrInvalidOrganizationInput) {
				t.Fatalf("error = %v", err)
			}
			if _, err := (&Store{}).ListOrganizationFiles(ctx, tc.username, tc.batchID, tc.afterID, tc.limit); !errors.Is(err, ErrInvalidOrganizationInput) {
				t.Fatalf("未就绪 error = %v", err)
			}
		})
	}

	if _, err := store.ListOrganizationFiles(nil, "alice", batchID, 0, 10); err == nil {
		t.Fatal("nil 上下文应返回错误")
	}
	if _, err := store.ListOrganizationFiles(ctx, "alice", batchID, 0, 10); err == nil {
		t.Fatal("连接不可用时应返回错误")
	}
}

func TestClaimOrganizationFilesGuards(t *testing.T) {
	ctx := context.Background()
	store := organizationFileGuardStore(t)
	batchID, token := uuid.New(), uuid.New()

	invalid := []struct {
		name    string
		batchID uuid.UUID
		token   uuid.UUID
		limit   int
	}{
		{name: "nil 批次", token: token, limit: 10},
		{name: "nil 租约令牌", batchID: batchID, limit: 10},
		{name: "页大小为零", batchID: batchID, token: token},
		{name: "页大小超限", batchID: batchID, token: token, limit: maxOrganizationPageSize + 1},
	}
	for _, tc := range invalid {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := store.ClaimOrganizationFiles(ctx, tc.batchID, tc.token, tc.limit); !errors.Is(err, ErrInvalidOrganizationInput) {
				t.Fatalf("error = %v", err)
			}
			if _, err := (&Store{}).ClaimOrganizationFiles(ctx, tc.batchID, tc.token, tc.limit); !errors.Is(err, ErrInvalidOrganizationInput) {
				t.Fatalf("未就绪 error = %v", err)
			}
		})
	}

	if _, err := store.ClaimOrganizationFiles(nil, batchID, token, 10); err == nil {
		t.Fatal("nil 上下文应返回错误")
	}
	if _, err := store.ClaimOrganizationFiles(ctx, batchID, token, 10); err == nil {
		t.Fatal("连接不可用时应返回错误")
	}
}

func TestOrganizationFileMutationGuards(t *testing.T) {
	ctx := context.Background()
	store := organizationFileGuardStore(t)
	batchID, token := uuid.New(), uuid.New()

	mutations := []struct {
		name string
		call func(ctx context.Context, store *Store, batchID, token uuid.UUID, fileID int64) error
	}{
		{
			name: "CompleteOrganizationFile",
			call: func(ctx context.Context, store *Store, batchID, token uuid.UUID, fileID int64) error {
				return store.CompleteOrganizationFile(ctx, batchID, token, fileID)
			},
		},
		{
			name: "FailOrganizationFile",
			call: func(ctx context.Context, store *Store, batchID, token uuid.UUID, fileID int64) error {
				return store.FailOrganizationFile(ctx, batchID, token, fileID, "失败原因")
			},
		},
		{
			name: "FinalizeOrganizationFileFailure",
			call: func(ctx context.Context, store *Store, batchID, token uuid.UUID, fileID int64) error {
				return store.FinalizeOrganizationFileFailure(ctx, batchID, token, fileID, "终态失败")
			},
		},
	}
	for _, tc := range mutations {
		t.Run(tc.name, func(t *testing.T) {
			invalidArgs := []struct {
				name    string
				batchID uuid.UUID
				token   uuid.UUID
				fileID  int64
			}{
				{name: "nil 批次", token: token, fileID: 1},
				{name: "nil 令牌", batchID: batchID, fileID: 1},
				{name: "文件 ID 为零", batchID: batchID, token: token},
			}
			for _, arg := range invalidArgs {
				if err := tc.call(ctx, store, arg.batchID, arg.token, arg.fileID); !errors.Is(err, ErrInvalidOrganizationInput) {
					t.Fatalf("%s error = %v", arg.name, err)
				}
			}
			if err := tc.call(nil, store, batchID, token, 1); err == nil {
				t.Fatal("nil 上下文应返回错误")
			}
			if err := tc.call(ctx, &Store{}, batchID, token, 1); err == nil {
				t.Fatal("未就绪 store 应返回错误")
			}
			if err := tc.call(ctx, store, batchID, token, 1); err == nil {
				t.Fatal("连接不可用时应返回错误")
			}
		})
	}
}

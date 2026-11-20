//go:build integration

package database

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/moviepilot-go/moviepilot-go/internal/scrapeflow"
)

// TestPersistenceErrorHelpersIntegration 逐个校验持久化错误包装器：
// 上下文已取消时透传 ctx.Err()，否则返回各自的哨兵错误。
func TestPersistenceErrorHelpersIntegration(t *testing.T) {
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	live := t.Context()

	cases := []struct {
		name   string
		helper func(context.Context) error
		want   error
	}{
		{"organizationSuccessSnapshotPersistenceError", organizationSuccessSnapshotPersistenceError, ErrOrganizationSuccessSnapshotPersistence},
		{"subscriptionDownloadStatePersistenceError", subscriptionDownloadStatePersistenceError, ErrSubscriptionDownloadStatePersistence},
		{"subscriptionPreciseSearchPersistenceError", subscriptionPreciseSearchPersistenceError, ErrSubscriptionPreciseSearchPersistence},
		{"downloadIntentSubscriptionBindingPersistenceError", downloadIntentSubscriptionBindingPersistenceError, ErrDownloadIntentSubscriptionBindingPersistence},
		{"subscriptionPublicMutationPersistenceError", subscriptionPublicMutationPersistenceError, ErrSubscriptionPublicMutationPersistence},
		{"subscriptionTargetPersistenceError", subscriptionTargetPersistenceError, ErrSubscriptionTargetPersistence},
		{"scrapePersistenceError", scrapePersistenceError, scrapeflow.ErrPersistence},
	}
	for _, testCase := range cases {
		if err := testCase.helper(live); !errors.Is(err, testCase.want) {
			t.Fatalf("%s(有效 ctx) = %v, want %v", testCase.name, err, testCase.want)
		}
		if err := testCase.helper(canceled); !errors.Is(err, context.Canceled) {
			t.Fatalf("%s(已取消 ctx) = %v, want context.Canceled", testCase.name, err)
		}
		if err := testCase.helper(nil); !errors.Is(err, testCase.want) {
			t.Fatalf("%s(nil ctx) = %v, want %v", testCase.name, err, testCase.want)
		}
	}

	// 成功快照的上下文判定单独处理 nil：没有上下文就不算失败。
	if err := organizationSuccessSnapshotContextError(nil); err != nil {
		t.Fatalf("organizationSuccessSnapshotContextError(nil) = %v", err)
	}
	if err := organizationSuccessSnapshotContextError(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("organizationSuccessSnapshotContextError(canceled) = %v", err)
	}
	if err := organizationSuccessSnapshotContextError(live); err != nil {
		t.Fatalf("organizationSuccessSnapshotContextError(live) = %v", err)
	}

	if got := normalizeSideEffectError("  boom  "); got != "boom" {
		t.Fatalf("normalizeSideEffectError(trim) = %q", got)
	}
	if got := normalizeSideEffectError(strings.Repeat("x", 200)); len(got) != 128 {
		t.Fatalf("normalizeSideEffectError(截断) 长度 = %d, want 128", len(got))
	}
	if got := normalizeSideEffectError(""); got != "" {
		t.Fatalf("normalizeSideEffectError(空) = %q", got)
	}
}

// TestOrganizationLeaseFailureIntegration 覆盖租约失败判定：批次不存在、已存在
// 以及上下文取消三条分支。
func TestOrganizationLeaseFailureIntegration(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	ctx := t.Context()

	if err := store.RecordManualOrganization(ctx, "/downloads/lease.mkv", "/library/lease.mkv"); err != nil {
		t.Fatalf("RecordManualOrganization() error = %v", err)
	}
	var batchID uuid.UUID
	if err := store.db.Pool.QueryRow(ctx, `SELECT id FROM organization_batches LIMIT 1`).Scan(&batchID); err != nil {
		t.Fatalf("读取批次 ID: %v", err)
	}
	if err := organizationLeaseFailure(ctx, store, batchID); !errors.Is(err, ErrOrganizationLeaseConflict) {
		t.Fatalf("已存在批次 = %v, want ErrOrganizationLeaseConflict", err)
	}
	if err := organizationLeaseFailure(ctx, store, uuid.New()); !errors.Is(err, ErrOrganizationBatchNotFound) {
		t.Fatalf("不存在的批次 = %v, want ErrOrganizationBatchNotFound", err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := organizationLeaseFailure(canceled, store, batchID); !errors.Is(err, context.Canceled) {
		t.Fatalf("取消上下文 = %v, want context.Canceled", err)
	}
}

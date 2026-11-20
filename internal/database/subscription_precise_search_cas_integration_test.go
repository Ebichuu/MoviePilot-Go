//go:build integration

package database

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

// TestCompareAndSwapSubscriptionPreciseSearchStateAtIntegration 覆盖带版本快照的
// 精准搜索状态 CAS：成功后状态迁移、旧版本冲突、入参校验与取消上下文。
func TestCompareAndSwapSubscriptionPreciseSearchStateAtIntegration(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	const username = "precise-search-cas-owner"
	insertSubscriptionIntegrationUsers(t, store, username)
	ctx := t.Context()
	subscriptionID := createSubscriptionIntegrationFixture(t, store, username, "precise-search-cas")

	version := readSubscriptionUpdatedAtIntegration(t, store, username, subscriptionID)
	if err := store.CompareAndSwapSubscriptionPreciseSearchStateAt(
		ctx, " "+username+" ", subscriptionID, subscription.Active, version,
	); err != nil {
		t.Fatalf("CAS(PendingSearch→Active) error = %v", err)
	}
	var internalState string
	if err := store.db.Pool.QueryRow(ctx,
		`SELECT internal_state FROM subscriptions WHERE id=$1`, subscriptionID).Scan(&internalState); err != nil {
		t.Fatalf("读取状态: %v", err)
	}
	if internalState != string(subscription.Active) {
		t.Fatalf("迁移后 internal_state = %q, want %q", internalState, subscription.Active)
	}

	// 旧版本快照必须冲突，且不会改动状态。
	if err := store.CompareAndSwapSubscriptionPreciseSearchStateAt(
		ctx, username, subscriptionID, subscription.Paused, version,
	); !errors.Is(err, ErrSubscriptionPreciseSearchStateConflict) {
		t.Fatalf("旧版本 CAS error = %v, want ErrSubscriptionPreciseSearchStateConflict", err)
	}

	// 状态不匹配（已不是 PendingSearch）时同样冲突。
	fresh := readSubscriptionUpdatedAtIntegration(t, store, username, subscriptionID)
	if err := store.CompareAndSwapSubscriptionPreciseSearchStateAt(
		ctx, username, subscriptionID, subscription.Paused, fresh,
	); !errors.Is(err, ErrSubscriptionPreciseSearchStateConflict) {
		t.Fatalf("状态不匹配 CAS error = %v, want ErrSubscriptionPreciseSearchStateConflict", err)
	}

	// 订阅不存在时返回 ErrSubscriptionNotFound。
	if err := store.CompareAndSwapSubscriptionPreciseSearchStateAt(
		ctx, username, 999999, subscription.Active, fresh,
	); !errors.Is(err, ErrSubscriptionNotFound) {
		t.Fatalf("缺失订阅 CAS error = %v, want ErrSubscriptionNotFound", err)
	}

	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := store.CompareAndSwapSubscriptionPreciseSearchStateAt(
		canceled, username, subscriptionID, subscription.Active, fresh,
	); !errors.Is(err, context.Canceled) {
		t.Fatalf("取消上下文 CAS error = %v, want context.Canceled", err)
	}

	// 入参校验：空 owner、非法 id、零值版本、不允许的目标状态。
	for _, testCase := range []struct {
		name    string
		owner   string
		id      int64
		next    subscription.State
		version time.Time
	}{
		{"空 owner", "  ", subscriptionID, subscription.Active, fresh},
		{"非法 id", username, 0, subscription.Active, fresh},
		{"零值版本", username, subscriptionID, subscription.Active, time.Time{}},
		{"不允许的目标状态", username, subscriptionID, subscription.Completed, fresh},
		{"未定义状态", username, subscriptionID, subscription.State("bogus"), fresh},
	} {
		if err := store.CompareAndSwapSubscriptionPreciseSearchStateAt(
			ctx, testCase.owner, testCase.id, testCase.next, testCase.version,
		); !errors.Is(err, ErrInvalidSubscriptionPreciseSearch) {
			t.Fatalf("%s CAS error = %v, want ErrInvalidSubscriptionPreciseSearch", testCase.name, err)
		}
	}

	// build 辅助函数单独校验：零值版本也要被拒绝。
	if _, _, err := buildCompareAndSwapSubscriptionPreciseSearchStateAt(
		username, subscriptionID, subscription.Active, time.Time{},
	); !errors.Is(err, ErrInvalidSubscriptionPreciseSearch) {
		t.Fatalf("build CAS 零值版本 error = %v", err)
	}
	if _, args, err := buildCompareAndSwapSubscriptionPreciseSearchStateAt(
		username, subscriptionID, subscription.Active, fresh,
	); err != nil || len(args) != 6 {
		t.Fatalf("build CAS = (%d args, %v), want 6", len(args), err)
	}

	// 未接数据库的 Store 直接返回持久化错误。
	empty := &Store{}
	if err := empty.CompareAndSwapSubscriptionPreciseSearchStateAt(
		ctx, username, subscriptionID, subscription.Active, fresh,
	); !errors.Is(err, ErrSubscriptionPreciseSearchPersistence) {
		t.Fatalf("空 Store CAS error = %v", err)
	}
}

func readSubscriptionUpdatedAtIntegration(t *testing.T, store *Store, username string, id int64) time.Time {
	t.Helper()
	var version time.Time
	if err := store.db.Pool.QueryRow(t.Context(),
		`SELECT updated_at FROM subscriptions WHERE id=$1 AND username=$2`, id, username).Scan(&version); err != nil {
		t.Fatalf("读取订阅版本: %v", err)
	}
	return version.UTC()
}

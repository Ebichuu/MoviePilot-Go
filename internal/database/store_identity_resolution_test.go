package database

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
)

// TestResolveSubscriptionIdentityErrors 覆盖订阅身份解析在插入/读取出现
// 非 ErrNoRows 错误时立即返回，以及持续冲突时的重试耗尽。
func TestResolveSubscriptionIdentityErrors(t *testing.T) {
	insertErr := errors.New("插入失败")
	if _, err := resolveSubscriptionIdentity(
		func() (int64, error) { return 0, insertErr },
		func() (int64, error) { return 0, nil },
	); !errors.Is(err, insertErr) {
		t.Fatalf("插入错误 error = %v", err)
	}

	refreshErr := errors.New("读取失败")
	if _, err := resolveSubscriptionIdentity(
		func() (int64, error) { return 0, pgx.ErrNoRows },
		func() (int64, error) { return 0, refreshErr },
	); !errors.Is(err, refreshErr) {
		t.Fatalf("读取错误 error = %v", err)
	}

	insertCalls, refreshCalls := 0, 0
	if _, err := resolveSubscriptionIdentity(
		func() (int64, error) { insertCalls++; return 0, pgx.ErrNoRows },
		func() (int64, error) { refreshCalls++; return 0, pgx.ErrNoRows },
	); err == nil {
		t.Fatal("持续冲突应返回重试耗尽错误")
	}
	if insertCalls != 3 || refreshCalls != 3 {
		t.Fatalf("尝试次数 = %d/%d", insertCalls, refreshCalls)
	}

	if id, err := resolveSubscriptionIdentity(
		func() (int64, error) { return 7, nil },
		func() (int64, error) { return 0, nil },
	); err != nil || id != 7 {
		t.Fatalf("插入成功 = %d, %v", id, err)
	}

	if id, err := resolveSubscriptionIdentity(
		func() (int64, error) { return 0, pgx.ErrNoRows },
		func() (int64, error) { return 9, nil },
	); err != nil || id != 9 {
		t.Fatalf("读取成功 = %d, %v", id, err)
	}
}

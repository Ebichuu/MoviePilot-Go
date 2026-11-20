package database

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// 本文件覆盖 RSS 游标存储层在连接不可用与参数互斥时的分支：
// 读取/领取/提交/失败提交的持久化错误，以及 NotModified 与 ReplaceCache 的互斥校验。

func TestRSSCursorPersistenceGuards(t *testing.T) {
	ctx := context.Background()
	store := rssGuardStore(t)
	now := time.Now().UTC()
	endpoint := "https://rss.example/feed.xml"

	t.Run("EnsureRSSCursor", func(t *testing.T) {
		if _, err := store.EnsureRSSCursor(ctx, 1, endpoint); !errors.Is(err, ErrRSSPersistence) {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("ListDueRSSCursors", func(t *testing.T) {
		if _, err := store.ListDueRSSCursors(ctx, now, 10); !errors.Is(err, ErrRSSPersistence) {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("TryAcquireRSSLease", func(t *testing.T) {
		if _, _, err := store.TryAcquireRSSLease(ctx, 1, endpoint, now, time.Minute); !errors.Is(err, ErrRSSPersistence) {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("CommitRSSPoll", func(t *testing.T) {
		token := uuid.New()
		result := RSSPollResult{ObservedAt: now}
		if err := store.CommitRSSPoll(ctx, 1, endpoint, token, result, now.Add(time.Minute)); !errors.Is(err, ErrRSSPersistence) {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("FailRSSPoll", func(t *testing.T) {
		token := uuid.New()
		if err := store.FailRSSPoll(ctx, 1, endpoint, token, now.Add(time.Minute), "失败"); !errors.Is(err, ErrRSSPersistence) {
			t.Fatalf("error = %v", err)
		}
	})
}

func TestCommitRSSPollRejectsConflictingFlags(t *testing.T) {
	ctx := context.Background()
	store := rssGuardStore(t)
	now := time.Now().UTC()
	endpoint := "https://rss.example/feed.xml"
	token := uuid.New()
	nextPoll := now.Add(time.Minute)

	cases := []struct {
		name   string
		result RSSPollResult
	}{
		{
			name: "未修改却带条目",
			result: RSSPollResult{ObservedAt: now, NotModified: true, Items: []RSSItemInput{{
				ItemKey: "item-1", Title: "示例",
			}}},
		},
		{
			name:   "未修改却要求替换缓存",
			result: RSSPollResult{ObservedAt: now, NotModified: true, ReplaceCache: true},
		},
		{
			name:   "替换缓存却没有条目",
			result: RSSPollResult{ObservedAt: now, ReplaceCache: true},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := store.CommitRSSPoll(ctx, 1, endpoint, token, tc.result, nextPoll); !errors.Is(err, ErrInvalidRSSInput) {
				t.Fatalf("error = %v", err)
			}
		})
	}

	t.Run("过长 endpoint", func(t *testing.T) {
		long := "https://rss.example/" + strings.Repeat("a", maxRSSCursorEndpoint)
		if err := store.CommitRSSPoll(ctx, 1, long, token, RSSPollResult{ObservedAt: now}, nextPoll); !errors.Is(err, ErrInvalidRSSInput) {
			t.Fatalf("error = %v", err)
		}
	})
}

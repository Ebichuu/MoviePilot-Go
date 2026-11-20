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

// 本文件覆盖 RSS 存储层的入口闸门：nil Store / nil 上下文 / 已取消上下文，
// 以及每个方法在触库之前必须拒绝的非法参数。

func rssGuardStore(t *testing.T) *Store {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), "postgres://invalid:invalid@127.0.0.1:1/none?sslmode=disable")
	if err != nil {
		t.Skipf("构造连接池失败: %v", err)
	}
	t.Cleanup(pool.Close)
	return &Store{db: &DB{Pool: pool}}
}

func TestRSSStoreRejectsNilStoreAndContext(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	now := time.Now().UTC()
	token := uuid.New()

	type guard struct {
		name string
		call func(store *Store, ctx context.Context) error
	}
	guards := []guard{
		{"EnsureRSSCursor", func(s *Store, c context.Context) error {
			_, err := s.EnsureRSSCursor(c, 1, "https://site.example/rss")
			return err
		}},
		{"ListDueRSSCursors", func(s *Store, c context.Context) error {
			_, err := s.ListDueRSSCursors(c, now, 10)
			return err
		}},
		{"TryAcquireRSSLease", func(s *Store, c context.Context) error {
			_, _, err := s.TryAcquireRSSLease(c, 1, "https://site.example/rss", now, time.Minute)
			return err
		}},
		{"CommitRSSPoll", func(s *Store, c context.Context) error {
			return s.CommitRSSPoll(c, 1, "https://site.example/rss", token,
				RSSPollResult{ObservedAt: now}, now.Add(time.Minute))
		}},
		{"FailRSSPoll", func(s *Store, c context.Context) error {
			return s.FailRSSPoll(c, 1, "https://site.example/rss", token, now, "失败")
		}},
		{"ListPendingRSSItems", func(s *Store, c context.Context) error {
			_, err := s.ListPendingRSSItems(c, 1, 10)
			return err
		}},
		{"ClaimRSSItems", func(s *Store, c context.Context) error {
			_, err := s.ClaimRSSItems(c, 1, 10, time.Minute)
			return err
		}},
		{"CompleteRSSItem", func(s *Store, c context.Context) error {
			return s.CompleteRSSItem(c, 1, token, "matched", "")
		}},
		{"UpdateRSSItemMediaRecognition", func(s *Store, c context.Context) error {
			return s.UpdateRSSItemMediaRecognition(c, 1, "标题")
		}},
		{"RecoverExpiredRSSLeases", func(s *Store, c context.Context) error {
			_, err := s.RecoverExpiredRSSLeases(c, now, 10)
			return err
		}},
		{"CleanupProcessedRSSItems", func(s *Store, c context.Context) error {
			_, err := s.CleanupProcessedRSSItems(c, 1, now, 10)
			return err
		}},
		{"ListRSSCacheSignatures", func(s *Store, c context.Context) error {
			_, err := s.ListRSSCacheSignatures(c, 1)
			return err
		}},
		{"ListRSSCacheSnapshot", func(s *Store, c context.Context) error {
			_, err := s.ListRSSCacheSnapshot(c, 1, now)
			return err
		}},
		{"RSSPollRoundReady", func(s *Store, c context.Context) error {
			_, err := s.RSSPollRoundReady(c, now, []int64{1})
			return err
		}},
		{"TrimRSSCacheItems", func(s *Store, c context.Context) error {
			_, err := s.TrimRSSCacheItems(c, 1, 10)
			return err
		}},
	}
	for _, guard := range guards {
		if err := guard.call(nil, ctx); !errors.Is(err, ErrRSSPersistence) {
			t.Fatalf("%s(nil store) = %v, want ErrRSSPersistence", guard.name, err)
		}
		if err := guard.call(&Store{}, ctx); !errors.Is(err, ErrRSSPersistence) {
			t.Fatalf("%s(空 db) = %v, want ErrRSSPersistence", guard.name, err)
		}
		store := rssGuardStore(t)
		if err := guard.call(store, nil); !errors.Is(err, ErrRSSPersistence) {
			t.Fatalf("%s(nil ctx) = %v, want ErrRSSPersistence", guard.name, err)
		}
		if err := guard.call(store, canceled); !errors.Is(err, context.Canceled) {
			t.Fatalf("%s(取消 ctx) = %v, want context.Canceled", guard.name, err)
		}
	}
}

func TestRSSStoreRejectsInvalidArguments(t *testing.T) {
	t.Parallel()
	store := rssGuardStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	token := uuid.New()
	tooLongEndpoint := "https://site.example/" + strings.Repeat("a", maxRSSCursorEndpoint)
	tooLongTitle := strings.Repeat("标", maxRSSTitle)

	cases := []struct {
		name string
		call func() error
	}{
		{"游标站点为零", func() error {
			_, err := store.EnsureRSSCursor(ctx, 0, "https://site.example/rss")
			return err
		}},
		{"游标地址为空", func() error {
			_, err := store.EnsureRSSCursor(ctx, 1, "   ")
			return err
		}},
		{"游标地址过长", func() error {
			_, err := store.EnsureRSSCursor(ctx, 1, tooLongEndpoint)
			return err
		}},
		{"到期游标时间为零", func() error {
			_, err := store.ListDueRSSCursors(ctx, time.Time{}, 10)
			return err
		}},
		{"到期游标上限为零", func() error {
			_, err := store.ListDueRSSCursors(ctx, now, 0)
			return err
		}},
		{"到期游标上限超量", func() error {
			_, err := store.ListDueRSSCursors(ctx, now, maxRSSItemsLimit+1)
			return err
		}},
		{"租约站点为零", func() error {
			_, _, err := store.TryAcquireRSSLease(ctx, 0, "https://site.example/rss", now, time.Minute)
			return err
		}},
		{"租约时间为零", func() error {
			_, _, err := store.TryAcquireRSSLease(ctx, 1, "https://site.example/rss", time.Time{}, time.Minute)
			return err
		}},
		{"租约过短", func() error {
			_, _, err := store.TryAcquireRSSLease(ctx, 1, "https://site.example/rss", now, time.Millisecond)
			return err
		}},
		{"租约过长", func() error {
			_, _, err := store.TryAcquireRSSLease(ctx, 1, "https://site.example/rss", now, 2*time.Hour)
			return err
		}},
		{"提交缺少 token", func() error {
			return store.CommitRSSPoll(ctx, 1, "https://site.example/rss", uuid.Nil,
				RSSPollResult{ObservedAt: now}, now.Add(time.Minute))
		}},
		{"提交缺少观测时间", func() error {
			return store.CommitRSSPoll(ctx, 1, "https://site.example/rss", token,
				RSSPollResult{}, now.Add(time.Minute))
		}},
		{"提交的下次时间不晚于观测时间", func() error {
			return store.CommitRSSPoll(ctx, 1, "https://site.example/rss", token,
				RSSPollResult{ObservedAt: now}, now)
		}},
		{"未变更仍携带条目", func() error {
			return store.CommitRSSPoll(ctx, 1, "https://site.example/rss", token,
				RSSPollResult{ObservedAt: now, NotModified: true, Items: []RSSItemInput{{ItemKey: "k"}}}, now.Add(time.Minute))
		}},
		{"未变更仍要求整体替换", func() error {
			return store.CommitRSSPoll(ctx, 1, "https://site.example/rss", token,
				RSSPollResult{ObservedAt: now, NotModified: true, ReplaceCache: true}, now.Add(time.Minute))
		}},
		{"整体替换却没有条目", func() error {
			return store.CommitRSSPoll(ctx, 1, "https://site.example/rss", token,
				RSSPollResult{ObservedAt: now, ReplaceCache: true}, now.Add(time.Minute))
		}},
		{"失败缺少 token", func() error {
			return store.FailRSSPoll(ctx, 1, "https://site.example/rss", uuid.Nil, now, "失败")
		}},
		{"失败缺少重试时间", func() error {
			return store.FailRSSPoll(ctx, 1, "https://site.example/rss", token, time.Time{}, "失败")
		}},
		{"待处理条目站点为零", func() error {
			_, err := store.ListPendingRSSItems(ctx, 0, 10)
			return err
		}},
		{"待处理条目上限超量", func() error {
			_, err := store.ListPendingRSSItems(ctx, 1, maxRSSItemsLimit+1)
			return err
		}},
		{"领取条目上限为零", func() error {
			_, err := store.ClaimRSSItems(ctx, 1, 0, time.Minute)
			return err
		}},
		{"领取条目租约过长", func() error {
			_, err := store.ClaimRSSItems(ctx, 1, 10, 2*time.Hour)
			return err
		}},
		{"完成条目缺少 token", func() error {
			return store.CompleteRSSItem(ctx, 1, uuid.Nil, "matched", "")
		}},
		{"完成条目状态非法", func() error {
			return store.CompleteRSSItem(ctx, 1, token, "archived", "")
		}},
		{"识别标题过长", func() error {
			return store.UpdateRSSItemMediaRecognition(ctx, 1, tooLongTitle)
		}},
		{"识别条目为零", func() error {
			return store.UpdateRSSItemMediaRecognition(ctx, 0, "标题")
		}},
		{"恢复租约上限为零", func() error {
			_, err := store.RecoverExpiredRSSLeases(ctx, now, 0)
			return err
		}},
		{"清理站点为零", func() error {
			_, err := store.CleanupProcessedRSSItems(ctx, 0, now, 10)
			return err
		}},
		{"清理截止时间为零", func() error {
			_, err := store.CleanupProcessedRSSItems(ctx, 1, time.Time{}, 10)
			return err
		}},
		{"缓存签名站点为零", func() error {
			_, err := store.ListRSSCacheSignatures(ctx, 0)
			return err
		}},
		{"缓存快照截止时间为零", func() error {
			_, err := store.ListRSSCacheSnapshot(ctx, 1, time.Time{})
			return err
		}},
		{"轮次就绪截止时间为零", func() error {
			_, err := store.RSSPollRoundReady(ctx, time.Time{}, nil)
			return err
		}},
		{"轮次就绪站点 ID 非正", func() error {
			_, err := store.RSSPollRoundReady(ctx, now, []int64{0})
			return err
		}},
		{"裁剪站点为零", func() error {
			_, err := store.TrimRSSCacheItems(ctx, 0, 10)
			return err
		}},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if err := tt.call(); !errors.Is(err, ErrInvalidRSSInput) {
				t.Fatalf("error = %v, want ErrInvalidRSSInput", err)
			}
		})
	}
}

func TestRSSPollRoundReadyEmptyBarrier(t *testing.T) {
	t.Parallel()
	store := rssGuardStore(t)
	ready, err := store.RSSPollRoundReady(context.Background(), time.Now().UTC(), nil)
	if err != nil || !ready {
		t.Fatalf("空屏障应视为就绪: (%v, %v)", ready, err)
	}
}

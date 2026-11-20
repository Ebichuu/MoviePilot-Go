package database

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/moviepilot-go/moviepilot-go/internal/mediaserver"
)

// 本文件覆盖媒体服务器条目缓存的调用闸门：nil/未就绪 store、nil 与已取消
// 上下文、非法条目、空查询，以及连接不可用时的错误归一。

func mediaServerItemGuardStore(t *testing.T) *Store {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), "postgres://invalid:invalid@127.0.0.1:1/none?sslmode=disable")
	if err != nil {
		t.Fatalf("创建不可用连接池: %v", err)
	}
	t.Cleanup(pool.Close)
	return &Store{db: &DB{Pool: pool}}
}

func validMediaServerItem() mediaserver.Item {
	return mediaserver.Item{
		Server: "plex", ItemID: "42", ItemType: mediaserver.ItemTypeMovie,
		Title: "示例电影", LstModDate: "2026-11-20T00:00:00Z",
	}
}

func TestUpsertMediaServerItemGuards(t *testing.T) {
	ctx := context.Background()
	item := validMediaServerItem()

	var nilStore *Store
	if err := nilStore.UpsertMediaServerItem(ctx, item); !errors.Is(err, mediaserver.ErrItemStore) {
		t.Fatalf("nil store error = %v", err)
	}
	if err := (&Store{}).UpsertMediaServerItem(ctx, item); !errors.Is(err, mediaserver.ErrItemStore) {
		t.Fatalf("未就绪 store error = %v", err)
	}

	store := mediaServerItemGuardStore(t)
	if err := store.UpsertMediaServerItem(nil, item); !errors.Is(err, mediaserver.ErrInvalidItem) {
		t.Fatalf("nil 上下文 error = %v", err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := store.UpsertMediaServerItem(canceled, item); !errors.Is(err, context.Canceled) {
		t.Fatalf("已取消上下文 error = %v", err)
	}
	if err := store.UpsertMediaServerItem(ctx, mediaserver.Item{}); !errors.Is(err, mediaserver.ErrInvalidItem) {
		t.Fatalf("非法条目 error = %v", err)
	}
	if err := store.UpsertMediaServerItem(ctx, item); !errors.Is(err, mediaserver.ErrItemStore) {
		t.Fatalf("连接不可用 error = %v", err)
	}
}

func TestDeleteStaleMediaServerItemsGuards(t *testing.T) {
	ctx := context.Background()

	var nilStore *Store
	if _, err := nilStore.DeleteStaleMediaServerItems(ctx, "plex", "2026-11-20T00:00:00Z"); !errors.Is(err, mediaserver.ErrItemStore) {
		t.Fatalf("nil store error = %v", err)
	}

	store := mediaServerItemGuardStore(t)
	if _, err := store.DeleteStaleMediaServerItems(nil, "plex", "2026-11-20T00:00:00Z"); !errors.Is(err, mediaserver.ErrInvalidItem) {
		t.Fatalf("nil 上下文 error = %v", err)
	}
	if _, err := store.DeleteStaleMediaServerItems(ctx, " ", "2026-11-20T00:00:00Z"); !errors.Is(err, mediaserver.ErrInvalidItem) {
		t.Fatalf("空服务器名 error = %v", err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := store.DeleteStaleMediaServerItems(canceled, "plex", "2026-11-20T00:00:00Z"); !errors.Is(err, context.Canceled) {
		t.Fatalf("已取消上下文 error = %v", err)
	}
	if _, err := store.DeleteStaleMediaServerItems(ctx, "plex", "2026-11-20T00:00:00Z"); !errors.Is(err, mediaserver.ErrItemStore) {
		t.Fatalf("连接不可用 error = %v", err)
	}
}

func TestDeleteExcludedMediaServerItemsGuards(t *testing.T) {
	ctx := context.Background()

	var nilStore *Store
	if _, err := nilStore.DeleteExcludedMediaServerItems(ctx, nil); !errors.Is(err, mediaserver.ErrItemStore) {
		t.Fatalf("nil store error = %v", err)
	}

	store := mediaServerItemGuardStore(t)
	if _, err := store.DeleteExcludedMediaServerItems(nil, []string{"plex"}); !errors.Is(err, mediaserver.ErrInvalidItem) {
		t.Fatalf("nil 上下文 error = %v", err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := store.DeleteExcludedMediaServerItems(canceled, []string{"plex"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("已取消上下文 error = %v", err)
	}
	if _, err := store.DeleteExcludedMediaServerItems(ctx, nil); !errors.Is(err, mediaserver.ErrItemStore) {
		t.Fatalf("清空缓存时连接不可用 error = %v", err)
	}
	if _, err := store.DeleteExcludedMediaServerItems(ctx, []string{"plex"}); !errors.Is(err, mediaserver.ErrItemStore) {
		t.Fatalf("按服务器清理时连接不可用 error = %v", err)
	}
}

func TestFindMediaServerItemIDGuards(t *testing.T) {
	ctx := context.Background()

	var nilStore *Store
	if _, _, err := nilStore.FindMediaServerItemID(ctx, mediaserver.Query{TMDBID: 1}); !errors.Is(err, mediaserver.ErrItemLookup) {
		t.Fatalf("nil store error = %v", err)
	}

	store := mediaServerItemGuardStore(t)
	if _, _, err := store.FindMediaServerItemID(nil, mediaserver.Query{TMDBID: 1}); !errors.Is(err, mediaserver.ErrItemLookup) {
		t.Fatalf("nil 上下文 error = %v", err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, _, err := store.FindMediaServerItemID(canceled, mediaserver.Query{TMDBID: 1}); !errors.Is(err, context.Canceled) {
		t.Fatalf("已取消上下文 error = %v", err)
	}
	if id, found, err := store.FindMediaServerItemID(ctx, mediaserver.Query{}); err != nil || found || id != "" {
		t.Fatalf("空查询 = %q, %v, %v", id, found, err)
	}
	if _, _, err := store.FindMediaServerItemID(ctx, mediaserver.Query{TMDBID: 7}); !errors.Is(err, mediaserver.ErrItemLookup) {
		t.Fatalf("按 TMDB 查询时连接不可用 error = %v", err)
	}
	if _, _, err := store.FindMediaServerItemID(ctx, mediaserver.Query{Title: "示例电影", ItemType: mediaserver.ItemTypeMovie}); !errors.Is(err, mediaserver.ErrItemLookup) {
		t.Fatalf("按标题查询时连接不可用 error = %v", err)
	}
}

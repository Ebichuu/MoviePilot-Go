//go:build integration

package database

import (
	"database/sql"
	"encoding/json"
	"errors"
	"testing"

	"github.com/pressly/goose/v3"

	"github.com/moviepilot-go/moviepilot-go/internal/mediaserver"
)

func TestMediaServerItemsMigrationPostgresDownUp(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	ctx := t.Context()
	var schema string
	if err := store.db.Pool.QueryRow(ctx, `SELECT current_schema()`).Scan(&schema); err != nil {
		t.Fatalf("读取隔离 schema: %v", err)
	}
	migrationDB, _ := openSubscriptionV2StateMigrationDB(t, schema)
	goose.SetBaseFS(migrationFS)
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatalf("设置 goose PostgreSQL 方言: %v", err)
	}
	if !mediaServerItemsTableExists(t, migrationDB) {
		t.Fatal("migration 037 Up 未创建 media_server_items")
	}
	if err := goose.DownToContext(ctx, migrationDB, "migrations", 36); err != nil {
		t.Fatalf("回退 migration 037: %v", err)
	}
	if mediaServerItemsTableExists(t, migrationDB) {
		t.Fatal("migration 037 Down 未删除 media_server_items")
	}
	if err := goose.UpToContext(ctx, migrationDB, "migrations", 37); err != nil {
		t.Fatalf("重新应用 migration 037: %v", err)
	}
	if !mediaServerItemsTableExists(t, migrationDB) {
		t.Fatal("migration 037 第二次 Up 未创建 media_server_items")
	}
}

func mediaServerItemsTableExists(t *testing.T, db *sql.DB) bool {
	t.Helper()
	var exists bool
	if err := db.QueryRowContext(t.Context(),
		`SELECT to_regclass('media_server_items') IS NOT NULL`).Scan(&exists); err != nil {
		t.Fatalf("检查 media_server_items: %v", err)
	}
	return exists
}

func TestMediaServerItemCachePostgresMatchesV2Lookup(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	ctx := t.Context()
	const (
		firstRun  = "2026-09-29 12:00:00.000000"
		secondRun = "2026-09-29 18:00:00.000000"
	)

	if err := store.UpsertMediaServerItem(ctx, mediaserver.Item{
		Server: "home", Library: "lib-1", ItemID: "movie-1", ItemType: mediaserver.ItemTypeMovie,
		Title: "Example Movie", OriginalTitle: "Original", Year: "2025", TMDBID: 123,
		IMDBID: "tt1", Path: "/media/movie-1.mkv", LstModDate: firstRun,
	}); err != nil {
		t.Fatalf("写入电影缓存: %v", err)
	}
	if err := store.UpsertMediaServerItem(ctx, mediaserver.Item{
		Server: "home", Library: "lib-2", ItemID: "series-1", ItemType: mediaserver.ItemTypeTV,
		Title: "Show", Year: "2026", TMDBID: 789, TVDBID: "tv1",
		SeasonInfo: map[int][]int{1: {1, 2, 3}}, LstModDate: firstRun,
	}); err != nil {
		t.Fatalf("写入剧集缓存: %v", err)
	}
	if err := store.UpsertMediaServerItem(ctx, mediaserver.Item{
		Server: "other", Library: "lib-3", ItemID: "movie-2", ItemType: mediaserver.ItemTypeMovie,
		Title: "Example Movie", Year: "2025", TMDBID: 321, LstModDate: firstRun,
	}); err != nil {
		t.Fatalf("写入其他服务器缓存: %v", err)
	}

	// V2 upserts by server + item_id and overwrites the previous run's payload.
	if err := store.UpsertMediaServerItem(ctx, mediaserver.Item{
		Server: "home", Library: "lib-1", ItemID: "movie-1", ItemType: mediaserver.ItemTypeMovie,
		Title: "Example Movie Renamed", Year: "2025", TMDBID: 123, LstModDate: secondRun,
	}); err != nil {
		t.Fatalf("覆盖电影缓存: %v", err)
	}
	var rows int
	if err := store.db.Pool.QueryRow(ctx,
		`SELECT count(*) FROM media_server_items WHERE server='home' AND item_id='movie-1'`).Scan(&rows); err != nil {
		t.Fatalf("统计 upsert 行数: %v", err)
	}
	if rows != 1 {
		t.Fatalf("upsert 行数 = %d, want 1", rows)
	}
	var title string
	var library *string
	if err := store.db.Pool.QueryRow(ctx,
		`SELECT title, library FROM media_server_items WHERE server='home' AND item_id='movie-1'`).
		Scan(&title, &library); err != nil {
		t.Fatalf("读取覆盖后的行: %v", err)
	}
	if title != "Example Movie Renamed" || library == nil || *library != "lib-1" {
		t.Fatalf("覆盖后的行 = (%q, %v)", title, library)
	}

	var seasonInfo map[string][]int
	var raw []byte
	if err := store.db.Pool.QueryRow(ctx,
		`SELECT seasoninfo FROM media_server_items WHERE item_id='series-1'`).Scan(&raw); err != nil {
		t.Fatalf("读取 seasoninfo: %v", err)
	}
	if err := json.Unmarshal(raw, &seasonInfo); err != nil {
		t.Fatalf("解析 seasoninfo %s: %v", raw, err)
	}
	if len(seasonInfo) != 1 || len(seasonInfo["1"]) != 3 || seasonInfo["1"][2] != 3 {
		t.Fatalf("seasoninfo = %#v, want V2 的 season -> episodes 对象", seasonInfo)
	}

	itemID, found, err := store.FindMediaServerItemID(ctx, mediaserver.Query{
		TMDBID: 123, ItemType: mediaserver.ItemTypeMovie, Title: "Example Movie Renamed",
	})
	if err != nil || !found || itemID != "movie-1" {
		t.Fatalf("FindMediaServerItemID(TMDB) = (%q, %v, %v)", itemID, found, err)
	}
	itemID, found, err = store.FindMediaServerItemID(ctx, mediaserver.Query{
		ItemType: mediaserver.ItemTypeMovie, Title: "Example Movie Renamed",
	})
	if err != nil || !found || itemID != "movie-1" {
		t.Fatalf("FindMediaServerItemID(title) = (%q, %v, %v)", itemID, found, err)
	}
	if _, found, err = store.FindMediaServerItemID(ctx, mediaserver.Query{
		ItemType: mediaserver.ItemTypeTV, Title: "Example Movie Renamed",
	}); err != nil || found {
		t.Fatalf("类型不匹配查询 = (%v, %v), want 未命中", found, err)
	}
	if _, found, err = store.FindMediaServerItemID(ctx, mediaserver.Query{}); err != nil || found {
		t.Fatalf("空查询 = (%v, %v), want 普通未命中", found, err)
	}

	deleted, err := store.DeleteStaleMediaServerItems(ctx, "home", secondRun)
	if err != nil {
		t.Fatalf("清理陈旧行: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("清理陈旧行数 = %d, want 1", deleted)
	}
	if itemID, found, err = store.FindMediaServerItemID(ctx, mediaserver.Query{
		TMDBID: 789, ItemType: mediaserver.ItemTypeTV,
	}); err != nil || found || itemID != "" {
		t.Fatalf("陈旧行查询 = (%q, %v, %v), want 已删除", itemID, found, err)
	}

	deleted, err = store.DeleteExcludedMediaServerItems(ctx, []string{"home"})
	if err != nil {
		t.Fatalf("清理被移除服务器: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("清理被移除服务器行数 = %d, want 1", deleted)
	}
	if _, err := store.DeleteExcludedMediaServerItems(ctx, nil); err != nil {
		t.Fatalf("空服务器列表清理: %v", err)
	}
	if err := store.db.Pool.QueryRow(ctx, `SELECT count(*) FROM media_server_items`).Scan(&rows); err != nil {
		t.Fatalf("统计清空后的行数: %v", err)
	}
	if rows != 0 {
		t.Fatalf("空列表清理后行数 = %d, want 0", rows)
	}
	if err := store.UpsertMediaServerItem(ctx, mediaserver.Item{
		Server: "home", ItemID: "movie-1", ItemType: "动画", LstModDate: secondRun,
	}); !errors.Is(err, mediaserver.ErrInvalidItem) {
		t.Fatalf("非法类型写入 error = %v, want %v", err, mediaserver.ErrInvalidItem)
	}
}

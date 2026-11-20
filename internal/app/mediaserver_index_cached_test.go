package app

import (
	"context"
	"errors"
	"testing"

	"github.com/moviepilot-go/moviepilot-go/internal/httpapi"
	"github.com/moviepilot-go/moviepilot-go/internal/mediaserver"
)

// TestFindCachedMediaServerItem 覆盖「标题/年份/类型」形式的本地媒体库存在性查询：
// 空缓存与空标题短路、类型归一化、命中返回 ID、错误统一包装。
func TestFindCachedMediaServerItem(t *testing.T) {
	ctx := context.Background()

	if itemID, found, err := (&mediaServerRuntime{}).findCachedMediaServerItem(
		ctx, httpapi.LocalMediaExistsQuery{Title: "沙丘"},
	); itemID != "" || found || err != nil {
		t.Fatalf("未接缓存 = (%q, %v, %v)", itemID, found, err)
	}

	lookup := &mediaServerItemLookupFake{itemID: "42", found: true}
	runtime := &mediaServerRuntime{itemLookup: lookup}
	if itemID, found, err := runtime.findCachedMediaServerItem(
		ctx, httpapi.LocalMediaExistsQuery{Title: "   ", Year: "2021"},
	); itemID != "" || found || err != nil {
		t.Fatalf("空标题 = (%q, %v, %v)", itemID, found, err)
	}
	if queries := lookup.recorded(); len(queries) != 0 {
		t.Fatalf("空标题不应查询缓存, queries = %#v", queries)
	}

	cases := []struct {
		name     string
		input    string
		wantType string
	}{
		{"电影", "movie", mediaserver.ItemTypeMovie},
		{"电影复数", "Movies", mediaserver.ItemTypeMovie},
		{"电影常量", mediaserver.ItemTypeMovie, mediaserver.ItemTypeMovie},
		{"剧集", "tv", mediaserver.ItemTypeTV},
		{"剧集别名", " TVShow ", mediaserver.ItemTypeTV},
		{"剧集常量", mediaserver.ItemTypeTV, mediaserver.ItemTypeTV},
		{"未知类型", "anime", ""},
		{"空类型", "", ""},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			lookup := &mediaServerItemLookupFake{itemID: "77", found: true}
			runtime := &mediaServerRuntime{itemLookup: lookup}
			itemID, found, err := runtime.findCachedMediaServerItem(ctx, httpapi.LocalMediaExistsQuery{
				Title: "  沙丘  ", Type: testCase.input, Year: " 2021 ",
			})
			if err != nil || !found || itemID != "77" {
				t.Fatalf("查询结果 = (%q, %v, %v)", itemID, found, err)
			}
			queries := lookup.recorded()
			if len(queries) != 1 {
				t.Fatalf("查询次数 = %d, want 1", len(queries))
			}
			if queries[0].Title != "沙丘" || queries[0].Year != "2021" || queries[0].ItemType != testCase.wantType {
				t.Fatalf("查询参数 = %#v, want type %q", queries[0], testCase.wantType)
			}
		})
	}

	// 未命中时保持 found=false。
	miss := &mediaServerRuntime{itemLookup: &mediaServerItemLookupFake{}}
	if itemID, found, err := miss.findCachedMediaServerItem(ctx, httpapi.LocalMediaExistsQuery{
		Title: "沙丘",
	}); itemID != "" || found || err != nil {
		t.Fatalf("未命中 = (%q, %v, %v)", itemID, found, err)
	}

	// 缓存层错误统一包装为 errLocalMediaServerLookup。
	broken := &mediaServerRuntime{itemLookup: &mediaServerItemLookupFake{err: errors.New("数据库不可用")}}
	if _, _, err := broken.findCachedMediaServerItem(ctx, httpapi.LocalMediaExistsQuery{
		Title: "沙丘",
	}); !errors.Is(err, errLocalMediaServerLookup) {
		t.Fatalf("缓存错误 = %v, want errLocalMediaServerLookup", err)
	}
}

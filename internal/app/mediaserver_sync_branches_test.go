package app

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/moviepilot-go/moviepilot-go/internal/mediaconfig"
	"github.com/moviepilot-go/moviepilot-go/internal/mediaserver/emby"
)

// syncBranchFixture 是可按端点开关失败的 Emby 假服务，用来打掉同步链路上的
// 逐分支错误路径（用户枚举、视图、条目分页、剧集分页）。
type syncBranchFixture struct {
	usersFails    bool
	usersCalls    atomic.Int32
	viewsFails    bool
	viewsBody     string
	itemsStatus   int
	itemsBody     func(parentID, startIndex string) string
	episodesFails bool
	episodesBody  string
}

func (f *syncBranchFixture) handler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.URL.Path == "/Users":
		f.usersCalls.Add(1)
		if f.usersFails {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		_, _ = io.WriteString(w, `[{"Id":"user-1","Name":"admin","Policy":{"IsAdministrator":true}}]`)
	case r.URL.Path == "/Users/user-1/Views":
		if f.viewsFails {
			http.Error(w, "views unavailable", http.StatusInternalServerError)
			return
		}
		body := f.viewsBody
		if body == "" {
			body = `{"Items":[{"Id":"lib-1","Name":"Movies","CollectionType":"movies"}]}`
		}
		_, _ = io.WriteString(w, body)
	case r.URL.Path == "/Users/user-1/Items":
		if f.itemsStatus != 0 {
			http.Error(w, "items unavailable", f.itemsStatus)
			return
		}
		if f.itemsBody == nil {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, f.itemsBody(
			r.URL.Query().Get("ParentId"), r.URL.Query().Get("StartIndex"),
		))
	case strings.HasPrefix(r.URL.Path, "/Shows/") && strings.HasSuffix(r.URL.Path, "/Episodes"):
		if f.episodesFails {
			http.Error(w, "episodes unavailable", http.StatusInternalServerError)
			return
		}
		_, _ = io.WriteString(w, f.episodesBody)
	default:
		http.NotFound(w, r)
	}
}

// newSyncBranchRuntime 起一个假 Emby 并按现有夹具组装只含一台服务器的运行态。
func newSyncBranchRuntime(
	t *testing.T,
	fixture *syncBranchFixture,
	store *mediaServerItemStoreFake,
) (*mediaServerRuntime, *mediaServerEntry) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(fixture.handler))
	t.Cleanup(server.Close)
	runtime, _ := newMediaServerSyncRuntime(t, server.URL, store, nil)
	if len(runtime.entries) != 1 || runtime.entries[0] == nil {
		t.Fatalf("夹具应装配一台媒体服务器，实际 %d 台", len(runtime.entries))
	}
	return runtime, runtime.entries[0]
}

// TestMediaServerEntryUserIDBranches 覆盖用户解析的缓存、nil 入口与非管理员令牌回退。
func TestMediaServerEntryUserIDBranches(t *testing.T) {
	if got := (&mediaServerRuntime{}).entryUserID(t.Context(), nil); got != "" {
		t.Fatalf("nil 条目用户 = %q，期望空", got)
	}
	if got := (&mediaServerRuntime{}).entryUserID(t.Context(), &mediaServerEntry{}); got != "" {
		t.Fatalf("缺客户端用户 = %q，期望空", got)
	}

	// 正常枚举：拿到真实用户并缓存，第二次不再请求 /Users。
	resolved, entry := newSyncBranchRuntime(t, &syncBranchFixture{}, &mediaServerItemStoreFake{})
	if got := resolved.entryUserID(t.Context(), entry); got != "user-1" {
		t.Fatalf("首次解析用户 = %q，期望 user-1", got)
	}
	if got := resolved.entryUserID(t.Context(), entry); got != "user-1" {
		t.Fatalf("缓存后用户 = %q，期望 user-1", got)
	}

	// 只读令牌：/Users 被拒绝时退回 Emby 的 Me 别名并照常缓存。
	forbidden := &syncBranchFixture{usersFails: true}
	fallback, fallbackEntry := newSyncBranchRuntime(t, forbidden, &mediaServerItemStoreFake{})
	if got := fallback.entryUserID(t.Context(), fallbackEntry); got != emby.SelfUserID {
		t.Fatalf("拒绝时的用户 = %q，期望 %q", got, emby.SelfUserID)
	}
	if got := fallback.entryUserID(t.Context(), fallbackEntry); got != emby.SelfUserID {
		t.Fatalf("回退缓存后用户 = %q，期望 %q", got, emby.SelfUserID)
	}
	if calls := forbidden.usersCalls.Load(); calls != 1 {
		t.Fatalf("/Users 调用次数 = %d，期望 1（回退结果应缓存）", calls)
	}
}

// TestMediaServerLoadSeasonInfoBranches 覆盖剧集分页的取消、失败、不一致、
// 去重/越界过滤与「未完整枚举」五种出口。
func TestMediaServerLoadSeasonInfoBranches(t *testing.T) {
	runtime, entry := newSyncBranchRuntime(t, &syncBranchFixture{}, &mediaServerItemStoreFake{})

	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, _, err := runtime.loadMediaServerSeasonInfo(canceled, entry, "series-1"); !errors.Is(err, context.Canceled) {
		t.Fatalf("取消上下文 = %v", err)
	}

	failing, failingEntry := newSyncBranchRuntime(t, &syncBranchFixture{episodesFails: true}, &mediaServerItemStoreFake{})
	if _, _, err := failing.loadMediaServerSeasonInfo(t.Context(), failingEntry, "series-1"); err == nil {
		t.Fatal("剧集接口失败应报错")
	}

	inconsistent, inconsistentEntry := newSyncBranchRuntime(t,
		&syncBranchFixture{episodesBody: `{"Items":[{"ParentIndexNumber":1,"IndexNumber":1}],"TotalRecordCount":1,"StartIndex":7}`},
		&mediaServerItemStoreFake{})
	if _, _, err := inconsistent.loadMediaServerSeasonInfo(t.Context(), inconsistentEntry, "series-1"); !errors.Is(err, errMediaServerItemSyncFailed) {
		t.Fatalf("分页不一致 = %v", err)
	}

	// 重复集、越界季/集与缺失索引都应被过滤或去重，结果按集号升序。
	dedupe, dedupeEntry := newSyncBranchRuntime(t, &syncBranchFixture{episodesBody: `{"Items":[` +
		`{"ParentIndexNumber":1,"IndexNumber":2},` +
		`{"ParentIndexNumber":1,"IndexNumber":2},` +
		`{"ParentIndexNumber":1,"IndexNumber":1},` +
		`{"ParentIndexNumber":1000,"IndexNumber":1},` +
		`{"ParentIndexNumber":1,"IndexNumber":0},` +
		`{"IndexNumber":3}` +
		`],"TotalRecordCount":6,"StartIndex":0}`}, &mediaServerItemStoreFake{})
	seasons, complete, err := dedupe.loadMediaServerSeasonInfo(t.Context(), dedupeEntry, "series-1")
	if err != nil || !complete {
		t.Fatalf("去重季信息 = (%v, %v, %v)", seasons, complete, err)
	}
	if len(seasons) != 1 || len(seasons[1]) != 2 || seasons[1][0] != 1 || seasons[1][1] != 2 {
		t.Fatalf("去重季信息 = %#v，期望 {1:[1 2]}", seasons)
	}

	// 服务端声称还有数据但返回空页：枚举不完整，不返回任何季信息。
	truncated, truncatedEntry := newSyncBranchRuntime(t,
		&syncBranchFixture{episodesBody: `{"Items":[],"TotalRecordCount":5,"StartIndex":0}`},
		&mediaServerItemStoreFake{})
	seasons, complete, err = truncated.loadMediaServerSeasonInfo(t.Context(), truncatedEntry, "series-1")
	if err != nil || complete || seasons != nil {
		t.Fatalf("截断季信息 = (%v, %v, %v)，期望 (nil, false, nil)", seasons, complete, err)
	}
}

// TestMediaServerSyncServerItemsBranches 覆盖单服务器同步的五个早退/失败分支。
func TestMediaServerSyncServerItemsBranches(t *testing.T) {
	runtime, _ := newSyncBranchRuntime(t, &syncBranchFixture{}, &mediaServerItemStoreFake{})
	store := &mediaServerItemStoreFake{}

	// 无法解析用户：保留旧行并报同步失败。
	if err := runtime.syncServerItems(t.Context(), store, nil); !errors.Is(err, errMediaServerItemSyncFailed) {
		t.Fatalf("缺用户同步 = %v", err)
	}

	viewsBroken, viewsEntry := newSyncBranchRuntime(t, &syncBranchFixture{viewsFails: true}, &mediaServerItemStoreFake{})
	if err := viewsBroken.syncServerItems(t.Context(), store, viewsEntry); err == nil {
		t.Fatal("视图接口失败应报错")
	}

	// 没有媒体库：直接跳过，不写缓存也不删旧行。
	emptyViews, emptyEntry := newSyncBranchRuntime(t,
		&syncBranchFixture{viewsBody: `{"Items":[]}`}, &mediaServerItemStoreFake{})
	emptyStore := &mediaServerItemStoreFake{}
	if err := emptyViews.syncServerItems(t.Context(), emptyStore, emptyEntry); err != nil {
		t.Fatalf("空媒体库同步 = %v", err)
	}
	if len(emptyStore.upserted()) != 0 || len(emptyStore.stale()) != 0 {
		t.Fatalf("空媒体库不应写缓存：%#v / %#v", emptyStore.upserted(), emptyStore.stale())
	}

	// 条目枚举失败：整轮判为未完成，不删除旧行。
	itemsBroken, itemsEntry := newSyncBranchRuntime(t,
		&syncBranchFixture{itemsStatus: http.StatusInternalServerError}, &mediaServerItemStoreFake{})
	brokenStore := &mediaServerItemStoreFake{}
	if err := itemsBroken.syncServerItems(t.Context(), brokenStore, itemsEntry); err == nil {
		t.Fatal("条目接口失败应报错")
	}
	if len(brokenStore.stale()) != 0 {
		t.Fatalf("未完成枚举不应删除旧行：%#v", brokenStore.stale())
	}

	// 清理旧行失败要向上返回。
	success, successEntry := newSyncBranchRuntime(t, &syncBranchFixture{itemsBody: func(string, string) string {
		return `{"Items":[{"Id":"movie-1","Type":"Movie","Name":"Example","ProviderIds":{"Tmdb":"1"}}],` +
			`"TotalRecordCount":1,"StartIndex":0}`
	}}, &mediaServerItemStoreFake{})
	cleanupStore := &mediaServerItemStoreFake{staleErr: errors.New("stale cleanup failed")}
	if err := success.syncServerItems(t.Context(), cleanupStore, successEntry); err == nil {
		t.Fatal("清理旧行失败应报错")
	}
}

// TestMediaServerSyncLibraryItemBranches 覆盖单媒体库枚举的入口校验、分页不一致、
// 过滤、目录深度上限、剧集失败/不完整、写失败与截断页。
func TestMediaServerSyncLibraryItemBranches(t *testing.T) {
	runtime, entry := newSyncBranchRuntime(t, &syncBranchFixture{}, &mediaServerItemStoreFake{})
	store := &mediaServerItemStoreFake{}
	library := emby.UserView{ID: "lib-1", Name: "Movies"}

	if _, err := runtime.syncLibraryItems(t.Context(), store, entry, "user-1", emby.UserView{}, "stamp"); !errors.Is(err, errMediaServerItemSyncFailed) {
		t.Fatalf("空媒体库 ID = %v", err)
	}

	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := runtime.syncLibraryItems(canceled, store, entry, "user-1", library, "stamp"); !errors.Is(err, context.Canceled) {
		t.Fatalf("取消上下文 = %v", err)
	}

	itemsBroken, itemsEntry := newSyncBranchRuntime(t,
		&syncBranchFixture{itemsStatus: http.StatusInternalServerError}, &mediaServerItemStoreFake{})
	if _, err := itemsBroken.syncLibraryItems(t.Context(), store, itemsEntry, "user-1", library, "stamp"); err == nil {
		t.Fatal("条目接口失败应报错")
	}

	inconsistent, inconsistentEntry := newSyncBranchRuntime(t, &syncBranchFixture{
		itemsBody: func(string, string) string {
			return `{"Items":[],"TotalRecordCount":3,"StartIndex":5}`
		}}, &mediaServerItemStoreFake{})
	if _, err := inconsistent.syncLibraryItems(t.Context(), store, inconsistentEntry, "user-1", library, "stamp"); !errors.Is(err, errMediaServerItemSyncFailed) {
		t.Fatalf("分页不一致 = %v", err)
	}

	// 空 ID、非影片/剧集类型与不可解析的 TMDB 都不写缓存，但枚举仍算完整。
	filtered, filteredEntry := newSyncBranchRuntime(t, &syncBranchFixture{
		itemsBody: func(string, string) string {
			return `{"Items":[` +
				`{"Id":"","Type":"Movie","Name":"No ID"},` +
				`{"Id":"audio-1","Type":"Audio","Name":"Track"},` +
				`{"Id":"bad-1","Type":"Movie","Name":"Bad","ProviderIds":{"Tmdb":"not-a-number"}}` +
				`],"TotalRecordCount":3,"StartIndex":0}`
		}}, &mediaServerItemStoreFake{})
	filteredStore := &mediaServerItemStoreFake{}
	result, err := filtered.syncLibraryItems(t.Context(), filteredStore, filteredEntry, "user-1", library, "stamp")
	if err != nil || !result.complete || result.items != 0 || result.folders != 0 {
		t.Fatalf("过滤结果 = (%+v, %v)", result, err)
	}
	if len(filteredStore.upserted()) != 0 {
		t.Fatalf("过滤项不应写缓存：%#v", filteredStore.upserted())
	}

	// 目录深度上限：每层再套一个 Folder，超过 maxDepth 后判为未完整枚举。
	deep, deepEntry := newSyncBranchRuntime(t, &syncBranchFixture{
		itemsBody: func(parentID, _ string) string {
			next := parentID + "-x"
			return `{"Items":[{"Id":"` + next + `","Type":"Folder","Name":"F"}],` +
				`"TotalRecordCount":1,"StartIndex":0}`
		}}, &mediaServerItemStoreFake{})
	deepResult, err := deep.syncLibraryItems(t.Context(), &mediaServerItemStoreFake{}, deepEntry, "user-1", library, "stamp")
	if err != nil || deepResult.complete {
		t.Fatalf("深度上限结果 = (%+v, %v)", deepResult, err)
	}

	// 剧集接口失败：整库返回错误。
	seriesBroken, seriesBrokenEntry := newSyncBranchRuntime(t, &syncBranchFixture{
		itemsBody: func(string, string) string {
			return `{"Items":[{"Id":"series-1","Type":"Series","Name":"Show"}],"TotalRecordCount":1,"StartIndex":0}`
		},
		episodesFails: true,
	}, &mediaServerItemStoreFake{})
	if _, err := seriesBroken.syncLibraryItems(t.Context(), store, seriesBrokenEntry, "user-1", library, "stamp"); err == nil {
		t.Fatal("剧集接口失败应报错")
	}

	// 剧集枚举不完整：该剧集不写缓存，整库判为未完成。
	seriesTruncated, seriesTruncatedEntry := newSyncBranchRuntime(t, &syncBranchFixture{
		itemsBody: func(string, string) string {
			return `{"Items":[{"Id":"series-1","Type":"Series","Name":"Show"}],"TotalRecordCount":1,"StartIndex":0}`
		},
		episodesBody: `{"Items":[],"TotalRecordCount":9,"StartIndex":0}`,
	}, &mediaServerItemStoreFake{})
	truncatedStore := &mediaServerItemStoreFake{}
	truncatedResult, err := seriesTruncated.syncLibraryItems(t.Context(), truncatedStore, seriesTruncatedEntry, "user-1", library, "stamp")
	if err != nil || truncatedResult.complete || truncatedResult.items != 0 {
		t.Fatalf("不完整剧集结果 = (%+v, %v)", truncatedResult, err)
	}
	if len(truncatedStore.upserted()) != 0 {
		t.Fatalf("不完整剧集不应写缓存：%#v", truncatedStore.upserted())
	}

	// 写入失败要向上返回。
	upsertBroken, upsertEntry := newSyncBranchRuntime(t, &syncBranchFixture{
		itemsBody: func(string, string) string {
			return `{"Items":[{"Id":"movie-1","Type":"Movie","Name":"Example","ProviderIds":{"Tmdb":"1"}}],` +
				`"TotalRecordCount":1,"StartIndex":0}`
		}}, &mediaServerItemStoreFake{})
	if _, err := upsertBroken.syncLibraryItems(t.Context(),
		&mediaServerItemStoreFake{upsertErr: errors.New("upsert failed")}, upsertEntry, "user-1", library, "stamp"); err == nil {
		t.Fatal("写缓存失败应报错")
	}

	// 服务端声称还有下一页但返回空页：判为未完整枚举。
	paged, pagedEntry := newSyncBranchRuntime(t, &syncBranchFixture{
		itemsBody: func(_ string, startIndex string) string {
			if startIndex == "" || startIndex == "0" {
				return `{"Items":[{"Id":"movie-1","Type":"Movie","Name":"Example","ProviderIds":{"Tmdb":"1"}}],` +
					`"TotalRecordCount":5,"StartIndex":0}`
			}
			return `{"Items":[],"TotalRecordCount":5,"StartIndex":` + startIndex + `}`
		}}, &mediaServerItemStoreFake{})
	pagedResult, err := paged.syncLibraryItems(t.Context(), &mediaServerItemStoreFake{}, pagedEntry, "user-1", library, "stamp")
	if err != nil || pagedResult.complete || pagedResult.items != 1 {
		t.Fatalf("截断分页结果 = (%+v, %v)", pagedResult, err)
	}
}

// TestMediaServerSyncSchedulesBranches 覆盖停用、空名、重复名与缺省间隔的四种跳过规则。
func TestMediaServerSyncSchedulesBranches(t *testing.T) {
	var nilRuntime *mediaServerRuntime
	if got := nilRuntime.SyncSchedules(time.Hour); got != nil {
		t.Fatalf("nil 运行态计划 = %#v，期望 nil", got)
	}

	runtime, _ := newSyncBranchRuntime(t, &syncBranchFixture{}, &mediaServerItemStoreFake{})
	runtime.mu.Lock()
	runtime.snapshot.Servers = []mediaconfig.Server{
		{ID: "a", Name: "Alpha", Enabled: true, SyncInterval: 3},
		{ID: "b", Name: "Beta", Enabled: false, SyncInterval: 3},
		{ID: "c", Name: "   ", Enabled: true, SyncInterval: 3},
		{ID: "d", Name: "Alpha", Enabled: true, SyncInterval: 5},
		{ID: "e", Name: "Gamma", Enabled: true, SyncInterval: 0},
	}
	runtime.mu.Unlock()

	// 缺省间隔为 0 时，未配置 sync_interval 的服务器也被跳过。
	schedules := runtime.SyncSchedules(0)
	if len(schedules) != 1 || schedules[0].Server != "Alpha" || schedules[0].Interval != 3*time.Hour {
		t.Fatalf("计划 = %#v，期望只有 Alpha/3h", schedules)
	}

	// 有缺省间隔时，sync_interval<=0 的服务器回落到缺省值。
	schedules = runtime.SyncSchedules(time.Hour)
	if len(schedules) != 2 || schedules[0].Server != "Alpha" || schedules[1].Server != "Gamma" ||
		schedules[1].Interval != time.Hour {
		t.Fatalf("计划 = %#v，期望 Alpha/3h 与 Gamma/1h", schedules)
	}
}

// TestMediaServerSyncMediaServerItemsBranches 覆盖整轮同步的清理失败、无命中与
// 单服务器未完成（不完整库）的汇总结果。
func TestMediaServerSyncMediaServerItemsBranches(t *testing.T) {
	// 排除清理失败要包装成同步失败。
	excludeStore := &mediaServerItemStoreFake{excludedErr: errors.New("exclude failed")}
	excludeBroken, _ := newSyncBranchRuntime(t, &syncBranchFixture{}, excludeStore)
	if err := excludeBroken.SyncMediaServerItems(t.Context(), ""); !errors.Is(err, errMediaServerItemSyncFailed) {
		t.Fatalf("排除清理失败 = %v，期望同步失败", err)
	}

	// 不存在的选择器不会同步任何服务器，也不会删除旧行。
	empty, _ := newSyncBranchRuntime(t, &syncBranchFixture{}, &mediaServerItemStoreFake{})
	emptyStore := &mediaServerItemStoreFake{}
	if err := empty.SetItemCache(emptyStore, &mediaServerItemLookupFake{}, "admin"); err != nil {
		t.Fatalf("SetItemCache() error = %v", err)
	}
	if err := empty.SyncMediaServerItems(t.Context(), "not-a-server"); err != nil {
		t.Fatalf("无命中同步 = %v", err)
	}
	if len(emptyStore.stale()) != 0 {
		t.Fatalf("无命中时不应清理旧行：%#v", emptyStore.stale())
	}

	// 分页截断的媒体库：整轮判为失败，但旧行不会被删除。
	truncated, truncatedEntry := newSyncBranchRuntime(t, &syncBranchFixture{
		itemsBody: func(_ string, startIndex string) string {
			if startIndex == "" || startIndex == "0" {
				return `{"Items":[{"Id":"movie-1","Type":"Movie","Name":"Example","ProviderIds":{"Tmdb":"1"}}],` +
					`"TotalRecordCount":5,"StartIndex":0}`
			}
			return `{"Items":[],"TotalRecordCount":5,"StartIndex":` + startIndex + `}`
		}}, &mediaServerItemStoreFake{})
	truncatedStore := &mediaServerItemStoreFake{}
	if err := truncated.SetItemCache(truncatedStore, &mediaServerItemLookupFake{}, "admin"); err != nil {
		t.Fatalf("SetItemCache() error = %v", err)
	}
	if err := truncated.SyncMediaServerItems(t.Context(), truncatedEntry.setting.Name); !errors.Is(err, errMediaServerItemSyncFailed) {
		t.Fatalf("截断库同步 = %v，期望同步失败", err)
	}
	if len(truncatedStore.stale()) != 0 {
		t.Fatalf("截断库不应清理旧行：%#v", truncatedStore.stale())
	}
}

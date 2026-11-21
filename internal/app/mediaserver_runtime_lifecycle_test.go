package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/moviepilot-go/moviepilot-go/internal/httpapi"
	"github.com/moviepilot-go/moviepilot-go/internal/mediaconfig"
	"github.com/moviepilot-go/moviepilot-go/internal/mediaindex"
	"github.com/moviepilot-go/moviepilot-go/internal/mediaserver/emby"
	"github.com/moviepilot-go/moviepilot-go/internal/organizer"
)

const mediaServerRuntimeSecondID = "6f1f1e0a-2f2b-4c3a-9c4f-9f6f2f1d0a11"

// mediaServerRuntimeStoreFake 是 mediaServerRuntimeStore 的可控替身。
type mediaServerRuntimeStoreFake struct {
	mu       sync.Mutex
	snapshot mediaconfig.Snapshot
	found    bool
	err      error
	calls    int
}

func (f *mediaServerRuntimeStoreFake) LoadMediaServers(context.Context) (mediaconfig.Snapshot, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.snapshot.Clone(), f.found, f.err
}

func (f *mediaServerRuntimeStoreFake) set(snapshot mediaconfig.Snapshot, found bool, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.snapshot, f.found, f.err = snapshot, found, err
}

func (f *mediaServerRuntimeStoreFake) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func mediaServerRuntimeTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newMediaServerRuntimeFromSnapshot(t *testing.T, snapshot mediaconfig.Snapshot) *mediaServerRuntime {
	t.Helper()
	runtime, err := buildMediaServerRuntime(snapshot, time.Second, mediaServerRuntimeTestLogger())
	if err != nil {
		t.Fatalf("buildMediaServerRuntime() error = %v", err)
	}
	t.Cleanup(runtime.CloseIdleConnections)
	return runtime
}

// newMediaServerRuntimeWithEmbyServer 构造一个指向假 Emby 的运行时，
// 返回可断言的服务器设置（ID/指纹取自规范化结果）。
func newMediaServerRuntimeWithEmbyServer(
	t *testing.T,
	host string,
	mutate func(*mediaconfig.Server),
) (*mediaServerRuntime, mediaconfig.Server) {
	t.Helper()
	setting := mediaServerRuntimeTestServer(nil)
	setting.Config.Host = host
	if mutate != nil {
		mutate(&setting)
	}
	snapshot, _, err := mediaconfig.Normalize([]mediaconfig.Server{setting})
	if err != nil {
		t.Fatalf("mediaconfig.Normalize() error = %v", err)
	}
	if err := mediaconfig.Validate(snapshot.Servers); err != nil {
		t.Fatalf("mediaconfig.Validate() error = %v", err)
	}
	return newMediaServerRuntimeFromSnapshot(t, snapshot), snapshot.Servers[0]
}

func TestLoadMediaServerRuntimeStoreFailure(t *testing.T) {
	t.Parallel()

	if _, err := loadMediaServerRuntime(t.Context(), nil, time.Second, nil); err == nil || err.Error() != "媒体服务器设置存储不可用" {
		t.Fatalf("loadMediaServerRuntime(nil store) error = %v", err)
	}

	store := &mediaServerRuntimeStoreFake{err: errors.New("db down")}
	_, err := loadMediaServerRuntime(t.Context(), store, time.Second, nil)
	if err == nil || err.Error() != "加载媒体服务器设置失败" {
		t.Fatalf("loadMediaServerRuntime(store error) error = %v", err)
	}
	// 存储失败不能被包装成「没有配置服务器」，否则调度层会以为用户没启用媒体服务器。
	if strings.Contains(err.Error(), "db down") {
		t.Fatalf("存储错误详情泄漏: %v", err)
	}
}

func TestLoadMediaServerRuntimeMissingSettingIsEmptyRuntime(t *testing.T) {
	t.Parallel()

	store := &mediaServerRuntimeStoreFake{found: false}
	runtime, err := loadMediaServerRuntime(t.Context(), store, time.Second, nil)
	if err != nil {
		t.Fatalf("loadMediaServerRuntime() error = %v", err)
	}
	if runtime == nil {
		t.Fatal("runtime 为 nil")
	}
	if got := runtime.ListMediaServerClients(t.Context()); len(got) != 0 || got == nil {
		t.Fatalf("ListMediaServerClients() = %#v, want empty non-nil", got)
	}
	if runtime.snapshot.RequiredID != "" {
		t.Fatalf("未配置时 RequiredID = %q", runtime.snapshot.RequiredID)
	}

	// 禁用的服务器必须被跳过：只声明不启用等于没接入。
	disabled := mediaServerRuntimeTestServer(nil)
	disabled.Enabled = false
	store.set(mediaconfig.Snapshot{Servers: []mediaconfig.Server{disabled}}, true, nil)
	runtime, err = loadMediaServerRuntime(t.Context(), store, time.Second, nil)
	if err != nil {
		t.Fatalf("loadMediaServerRuntime(禁用) error = %v", err)
	}
	if got := runtime.ListMediaServerClients(t.Context()); len(got) != 0 {
		t.Fatalf("禁用服务器不应装载: %#v", got)
	}
	if store.callCount() != 2 {
		t.Fatalf("LoadMediaServers 调用次数 = %d", store.callCount())
	}
}

func TestBuildMediaServerRuntimeRejectsUnsupportedConfiguration(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		servers []mediaconfig.Server
		want    string
	}{
		{
			name:    "不支持的类型",
			servers: []mediaconfig.Server{{ID: mediaServerRuntimeTestID, Name: "Unknown", Type: "unknown", Enabled: true}},
			want:    "媒体服务器类型当前不受支持",
		},
		{
			name: "缺少 host",
			servers: []mediaconfig.Server{{
				ID: mediaServerRuntimeTestID, Name: "Fixture Emby", Type: "emby", Enabled: true,
				Config: mediaconfig.Config{APIKey: "token"},
			}},
			want: "Emby 运行时配置无效",
		},
		{
			name: "缺少 API Key",
			servers: []mediaconfig.Server{{
				ID: mediaServerRuntimeTestID, Name: "Fixture Emby", Type: "emby", Enabled: true,
				Config: mediaconfig.Config{Host: "https://emby.example.test"},
			}},
			want: "Emby 运行时配置无效",
		},
		{
			name: "带查询参数的 host",
			servers: []mediaconfig.Server{{
				ID: mediaServerRuntimeTestID, Name: "Fixture Emby", Type: "emby", Enabled: true,
				Config: mediaconfig.Config{Host: "https://emby.example.test/?x=1", APIKey: "token"},
			}},
			want: "Emby 运行时配置无效",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runtime, err := buildMediaServerRuntime(
				mediaconfig.Snapshot{Servers: tt.servers}, time.Second, mediaServerRuntimeTestLogger(),
			)
			if err == nil || err.Error() != tt.want {
				t.Fatalf("buildMediaServerRuntime() error = %v, want %q", err, tt.want)
			}
			if runtime != nil {
				t.Fatal("失败时必须返回 nil，不能交出半装配的运行时")
			}
		})
	}

	// 禁用服务器即使配置不合法也不该让整个运行时装配失败。
	runtime, err := buildMediaServerRuntime(mediaconfig.Snapshot{Servers: []mediaconfig.Server{{
		ID: mediaServerRuntimeSecondID, Name: "Disabled", Type: "plex", Enabled: false,
	}}}, time.Second, nil)
	if err != nil || runtime == nil {
		t.Fatalf("禁用服务器 rebuild = %v %v", runtime, err)
	}
	if got := runtime.ListMediaServerClients(t.Context()); len(got) != 0 {
		t.Fatalf("禁用服务器被装载: %#v", got)
	}
}

func TestMediaServerRuntimeReloadSwapsOrKeepsEntries(t *testing.T) {
	t.Parallel()

	var nilRuntime *mediaServerRuntime
	if err := nilRuntime.Reload(t.Context(), &mediaServerRuntimeStoreFake{}, time.Second, nil); err == nil || err.Error() != "媒体服务器运行时不可用" {
		t.Fatalf("nil Reload error = %v", err)
	}

	first := mediaServerRuntimeTestServer(nil)
	second := first
	second.ID = mediaServerRuntimeSecondID
	second.Name = "Second Emby"

	store := &mediaServerRuntimeStoreFake{}
	store.set(mediaconfig.Snapshot{
		Servers:    []mediaconfig.Server{first},
		RequiredID: first.ID,
	}, true, nil)
	runtime := newMediaServerRuntimeFromSnapshot(t, mediaconfig.Snapshot{Servers: []mediaconfig.Server{first}, RequiredID: first.ID})
	if got := runtime.ListMediaServerClients(t.Context()); len(got) != 1 || got[0].Name != first.Name {
		t.Fatalf("初始客户端 = %#v", got)
	}

	// 重载失败必须保留旧条目：站点连不上时不能把整个媒体库能力清空。
	store.set(mediaconfig.Snapshot{}, false, errors.New("db down"))
	if err := runtime.Reload(t.Context(), store, time.Second, nil); err == nil {
		t.Fatal("Reload(store error) error = nil")
	}
	if got := runtime.ListMediaServerClients(t.Context()); len(got) != 1 || got[0].Name != first.Name {
		t.Fatalf("失败重载后客户端 = %#v, want 旧条目保留", got)
	}

	// 配置本身非法的重载同样保留旧条目。
	store.set(mediaconfig.Snapshot{Servers: []mediaconfig.Server{{
		ID: mediaServerRuntimeTestID, Name: "Broken", Type: "unknown", Enabled: true,
	}}}, true, nil)
	if err := runtime.Reload(t.Context(), store, time.Second, nil); err == nil {
		t.Fatal("Reload(非法配置) error = nil")
	}
	if got := runtime.ListMediaServerClients(t.Context()); len(got) != 1 || got[0].Name != first.Name {
		t.Fatalf("非法重载后客户端 = %#v, want 旧条目保留", got)
	}

	// 合法重载整体替换，而不是把新旧条目叠加。
	store.set(mediaconfig.Snapshot{
		Servers:    []mediaconfig.Server{second},
		RequiredID: second.ID,
	}, true, nil)
	if err := runtime.Reload(t.Context(), store, time.Second, nil); err != nil {
		t.Fatalf("Reload() error = %v", err)
	}
	clients := runtime.ListMediaServerClients(t.Context())
	if len(clients) != 1 || clients[0].Name != second.Name {
		t.Fatalf("重载后客户端 = %#v", clients)
	}
	if runtime.snapshot.RequiredID != second.ID {
		t.Fatalf("RequiredID = %q, want %q", runtime.snapshot.RequiredID, second.ID)
	}

	// 配置被清空时应撤下全部条目。
	store.set(mediaconfig.Snapshot{}, false, nil)
	if err := runtime.Reload(t.Context(), store, time.Second, nil); err != nil {
		t.Fatalf("Reload(空配置) error = %v", err)
	}
	if got := runtime.ListMediaServerClients(t.Context()); len(got) != 0 {
		t.Fatalf("空配置后客户端 = %#v", got)
	}
}

func TestMediaServerRuntimeItemCache(t *testing.T) {
	t.Parallel()

	store := &mediaServerItemStoreFake{}
	lookup := &mediaServerItemLookupFake{}
	var nilRuntime *mediaServerRuntime
	if err := nilRuntime.SetItemCache(store, lookup, "admin"); !errors.Is(err, errMediaServerItemSyncUnavailable) {
		t.Fatalf("nil runtime SetItemCache error = %v", err)
	}
	if store, lookup := nilRuntime.itemCache(); store != nil || lookup != nil {
		t.Fatal("nil runtime 的 itemCache 必须返回 nil")
	}

	runtime := newMediaServerRuntimeCompletionPlanTest(t, nil)
	if err := runtime.SetItemCache(nil, lookup, "admin"); !errors.Is(err, errMediaServerItemSyncUnavailable) {
		t.Fatalf("缺 store 的 SetItemCache error = %v", err)
	}
	if err := runtime.SetItemCache(store, nil, "admin"); !errors.Is(err, errMediaServerItemSyncUnavailable) {
		t.Fatalf("缺 lookup 的 SetItemCache error = %v", err)
	}
	if err := runtime.SetItemCache(store, lookup, "  admin  "); err != nil {
		t.Fatalf("SetItemCache() error = %v", err)
	}
	gotStore, gotLookup := runtime.itemCache()
	if gotStore != store || gotLookup != lookup {
		t.Fatal("itemCache 未返回装配时注入的缓存")
	}
	if runtime.bootstrapUsername != "admin" {
		t.Fatalf("bootstrapUsername = %q, want 裁剪后的 admin", runtime.bootstrapUsername)
	}
}

func TestMediaServerRuntimeResolveRejectsStaleIdentity(t *testing.T) {
	t.Parallel()

	setting := mediaServerRuntimeTestServer(nil)
	runtime := newMediaServerRuntimeCompletionPlanTest(t, []mediaconfig.Server{setting})
	valid := mediaindex.ServerRef{
		ID: setting.ID, Name: setting.Name, Type: "emby",
		Fingerprint: mediaServerEndpointFingerprint(setting),
	}

	var nilRuntime *mediaServerRuntime
	if _, found := nilRuntime.Resolve(valid); found {
		t.Fatal("nil runtime 不应解析出服务器")
	}
	if _, found := runtime.Resolve(mediaindex.ServerRef{}); found {
		t.Fatal("空 ref 不应解析出服务器")
	}
	if _, found := runtime.Resolve(mediaindex.ServerRef{
		ID: "unknown", Name: "x", Type: "emby", Fingerprint: strings.Repeat("0", 64),
	}); found {
		t.Fatal("未知 ID 不应解析出服务器")
	}
	// 展示名变了仍然可以解析：身份是 ID + 指纹，不是名字。
	renamed := valid
	renamed.Name = "old display name"
	if server, found := runtime.Resolve(renamed); !found || server == nil {
		t.Fatal("名字变化不应让指纹相同的服务器失效")
	}

	runtime.CloseIdleConnections()
	if _, found := runtime.Resolve(valid); found {
		t.Fatal("关闭连接后不应再解析出服务器")
	}
}

func TestMediaServerRuntimeCompletionPlanGuards(t *testing.T) {
	t.Parallel()

	plan := organizer.Plan{Operations: []organizer.Operation{{
		Target: "/mnt/media/Movie.mkv", ExpectedSize: 1024, ExpectedSHA256: strings.Repeat("a", 64),
	}}}

	var nilRuntime *mediaServerRuntime
	if _, err := nilRuntime.MediaIndexCompletionPlan(t.Context(), plan); err == nil || err.Error() != "媒体入库确认规划不可用" {
		t.Fatalf("nil runtime error = %v", err)
	}
	runtime := newMediaServerRuntimeCompletionPlanTest(t, nil)
	if _, err := runtime.MediaIndexCompletionPlan(nil, plan); err == nil || err.Error() != "媒体入库确认规划不可用" {
		t.Fatalf("nil ctx error = %v", err)
	}

	// RequiredID 指向一个没有装载进来的服务器：这是可重试状态，不是空计划。
	disabled := mediaServerRuntimeTestServer(nil)
	disabled.Enabled = false
	snapshot, _, err := mediaconfig.Normalize([]mediaconfig.Server{disabled})
	if err != nil {
		t.Fatalf("Normalize() error = %v", err)
	}
	snapshot.RequiredID = disabled.ID
	missing := newMediaServerRuntimeFromSnapshot(t, snapshot)
	if _, err := missing.MediaIndexCompletionPlan(t.Context(), plan); !errors.Is(err, mediaindex.ErrRequiredServerUnavailable) {
		t.Fatalf("RequiredID 未装载 error = %v, want ErrRequiredServerUnavailable", err)
	}

	// 只有非视频文件时没有可确认目标，必须报错而不是返回空计划。
	videoOnly := newMediaServerRuntimeCompletionPlanTest(t, []mediaconfig.Server{
		mediaServerRuntimeTestServer([]mediaconfig.PathMapping{{LocalRoot: "/mnt/media", EmbyRoot: "/library"}}),
	})
	nonVideo := organizer.Plan{Operations: []organizer.Operation{
		{Target: "/mnt/media/Movie.nfo", ExpectedSize: 1, ExpectedSHA256: strings.Repeat("b", 64)},
		{Target: "/mnt/media/poster.jpg", ExpectedSize: 1, ExpectedSHA256: strings.Repeat("c", 64)},
	}}
	_, err = videoOnly.MediaIndexCompletionPlan(t.Context(), nonVideo)
	if err == nil || err.Error() != "整理计划没有可确认的视频文件" {
		t.Fatalf("非视频计划 error = %v", err)
	}
}

func TestMediaServerRuntimeListsClientsInConfigOrder(t *testing.T) {
	t.Parallel()

	first := mediaServerRuntimeTestServer(nil)
	second := first
	second.ID = mediaServerRuntimeSecondID
	second.Name = "Secondary Emby"
	disabled := first
	disabled.ID = "8a0f3c55-0a3f-4d9a-a0d8-2f7c9c5a2b13"
	disabled.Name = "Disabled Emby"
	disabled.Enabled = false

	runtime := newMediaServerRuntimeCompletionPlanTest(t, []mediaconfig.Server{first, disabled, second})
	got := runtime.ListMediaServerClients(t.Context())
	want := []httpapi.MediaServerClient{
		{Name: first.Name, Type: "emby"},
		{Name: second.Name, Type: "emby"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ListMediaServerClients() = %#v, want %#v", got, want)
	}

	// entriesSnapshot 的选择器同时支持 ID 与大小写不敏感的名称。
	if entries := runtime.entriesSnapshot(first.ID); len(entries) != 1 {
		t.Fatalf("按 ID 过滤 = %d", len(entries))
	}
	if entries := runtime.entriesSnapshot(strings.ToUpper(second.Name)); len(entries) != 1 {
		t.Fatalf("按名称过滤（忽略大小写） = %d", len(entries))
	}
	if entries := runtime.entriesSnapshot("  " + second.Name + "  "); len(entries) != 1 {
		t.Fatalf("按名称过滤（含空白） = %d", len(entries))
	}
	if entries := runtime.entriesSnapshot("unknown"); len(entries) != 0 {
		t.Fatalf("未知选择器 = %d", len(entries))
	}
	var nilRuntime *mediaServerRuntime
	if entries := nilRuntime.entriesSnapshot(""); entries != nil {
		t.Fatalf("nil runtime entriesSnapshot = %#v", entries)
	}
}

func TestMediaServerRuntimePlayMediaServerItem(t *testing.T) {
	t.Parallel()

	host := "https://play.example.test/emby/"
	runtime, setting := newMediaServerRuntimeWithEmbyServer(t, "https://emby.example.test", func(server *mediaconfig.Server) {
		server.Config.PlayHost = host
	})

	if _, found, err := runtime.PlayMediaServerItem(nil, "item-1"); err != nil || found {
		t.Fatalf("nil ctx = found %v err %v", found, err)
	}
	if _, found, err := runtime.PlayMediaServerItem(t.Context(), "   "); err != nil || found {
		t.Fatalf("空 itemID = found %v err %v", found, err)
	}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, _, err := runtime.PlayMediaServerItem(cancelled, "item-1"); !errors.Is(err, context.Canceled) {
		t.Fatalf("已取消 ctx error = %v", err)
	}

	playback, found, err := runtime.PlayMediaServerItem(t.Context(), "item-1")
	if err != nil || !found {
		t.Fatalf("PlayMediaServerItem() = found %v err %v", found, err)
	}
	// PlayHost 的基础路径必须保留（反代到子路径的 Emby 很常见）。
	want := &httpapi.MediaServerPlayback{
		URL:        host + "web/index.html#!/item?id=item-1",
		ItemID:     "item-1",
		ServerID:   setting.ID,
		ServerType: "emby",
	}
	if !reflect.DeepEqual(playback, *want) {
		t.Fatalf("PlayMediaServerItem() = %#v, want %#v", playback, *want)
	}

	// 单个服务器拼不出地址时跳过它，不把一个空 URL 当成功返回。
	if _, found, err := runtime.PlayMediaServerItem(t.Context(), strings.Repeat("a", 4097)); err != nil || found {
		t.Fatalf("超长 itemID = found %v err %v", found, err)
	}

	empty := newMediaServerRuntimeCompletionPlanTest(t, nil)
	if _, found, _ := empty.PlayMediaServerItem(t.Context(), "item-1"); found {
		t.Fatal("没有服务器时不应返回播放地址")
	}
}

func TestMediaServerRuntimeListLibraries(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/Library/VirtualFolders" {
			http.Error(w, "unexpected path", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `[{"Name":"电影","CollectionType":"movies","Locations":["/mnt/movies"],"ItemId":"lib-1"},`+
			`{"Name":"剧集","CollectionType":"tvshows","Locations":["/mnt/tv","/mnt/tv2"],"ItemId":"lib-2"}]`)
	}))
	t.Cleanup(server.Close)

	runtime, setting := newMediaServerRuntimeWithEmbyServer(t, server.URL, nil)
	got, err := runtime.ListMediaServerLibraries(t.Context(), "", false)
	if err != nil {
		t.Fatalf("ListMediaServerLibraries() error = %v", err)
	}
	want := []httpapi.MediaServerLibrary{
		{
			Name: "电影", Server: setting.Name, Type: "emby", Collection: "movies",
			Locations: []string{"/mnt/movies"}, ItemID: "lib-1",
			ID: "lib-1", ServerType: "emby", Path: []string{"/mnt/movies"},
			Image: server.URL + "/Items/lib-1/Images/Primary",
			Link:  server.URL + "/web/index.html#!/videos?parentId=lib-1",
		},
		{
			Name: "剧集", Server: setting.Name, Type: "emby", Collection: "tvshows",
			Locations: []string{"/mnt/tv", "/mnt/tv2"}, ItemID: "lib-2",
			ID: "lib-2", ServerType: "emby", Path: []string{"/mnt/tv", "/mnt/tv2"},
			Image: server.URL + "/Items/lib-2/Images/Primary",
			Link:  server.URL + "/web/index.html#!/videos?parentId=lib-2",
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ListMediaServerLibraries() = %#v, want %#v", got, want)
	}

	// 选择器不匹配时一个请求都不该发出去。
	calls.Store(0)
	empty, err := runtime.ListMediaServerLibraries(t.Context(), "unknown", false)
	if err != nil {
		t.Fatalf("未知选择器 error = %v", err)
	}
	if len(empty) != 0 || empty == nil {
		t.Fatalf("未知选择器 = %#v, want 空非 nil", empty)
	}
	if calls.Load() != 0 {
		t.Fatalf("未知选择器发出了 %d 次请求", calls.Load())
	}

	// 选择器命中 ID 或名称都能用。
	if _, err := runtime.ListMediaServerLibraries(t.Context(), setting.Name, false); err != nil {
		t.Fatalf("按名称选择 error = %v", err)
	}
}

func TestMediaServerRuntimeListLibrariesFailure(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "sensitive upstream response", http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)

	runtime, _ := newMediaServerRuntimeWithEmbyServer(t, server.URL, nil)
	libraries, err := runtime.ListMediaServerLibraries(t.Context(), "", false)
	if libraries != nil {
		t.Fatalf("失败时返回了部分媒体库: %#v", libraries)
	}
	if err == nil || err.Error() != "媒体服务器请求失败" {
		t.Fatalf("ListMediaServerLibraries() error = %v", err)
	}
	if strings.Contains(err.Error(), "sensitive upstream response") || strings.Contains(err.Error(), server.URL) {
		t.Fatalf("错误泄漏上游细节: %v", err)
	}

	// nil 上下文不能在装配层崩掉，必须变成可读错误。
	if _, err := runtime.ListMediaServerLibraries(nil, "", false); err == nil {
		t.Fatal("nil ctx error = nil")
	}
}

func TestMediaServerRuntimeLatestAndPlayingAreEmpty(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/Users":
			_, _ = w.Write([]byte(`[{"Id":"u1","Name":"admin","Policy":{"IsAdministrator":true}}]`))
		case "/Users/u1/Items/Latest":
			_, _ = w.Write([]byte(`[]`))
		case "/Users/u1/Items/Resume":
			_, _ = w.Write([]byte(`{"Items":[]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	runtime, _ := newMediaServerRuntimeWithEmbyServer(t, server.URL, nil)
	latest, err := runtime.ListMediaServerLatest(t.Context(), "", 10)
	if err != nil || latest == nil || len(latest) != 0 {
		t.Fatalf("ListMediaServerLatest() = %#v %v", latest, err)
	}
	playing, err := runtime.ListMediaServerPlaying(t.Context(), "", 10)
	if err != nil || playing == nil || len(playing) != 0 {
		t.Fatalf("ListMediaServerPlaying() = %#v %v", playing, err)
	}
}

func TestMediaServerRuntimeExistingEpisodesGuards(t *testing.T) {
	t.Parallel()

	runtime, _ := newMediaServerRuntimeWithEmbyServer(t, "https://emby.example.test", nil)
	// 没有路径时直接返回空结果，不能拿空 Path 去查询整个媒体库。
	if got, err := runtime.ExistingEpisodes(t.Context(), httpapi.RemoteMediaInfo{Path: "  "}); err != nil || len(got) != 0 {
		t.Fatalf("空路径 = %#v %v", got, err)
	}

	// 未命中路径时，上游 500 必须变成可重试错误而不是空结果。
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(failing.Close)
	failingRuntime, _ := newMediaServerRuntimeWithEmbyServer(t, failing.URL, nil)
	if _, err := failingRuntime.ExistingEpisodes(t.Context(), httpapi.RemoteMediaInfo{Path: "/library/Show"}); err == nil || err.Error() != "媒体服务器请求失败" {
		t.Fatalf("上游 500 error = %v", err)
	}
}

func TestMediaServerRuntimeExistingEpisodesRejectsInvalidPagination(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "StartIndex 回显不一致",
			body: `{"Items":[],"TotalRecordCount":10,"StartIndex":5}`,
			want: "媒体服务器返回了无效分页",
		},
		{
			name: "单页超过上限",
			body: `{"Items":[` + strings.Repeat(`{"Id":"x","Type":"Movie"},`, existingEpisodePageSize) + `{"Id":"y","Type":"Movie"}],"TotalRecordCount":201,"StartIndex":0}`,
			want: "媒体服务器返回了无效分页",
		},
		{
			name: "总数小于已返回数",
			body: `{"Items":[{"Id":"1","Type":"Episode","ParentIndexNumber":1,"IndexNumber":1}],"TotalRecordCount":0,"StartIndex":0}`,
			want: "媒体服务器返回了无效分页",
		},
		{
			name: "总数大于返回却给空页",
			body: `{"Items":[],"TotalRecordCount":10,"StartIndex":0}`,
			want: "媒体服务器结果超过有界分页限制",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, tt.body)
			}))
			t.Cleanup(server.Close)

			runtime, _ := newMediaServerRuntimeWithEmbyServer(t, server.URL, nil)
			if _, err := runtime.ExistingEpisodes(t.Context(), httpapi.RemoteMediaInfo{Path: "/library/Show"}); err == nil || err.Error() != tt.want {
				t.Fatalf("ExistingEpisodes() error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestMediaServerRuntimeExistingEpisodesStopsAtBoundedLimit(t *testing.T) {
	t.Parallel()

	page := make([]string, 0, existingEpisodePageSize)
	for index := 0; index < existingEpisodePageSize; index++ {
		page = append(page, fmt.Sprintf(`{"Id":"e%d","Type":"Episode","ParentIndexNumber":1,"IndexNumber":%d}`, index, index+1))
	}
	pageJSON := "[" + strings.Join(page, ",") + "]"
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		start := r.URL.Query().Get("StartIndex")
		if start == "" {
			start = "0"
		}
		w.Header().Set("Content-Type", "application/json")
		// 永远有下一页：客户端必须在到达上限时报错，而不是无限翻页。
		_, _ = fmt.Fprintf(w, `{"Items":%s,"TotalRecordCount":%d,"StartIndex":%s}`, pageJSON, existingEpisodeMaxItems+1, start)
	}))
	t.Cleanup(server.Close)

	runtime, _ := newMediaServerRuntimeWithEmbyServer(t, server.URL, nil)
	_, err := runtime.ExistingEpisodes(t.Context(), httpapi.RemoteMediaInfo{Path: "/library/Show"})
	if err == nil || err.Error() != "媒体服务器结果超过有界分页限制" {
		t.Fatalf("ExistingEpisodes() error = %v", err)
	}
	if requests.Load() != existingEpisodeMaxItems/existingEpisodePageSize {
		t.Fatalf("请求次数 = %d, want %d", requests.Load(), existingEpisodeMaxItems/existingEpisodePageSize)
	}
}

func TestMediaServerRuntimeExistingEpisodesMergesAndSkipsDirtyItems(t *testing.T) {
	t.Parallel()

	episodePage := func(items string, total int) string {
		return fmt.Sprintf(`{"Items":[%s],"TotalRecordCount":%d,"StartIndex":0}`, items, total)
	}
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("Path") != "/library/Show" || r.URL.Query().Get("AnyProviderIdEquals") != "tmdb.42" {
			t.Errorf("unexpected query: %v", r.URL.Query())
		}
		if r.URL.Query().Get("Limit") != "200" || r.URL.Query().Get("IncludeItemTypes") != "Movie,Episode" {
			t.Errorf("unexpected page options: %v", r.URL.Query())
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, episodePage(
			`{"Id":"1","Type":"Episode","ParentIndexNumber":2,"IndexNumber":3},`+
				`{"Id":"2","Type":"Episode","ParentIndexNumber":2,"IndexNumber":1},`+
				`{"Id":"3","Type":"Movie"},`+
				`{"Id":"4","Type":"Episode","IndexNumber":1},`+
				`{"Id":"5","Type":"Episode","ParentIndexNumber":-1,"IndexNumber":1},`+
				`{"Id":"6","Type":"Episode","ParentIndexNumber":1,"IndexNumber":0}`,
			6,
		))
	}))
	t.Cleanup(first.Close)
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, episodePage(
			`{"Id":"7","Type":"Episode","ParentIndexNumber":2,"IndexNumber":2},`+
				`{"Id":"8","Type":"Episode","ParentIndexNumber":1,"IndexNumber":5}`,
			2,
		))
	}))
	t.Cleanup(second.Close)

	firstSetting := mediaServerRuntimeTestServer(nil)
	firstSetting.Config.Host = first.URL
	secondSetting := firstSetting
	secondSetting.ID = mediaServerRuntimeSecondID
	secondSetting.Name = "Second Emby"
	secondSetting.Config.Host = second.URL
	runtime := newMediaServerRuntimeCompletionPlanTest(t, []mediaconfig.Server{firstSetting, secondSetting})

	got, err := runtime.ExistingEpisodes(t.Context(), httpapi.RemoteMediaInfo{Path: "/library/Show", TMDBID: 42})
	if err != nil {
		t.Fatalf("ExistingEpisodes() error = %v", err)
	}
	want := map[int][]int{1: {5}, 2: {1, 2, 3}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ExistingEpisodes() = %#v, want %#v", got, want)
	}
	// 没有集数的媒体库返回空 map（不是 nil），调用方不必区分空与缺。
	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"Items":[],"TotalRecordCount":0,"StartIndex":0}`)
	}))
	t.Cleanup(empty.Close)
	emptyRuntime, _ := newMediaServerRuntimeWithEmbyServer(t, empty.URL, nil)
	if got, err := emptyRuntime.ExistingEpisodes(t.Context(), httpapi.RemoteMediaInfo{Path: "/library/Show"}); err != nil || len(got) != 0 || got == nil {
		t.Fatalf("空结果 = %#v %v", got, err)
	}
}

func TestMediaServerRuntimeMissingEpisodesIsEmpty(t *testing.T) {
	t.Parallel()

	runtime, _ := newMediaServerRuntimeWithEmbyServer(t, "https://emby.example.test", nil)
	missing, err := runtime.MissingEpisodes(t.Context(), httpapi.RemoteMediaInfo{Path: "/library/Show"})
	if err != nil || missing == nil || len(missing) != 0 {
		t.Fatalf("MissingEpisodes() = %#v %v", missing, err)
	}
}

func TestMediaIndexEmbyServerMapsItems(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		query := r.URL.Query()
		if r.URL.Path != "/Items" || query.Get("Path") != "/library/Show" ||
			query.Get("AnyProviderIdEquals") != "tmdb.7" || query.Get("Limit") != "2" ||
			query.Get("Recursive") != "true" || query.Get("Fields") != "Path,ProviderIds" {
			t.Errorf("unexpected request: %s?%s", r.URL.Path, r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"Items":[{"Id":"item-1","Type":"Episode","Path":"/library/Show/S01E01.mkv","ProviderIds":{"Tmdb":"7"}}],"TotalRecordCount":1}`)
	}))
	t.Cleanup(server.Close)

	runtime, setting := newMediaServerRuntimeWithEmbyServer(t, server.URL, nil)
	ref := mediaindex.ServerRef{
		ID: setting.ID, Name: setting.Name, Type: "emby",
		Fingerprint: mediaServerEndpointFingerprint(setting),
	}
	resolved, found := runtime.Resolve(ref)
	if !found {
		t.Fatal("Resolve() 未找到测试服务器")
	}
	items, err := resolved.FindItems(t.Context(), mediaindex.Query{Path: "/library/Show", ProviderID: "tmdb.7"})
	if err != nil {
		t.Fatalf("FindItems() error = %v", err)
	}
	want := []mediaindex.Item{{
		ID: "item-1", Type: "Episode", Path: "/library/Show/S01E01.mkv",
		ProviderIDs: map[string]string{"Tmdb": "7"},
	}}
	if !reflect.DeepEqual(items, want) {
		t.Fatalf("FindItems() = %#v, want %#v", items, want)
	}
	if calls.Load() != 1 {
		t.Fatalf("请求次数 = %d", calls.Load())
	}

	// 上游失败翻译成稳定的服务器错误，不泄漏响应正文。
	sensitive := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "secret upstream detail", http.StatusInternalServerError)
	}))
	t.Cleanup(sensitive.Close)
	failingRuntime, failingSetting := newMediaServerRuntimeWithEmbyServer(t, sensitive.URL, nil)
	failingRef := mediaindex.ServerRef{
		ID: failingSetting.ID, Name: failingSetting.Name, Type: "emby",
		Fingerprint: mediaServerEndpointFingerprint(failingSetting),
	}
	failingServer, found := failingRuntime.Resolve(failingRef)
	if !found {
		t.Fatal("Resolve() 未找到失败场景服务器")
	}
	if _, err := failingServer.FindItems(t.Context(), mediaindex.Query{Path: "/library/Show"}); !errors.Is(err, mediaindex.ErrServerUnavailable) {
		t.Fatalf("FindItems(上游 500) error = %v", err)
	} else if strings.Contains(err.Error(), "secret upstream detail") {
		t.Fatalf("错误泄漏上游正文: %v", err)
	}

	// 刷新成功为 nil、失败为稳定错误，且只发一次 POST。
	refresh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/Library/Refresh" {
			t.Errorf("unexpected refresh request: %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(refresh.Close)
	refreshRuntime, refreshSetting := newMediaServerRuntimeWithEmbyServer(t, refresh.URL, nil)
	refreshRef := mediaindex.ServerRef{
		ID: refreshSetting.ID, Name: refreshSetting.Name, Type: "emby",
		Fingerprint: mediaServerEndpointFingerprint(refreshSetting),
	}
	refreshServer, found := refreshRuntime.Resolve(refreshRef)
	if !found {
		t.Fatal("Resolve() 未找到刷新场景服务器")
	}
	if err := refreshServer.RefreshAll(t.Context()); err != nil {
		t.Fatalf("RefreshAll() error = %v", err)
	}

	failingRefresh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	t.Cleanup(failingRefresh.Close)
	brokenRuntime, brokenSetting := newMediaServerRuntimeWithEmbyServer(t, failingRefresh.URL, nil)
	brokenRef := mediaindex.ServerRef{
		ID: brokenSetting.ID, Name: brokenSetting.Name, Type: "emby",
		Fingerprint: mediaServerEndpointFingerprint(brokenSetting),
	}
	brokenServer, found := brokenRuntime.Resolve(brokenRef)
	if !found {
		t.Fatal("Resolve() 未找到失败刷新场景服务器")
	}
	if err := brokenServer.RefreshAll(t.Context()); !errors.Is(err, mediaindex.ErrServerUnavailable) {
		t.Fatalf("RefreshAll(上游 500) error = %v", err)
	}
}

func TestMediaIndexEmbyServerWithoutClient(t *testing.T) {
	t.Parallel()

	server := mediaIndexEmbyServer{}
	if err := server.RefreshAll(t.Context()); !errors.Is(err, mediaindex.ErrServerUnavailable) {
		t.Fatalf("RefreshAll() error = %v", err)
	}
	if _, err := server.FindItems(t.Context(), mediaindex.Query{}); !errors.Is(err, mediaindex.ErrServerUnavailable) {
		t.Fatalf("FindItems() error = %v", err)
	}
}

func TestTranslateEmbyErrorClasses(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		err       error
		wantClass error
		retryable *bool
	}{
		{name: "context.Canceled 原样保留", err: context.Canceled, wantClass: context.Canceled},
		{name: "包装后的超时原样保留", err: fmt.Errorf("wrap: %w", context.DeadlineExceeded), wantClass: context.DeadlineExceeded},
		{name: "刷新结果未知", err: emby.ErrOutcomeUnknown, wantClass: mediaindex.ErrOutcomeUnknown},
		{
			name: "非法配置不可重试", err: &emby.Error{Code: emby.ErrorCodeInvalidConfig},
			wantClass: mediaindex.ErrServerUnavailable, retryable: boolPtr(false),
		},
		{
			name: "上游状态可重试", err: &emby.Error{Code: emby.ErrorCodeHTTPStatus, StatusCode: 502},
			wantClass: mediaindex.ErrServerUnavailable, retryable: boolPtr(true),
		},
		{name: "其它错误折叠为服务器不可用", err: errors.New("boom"), wantClass: mediaindex.ErrServerUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			translated := translateEmbyError(tt.err)
			if !errors.Is(translated, tt.wantClass) {
				t.Fatalf("translateEmbyError(%v) = %v, want %v", tt.err, translated, tt.wantClass)
			}
			if tt.wantClass == context.Canceled || tt.wantClass == context.DeadlineExceeded {
				// 上下文错误必须保持同一实例，上层用 errors.Is 判断可重试性。
				if !errors.Is(translated, tt.wantClass) {
					t.Fatalf("上下文错误被替换: %v", translated)
				}
				return
			}
			var serverErr *mediaindex.ServerError
			hasServerError := errors.As(translated, &serverErr)
			if tt.retryable != nil {
				if !hasServerError {
					t.Fatalf("expected *mediaindex.ServerError, got %T", translated)
				}
				if serverErr.Retryable != *tt.retryable {
					t.Fatalf("Retryable = %v, want %v", serverErr.Retryable, *tt.retryable)
				}
			}
			if translated.Error() != tt.wantClass.Error() {
				t.Fatalf("错误文案 = %q", translated.Error())
			}
		})
	}
}

func TestMediaServerRuntimePureHelpers(t *testing.T) {
	t.Parallel()

	if got, want := embyAuthorization("device-1"), `Emby Client="MoviePilot-Go", Device="MoviePilot-Go", DeviceId="device-1", Version="0.1"`; got != want {
		t.Fatalf("embyAuthorization() = %q, want %q", got, want)
	}

	tmdbIDs := map[int64]string{0: "", -1: "", 42: "tmdb.42"}
	for id, want := range tmdbIDs {
		if got := tmdbProviderID(id); got != want {
			t.Fatalf("tmdbProviderID(%d) = %q, want %q", id, got, want)
		}
	}

	videoTargets := []string{
		"a.mkv", "a.MP4", "a.avi", "a.mov", "a.ts", "a.m2ts", "a.wmv", "a.flv", "a.webm", "a.iso",
	}
	for _, path := range videoTargets {
		if !isVideoTarget(path) {
			t.Fatalf("isVideoTarget(%q) = false, want true", path)
		}
	}
	nonVideoTargets := []string{"a.nfo", "a.jpg", "a", "a.mkvv", "dir/a.mkv/", ""}
	for _, path := range nonVideoTargets {
		if isVideoTarget(path) {
			t.Fatalf("isVideoTarget(%q) = true, want false", path)
		}
	}

	if got := cloneStringMap(nil); got != nil {
		t.Fatalf("cloneStringMap(nil) = %#v", got)
	}
	source := map[string]string{"Tmdb": "7"}
	cloned := cloneStringMap(source)
	if !reflect.DeepEqual(cloned, source) {
		t.Fatalf("cloneStringMap() = %#v", cloned)
	}
	cloned["Tmdb"] = "9"
	if source["Tmdb"] != "7" {
		t.Fatal("cloneStringMap 返回了共享 map")
	}
}

func TestMediaServerRuntimeCloseIdleConnectionsIsIdempotent(t *testing.T) {
	t.Parallel()

	var nilRuntime *mediaServerRuntime
	nilRuntime.CloseIdleConnections()

	runtime, _ := newMediaServerRuntimeWithEmbyServer(t, "https://emby.example.test", nil)
	runtime.CloseIdleConnections()
	runtime.CloseIdleConnections()
	if got := runtime.ListMediaServerClients(t.Context()); len(got) != 0 {
		t.Fatalf("关闭后客户端 = %#v", got)
	}
	if runtime.entriesSnapshot("") != nil && len(runtime.entriesSnapshot("")) != 0 {
		t.Fatal("关闭后 entries 未清空")
	}
}

func TestMediaServerRuntimeCompletionPlanStopsWhenContextCancelledMidLoop(t *testing.T) {
	t.Parallel()

	runtime := newMediaServerRuntimeCompletionPlanTest(t, []mediaconfig.Server{
		mediaServerRuntimeTestServer([]mediaconfig.PathMapping{{LocalRoot: "/mnt/media", EmbyRoot: "/library"}}),
	})
	plan := organizer.Plan{Operations: []organizer.Operation{{
		Target: "/mnt/media/Movie.mkv", ExpectedSize: 1024, ExpectedSHA256: strings.Repeat("a", 64),
	}}}
	// 入口检查时还没取消，进入逐文件循环后才取消：规划必须立刻停，不能把剩余文件
	// 当成「已确认」交出去。
	ctx := &cancelAfterFirstCheckContext{Context: context.Background()}
	if _, err := runtime.MediaIndexCompletionPlan(ctx, plan); !errors.Is(err, context.Canceled) {
		t.Fatalf("MediaIndexCompletionPlan() error = %v, want context.Canceled", err)
	}
}

// cancelAfterFirstCheckContext 的第一次 Err() 返回 nil，之后返回 context.Canceled，
// 用来稳定复现「循环中途取消」这一分支。
type cancelAfterFirstCheckContext struct {
	context.Context
	calls atomic.Int32
}

func (c *cancelAfterFirstCheckContext) Err() error {
	if c.calls.Add(1) == 1 {
		return nil
	}
	return context.Canceled
}

func TestMediaServerRuntimeEntriesSnapshotSkipsBrokenEntries(t *testing.T) {
	t.Parallel()

	setting := mediaServerRuntimeTestServer(nil)
	runtime := newMediaServerRuntimeCompletionPlanTest(t, []mediaconfig.Server{setting})
	runtime.mu.Lock()
	runtime.entries = append(runtime.entries, nil, &mediaServerEntry{setting: setting})
	runtime.mu.Unlock()

	// 半装配的条目（nil 或没有客户端）不能出现在客户端列表里。
	clients := runtime.ListMediaServerClients(t.Context())
	if len(clients) != 1 || clients[0].Name != setting.Name {
		t.Fatalf("ListMediaServerClients() = %#v", clients)
	}
	if entries := runtime.entriesSnapshot(""); len(entries) != 1 {
		t.Fatalf("entriesSnapshot() = %d, want 1", len(entries))
	}
}

func boolPtr(value bool) *bool { return &value }

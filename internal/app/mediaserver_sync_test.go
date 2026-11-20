package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/moviepilot-go/moviepilot-go/internal/mediaconfig"
	"github.com/moviepilot-go/moviepilot-go/internal/mediaserver"
	"github.com/moviepilot-go/moviepilot-go/internal/mediaserver/emby"
	"github.com/moviepilot-go/moviepilot-go/internal/subscriptionflow"
)

type mediaServerStaleCall struct {
	server   string
	syncTime string
}

// mediaServerItemStoreFake records the cache writes and maintenance calls the
// sync task performs.
type mediaServerItemStoreFake struct {
	mu              sync.Mutex
	upserts         []mediaserver.Item
	upsertErr       error
	excluded        [][]string
	excludedDeleted int64
	excludedErr     error
	staleCalls      []mediaServerStaleCall
	staleDeleted    int64
	staleErr        error
}

func (f *mediaServerItemStoreFake) UpsertMediaServerItem(_ context.Context, item mediaserver.Item) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.upsertErr != nil {
		return f.upsertErr
	}
	f.upserts = append(f.upserts, item)
	return nil
}

func (f *mediaServerItemStoreFake) DeleteStaleMediaServerItems(_ context.Context, server, syncTime string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.staleCalls = append(f.staleCalls, mediaServerStaleCall{server: server, syncTime: syncTime})
	return f.staleDeleted, f.staleErr
}

func (f *mediaServerItemStoreFake) DeleteExcludedMediaServerItems(_ context.Context, servers []string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.excluded = append(f.excluded, append([]string(nil), servers...))
	return f.excludedDeleted, f.excludedErr
}

func (f *mediaServerItemStoreFake) upserted() []mediaserver.Item {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]mediaserver.Item(nil), f.upserts...)
}

func (f *mediaServerItemStoreFake) stale() []mediaServerStaleCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]mediaServerStaleCall(nil), f.staleCalls...)
}

func (f *mediaServerItemStoreFake) excludedServers() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	result := make([][]string, 0, len(f.excluded))
	for _, entry := range f.excluded {
		result = append(result, append([]string(nil), entry...))
	}
	return result
}

type mediaServerItemLookupFake struct {
	mu      sync.Mutex
	itemID  string
	found   bool
	err     error
	queries []mediaserver.Query
}

func (f *mediaServerItemLookupFake) FindMediaServerItemID(_ context.Context, query mediaserver.Query) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queries = append(f.queries, query)
	if f.err != nil {
		return "", false, f.err
	}
	return f.itemID, f.found, nil
}

func (f *mediaServerItemLookupFake) recorded() []mediaserver.Query {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]mediaserver.Query(nil), f.queries...)
}

// newMediaServerSyncFixture serves the user, view, folder and episode calls V2
// issues while syncing one server. failFolder makes the nested folder listing
// fail so the enumeration cannot be treated as complete.
func newMediaServerSyncFixture(t *testing.T, failFolder bool) (string, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	var itemCalls atomic.Int32
	var userCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/Users":
			userCalls.Add(1)
			_, _ = io.WriteString(w, `[{"Id":"user-1","Name":"admin","Policy":{"IsAdministrator":true}}]`)
		case r.URL.Path == "/Users/user-1/Views":
			_, _ = io.WriteString(w, `{"Items":[`+
				`{"Id":"lib-1","Name":"Movies","CollectionType":"movies"},`+
				`{"Id":"lib-2","Name":"Shows","CollectionType":"tvshows"}]}`)
		case r.URL.Path == "/Users/user-1/Items":
			itemCalls.Add(1)
			switch r.URL.Query().Get("ParentId") {
			case "lib-1":
				_, _ = io.WriteString(w, `{"Items":[`+
					`{"Id":"folder-1","Type":"Folder","Name":"A"},`+
					`{"Id":"movie-1","Type":"Movie","Name":"Example Movie","OriginalTitle":"Original",`+
					`"ProductionYear":2025,"ParentId":"lib-1","Path":"/media/movie-1.mkv",`+
					`"ProviderIds":{"Tmdb":"123","Imdb":"tt1"}},`+
					`{"Id":"unparsable-1","Type":"Movie","Name":"Broken","ParentId":"lib-1",`+
					`"ProviderIds":{"Tmdb":"not-a-number"}}`+
					`],"TotalRecordCount":3,"StartIndex":0}`)
			case "folder-1":
				if failFolder {
					http.Error(w, "library unavailable", http.StatusInternalServerError)
					return
				}
				_, _ = io.WriteString(w, `{"Items":[{"Id":"movie-2","Type":"Movie","Name":"Nested",`+
					`"ProductionYear":2024,"ParentId":"folder-1","ProviderIds":{"Tmdb":"456"}}],`+
					`"TotalRecordCount":1,"StartIndex":0}`)
			case "lib-2":
				_, _ = io.WriteString(w, `{"Items":[{"Id":"series-1","Type":"Series","Name":"Show",`+
					`"ProductionYear":2026,"ParentId":"lib-2","ProviderIds":{"Tmdb":"789","Tvdb":"tv1"}}],`+
					`"TotalRecordCount":1,"StartIndex":0}`)
			default:
				http.NotFound(w, r)
			}
		case r.URL.Path == "/Shows/series-1/Episodes":
			_, _ = io.WriteString(w, `{"Items":[{"ParentIndexNumber":1,"IndexNumber":2},`+
				`{"ParentIndexNumber":1,"IndexNumber":1},{"IndexNumber":3}],`+
				`"TotalRecordCount":3,"StartIndex":0}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server.URL, &itemCalls, &userCalls
}

func newMediaServerSyncRuntime(
	t *testing.T,
	host string,
	store mediaserver.ItemStore,
	lookup mediaserver.ItemLookup,
) (*mediaServerRuntime, time.Time) {
	t.Helper()
	setting := mediaServerRuntimeTestServer(nil)
	setting.Config.Host = host
	runtime := newMediaServerRuntimeCompletionPlanTest(t, []mediaconfig.Server{setting})
	fixed := time.Date(2026, 9, 29, 12, 0, 0, 250_000_000, time.UTC)
	runtime.now = func() time.Time { return fixed }
	if lookup == nil {
		lookup = &mediaServerItemLookupFake{}
	}
	if err := runtime.SetItemCache(store, lookup, "admin"); err != nil {
		t.Fatalf("SetItemCache() error = %v", err)
	}
	return runtime, fixed
}

func TestMediaServerRuntimeSyncMediaServerItemsWritesV2CacheRows(t *testing.T) {
	t.Parallel()
	host, itemCalls, userCalls := newMediaServerSyncFixture(t, false)
	store := &mediaServerItemStoreFake{}
	runtime, fixed := newMediaServerSyncRuntime(t, host, store, nil)

	if err := runtime.SyncMediaServerItems(t.Context(), ""); err != nil {
		t.Fatalf("SyncMediaServerItems() error = %v", err)
	}

	upserts := store.upserted()
	if len(upserts) != 3 {
		t.Fatalf("upserts = %d, want 3 (%#v)", len(upserts), upserts)
	}
	syncTime := fixed.Format(mediaServerSyncTimeLayout)
	byID := make(map[string]mediaserver.Item, len(upserts))
	for _, item := range upserts {
		byID[item.ItemID] = item
		if item.Server != "Fixture Emby" || item.LstModDate != syncTime {
			t.Errorf("item %s = (%q, %q), want server Fixture Emby and the run stamp",
				item.ItemID, item.Server, item.LstModDate)
		}
	}
	movie, ok := byID["movie-1"]
	if !ok {
		t.Fatalf("movie-1 was not cached: %#v", upserts)
	}
	if movie.ItemType != mediaserver.ItemTypeMovie || movie.Title != "Example Movie" ||
		movie.OriginalTitle != "Original" || movie.Year != "2025" || movie.TMDBID != 123 ||
		movie.Library != "lib-1" || movie.Path != "/media/movie-1.mkv" || movie.IMDBID != "tt1" {
		t.Errorf("movie-1 = %#v", movie)
	}
	if item, ok := byID["unparsable-1"]; ok {
		t.Errorf("an unparsable TMDB provider ID must skip the row like V2's formatter: %#v", item)
	}
	nested, ok := byID["movie-2"]
	if !ok || nested.Library != "folder-1" {
		t.Fatalf("nested folder item = (%#v, %v), want cached with its folder parent", nested, ok)
	}
	series, ok := byID["series-1"]
	if !ok {
		t.Fatalf("series-1 was not cached: %#v", upserts)
	}
	if series.ItemType != mediaserver.ItemTypeTV || series.TVDBID != "tv1" ||
		len(series.SeasonInfo) != 1 || len(series.SeasonInfo[1]) != 2 ||
		series.SeasonInfo[1][0] != 1 || series.SeasonInfo[1][1] != 2 {
		t.Errorf("series-1 = %#v", series)
	}

	stale := store.stale()
	if len(stale) != 1 || stale[0].server != "Fixture Emby" || stale[0].syncTime != syncTime {
		t.Fatalf("stale calls = %#v, want one call with the run stamp", stale)
	}
	excluded := store.excludedServers()
	if len(excluded) != 1 || len(excluded[0]) != 1 || excluded[0][0] != "Fixture Emby" {
		t.Fatalf("excluded servers = %#v, want the enabled server name", excluded)
	}
	if itemCalls.Load() != 3 || userCalls.Load() != 1 {
		t.Fatalf("item calls = %d, user calls = %d, want 3 and 1", itemCalls.Load(), userCalls.Load())
	}
}

func TestMediaServerRuntimeSyncMediaServerItemsKeepsCacheWhenIncomplete(t *testing.T) {
	t.Parallel()
	host, _, _ := newMediaServerSyncFixture(t, true)
	store := &mediaServerItemStoreFake{}
	runtime, _ := newMediaServerSyncRuntime(t, host, store, nil)

	err := runtime.SyncMediaServerItems(t.Context(), "")
	if !errors.Is(err, errMediaServerItemSyncFailed) {
		t.Fatalf("SyncMediaServerItems() error = %v, want %v", err, errMediaServerItemSyncFailed)
	}
	if upserts := store.upserted(); len(upserts) == 0 {
		t.Error("a partially enumerated library must keep the rows it already wrote")
	}
	if stale := store.stale(); len(stale) != 0 {
		t.Fatalf("stale calls = %#v, want none after an incomplete enumeration", stale)
	}
}

func TestMediaServerRuntimeSyncMediaServerItemsRequiresResolvedUser(t *testing.T) {
	t.Parallel()
	var viewsCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/Users" {
			_, _ = io.WriteString(w, `[]`)
			return
		}
		viewsCalls.Add(1)
		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)
	store := &mediaServerItemStoreFake{}
	setting := mediaServerRuntimeTestServer(nil)
	setting.Config.Host = server.URL
	setting.Config.Username = ""
	runtime := newMediaServerRuntimeCompletionPlanTest(t, []mediaconfig.Server{setting})
	if err := runtime.SetItemCache(store, &mediaServerItemLookupFake{}, ""); err != nil {
		t.Fatalf("SetItemCache() error = %v", err)
	}

	if err := runtime.SyncMediaServerItems(t.Context(), ""); !errors.Is(err, errMediaServerItemSyncFailed) {
		t.Fatalf("SyncMediaServerItems() error = %v, want %v", err, errMediaServerItemSyncFailed)
	}
	if viewsCalls.Load() != 0 {
		t.Fatalf("library views calls = %d, want 0 without a resolved user", viewsCalls.Load())
	}
	if stale := store.stale(); len(stale) != 0 {
		t.Fatalf("stale calls = %#v, want none without a resolved user", stale)
	}
}

func TestMediaServerRuntimeSyncMediaServerItemsHonoursSyncLibraries(t *testing.T) {
	t.Parallel()
	host, _, _ := newMediaServerSyncFixture(t, false)
	store := &mediaServerItemStoreFake{}
	setting := mediaServerRuntimeTestServer(nil)
	setting.Config.Host = host
	setting.SyncLibraries = []string{"lib-2"}
	runtime := newMediaServerRuntimeCompletionPlanTest(t, []mediaconfig.Server{setting})
	runtime.now = func() time.Time { return time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC) }
	if err := runtime.SetItemCache(store, &mediaServerItemLookupFake{}, "admin"); err != nil {
		t.Fatalf("SetItemCache() error = %v", err)
	}

	if err := runtime.SyncMediaServerItems(t.Context(), ""); err != nil {
		t.Fatalf("SyncMediaServerItems() error = %v", err)
	}
	upserts := store.upserted()
	if len(upserts) != 1 || upserts[0].ItemID != "series-1" {
		t.Fatalf("upserts = %#v, want only the selected library item", upserts)
	}
	if stale := store.stale(); len(stale) != 1 {
		t.Fatalf("stale calls = %#v, want one call for the selected library run", stale)
	}
}

func TestMediaServerRuntimeSyncMediaServerItemsWithoutServersSkipsStore(t *testing.T) {
	t.Parallel()
	store := &mediaServerItemStoreFake{}
	runtime := &mediaServerRuntime{now: time.Now}
	if err := runtime.SetItemCache(store, &mediaServerItemLookupFake{}, "admin"); err != nil {
		t.Fatalf("SetItemCache() error = %v", err)
	}
	if err := runtime.SyncMediaServerItems(t.Context(), ""); err != nil {
		t.Fatalf("SyncMediaServerItems() error = %v", err)
	}
	if len(store.excludedServers()) != 0 || len(store.stale()) != 0 {
		t.Fatal("a run without enabled servers must not touch the cache")
	}
	if err := runtime.SyncMediaServerItems(nil, ""); !errors.Is(err, errMediaServerItemSyncUnavailable) {
		t.Fatalf("SyncMediaServerItems(nil) error = %v, want %v", err, errMediaServerItemSyncUnavailable)
	}
	if err := (&mediaServerRuntime{}).SetItemCache(nil, &mediaServerItemLookupFake{}, ""); !errors.Is(err, errMediaServerItemSyncUnavailable) {
		t.Fatalf("SetItemCache(nil) error = %v, want %v", err, errMediaServerItemSyncUnavailable)
	}
	client, err := emby.NewClient(emby.Config{BaseURL: "https://emby.example.test", Token: "tok", Timeout: time.Second})
	if err != nil {
		t.Fatalf("emby.NewClient() error = %v", err)
	}
	t.Cleanup(client.CloseIdleConnections)
	withoutStore := &mediaServerRuntime{
		now: time.Now,
		entries: []*mediaServerEntry{{
			setting: mediaconfig.Server{Name: "Fixture Emby", Enabled: true},
			client:  client,
		}},
	}
	if err := withoutStore.SyncMediaServerItems(t.Context(), ""); !errors.Is(err, errMediaServerItemSyncUnavailable) {
		t.Fatalf("SyncMediaServerItems() without a cache error = %v, want %v", err, errMediaServerItemSyncUnavailable)
	}
}

func TestMediaServerRuntimeSyncMediaServerItemsPropagatesCancellation(t *testing.T) {
	t.Parallel()
	host, _, _ := newMediaServerSyncFixture(t, false)
	store := &mediaServerItemStoreFake{}
	runtime, _ := newMediaServerSyncRuntime(t, host, store, nil)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if err := runtime.SyncMediaServerItems(ctx, ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("SyncMediaServerItems() error = %v, want context.Canceled", err)
	}
	if len(store.stale()) != 0 {
		t.Fatal("a cancelled run must not run stale deletion")
	}
}

func newMediaServerMovieFastPathRuntime(
	t *testing.T,
	lookup mediaserver.ItemLookup,
	hosts ...string,
) *mediaServerRuntime {
	t.Helper()
	servers := make([]mediaconfig.Server, 0, len(hosts))
	for index, host := range hosts {
		setting := mediaServerRuntimeTestServer(nil)
		setting.ID = fmt.Sprintf("2a46de31-c7bc-42d3-b65f-5691e9478c%02d", index)
		setting.Name = fmt.Sprintf("Fixture Emby %d", index+1)
		setting.Config.Host = host
		servers = append(servers, setting)
	}
	runtime := newMediaServerRuntimeCompletionPlanTest(t, servers)
	if err := runtime.SetItemCache(&mediaServerItemStoreFake{}, lookup, "admin"); err != nil {
		t.Fatalf("SetItemCache() error = %v", err)
	}
	return runtime
}

func TestMediaServerRuntimeFindExistingMovieUsesCachedItemFastPath(t *testing.T) {
	t.Parallel()
	var detailCalls atomic.Int32
	var searchCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/Users":
			_, _ = io.WriteString(w, `[{"Id":"user-1","Name":"admin","Policy":{"IsAdministrator":true}}]`)
		case "/Users/user-1/Items/item-9":
			detailCalls.Add(1)
			// V2 accepts any formattable object from the cached ID and never
			// re-validates title, year or provider IDs.
			_, _ = io.WriteString(w, `{"Id":"item-9","Name":"Some Other Title"}`)
		default:
			searchCalls.Add(1)
			_, _ = io.WriteString(w, `{"Items":[],"TotalRecordCount":0,"StartIndex":0}`)
		}
	}))
	t.Cleanup(server.Close)
	lookup := &mediaServerItemLookupFake{itemID: "item-9", found: true}
	runtime := newMediaServerMovieFastPathRuntime(t, lookup, server.URL)

	found, err := runtime.FindExistingMovie(t.Context(), subscriptionflow.ExistingMovieQuery{
		Title: "Example Movie", Year: "2026", MediaSource: "tmdb", MediaID: "123",
	})
	if err != nil || !found {
		t.Fatalf("FindExistingMovie() = (%v, %v), want true", found, err)
	}
	if detailCalls.Load() != 1 {
		t.Fatalf("detail calls = %d, want 1", detailCalls.Load())
	}
	if searchCalls.Load() != 0 {
		t.Fatalf("title search calls = %d, want 0 after a cache hit", searchCalls.Load())
	}
	queries := lookup.recorded()
	want := mediaserver.Query{TMDBID: 123, ItemType: mediaserver.ItemTypeMovie, Title: "Example Movie"}
	if len(queries) != 1 || queries[0] != want {
		t.Fatalf("cache queries = %#v, want one V2 movie lookup %#v", queries, want)
	}
}

func TestMediaServerRuntimeFindExistingMovieFallsBackAfterCachedItemMiss(t *testing.T) {
	t.Parallel()
	matched := `{"Items":[{"Name":"Example Movie","ProductionYear":2026,` +
		`"ProviderIds":{"Tmdb":"123"}}],"TotalRecordCount":1,"StartIndex":0}`
	tests := []struct {
		name      string
		detail    int
		search    string
		wantFound bool
	}{
		{name: "404 detail falls back to the title search", detail: http.StatusNotFound,
			search: matched, wantFound: true},
		{name: "detail failure falls back to the title search", detail: http.StatusInternalServerError,
			search: matched, wantFound: true},
		{name: "empty search result is a miss", detail: http.StatusNotFound,
			search: `{"Items":[],"TotalRecordCount":0,"StartIndex":0}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var detailCalls atomic.Int32
			var searchCalls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/Users":
					_, _ = io.WriteString(w, `[{"Id":"user-1","Policy":{"IsAdministrator":true}}]`)
				case "/Users/user-1/Items/item-9":
					detailCalls.Add(1)
					w.WriteHeader(tt.detail)
				default:
					searchCalls.Add(1)
					_, _ = io.WriteString(w, tt.search)
				}
			}))
			t.Cleanup(server.Close)
			runtime := newMediaServerMovieFastPathRuntime(t,
				&mediaServerItemLookupFake{itemID: "item-9", found: true}, server.URL)

			found, err := runtime.FindExistingMovie(t.Context(), subscriptionflow.ExistingMovieQuery{
				Title: "Example Movie", Year: "2026", MediaSource: "tmdb", MediaID: "123",
			})
			if err != nil || found != tt.wantFound {
				t.Fatalf("FindExistingMovie() = (%v, %v), want %v", found, err, tt.wantFound)
			}
			if detailCalls.Load() != 1 || searchCalls.Load() != 1 {
				t.Fatalf("detail calls = %d, search calls = %d, want 1 and 1",
					detailCalls.Load(), searchCalls.Load())
			}
		})
	}
}

func TestMediaServerRuntimeFindExistingMovieBroadcastsCachedIDInServerOrder(t *testing.T) {
	t.Parallel()
	var firstSearchCalls atomic.Int32
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/Users":
			_, _ = io.WriteString(w, `[{"Id":"user-1","Policy":{"IsAdministrator":true}}]`)
		case "/Users/user-1/Items/item-9":
			http.NotFound(w, r)
		default:
			firstSearchCalls.Add(1)
			_, _ = io.WriteString(w, `{"Items":[],"TotalRecordCount":0,"StartIndex":0}`)
		}
	}))
	t.Cleanup(first.Close)
	var secondSearchCalls atomic.Int32
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/Users":
			_, _ = io.WriteString(w, `[{"Id":"user-2","Policy":{"IsAdministrator":true}}]`)
		case "/Users/user-2/Items/item-9":
			_, _ = io.WriteString(w, `{"Id":"item-9","Name":"Second Server Copy"}`)
		default:
			secondSearchCalls.Add(1)
			_, _ = io.WriteString(w, `{"Items":[],"TotalRecordCount":0,"StartIndex":0}`)
		}
	}))
	t.Cleanup(second.Close)

	runtime := newMediaServerMovieFastPathRuntime(t,
		&mediaServerItemLookupFake{itemID: "item-9", found: true}, first.URL, second.URL)
	found, err := runtime.FindExistingMovie(t.Context(), subscriptionflow.ExistingMovieQuery{
		Title: "Example Movie", Year: "2026", MediaSource: "tmdb", MediaID: "123",
	})
	if err != nil || !found {
		t.Fatalf("FindExistingMovie() = (%v, %v), want true", found, err)
	}
	if firstSearchCalls.Load() != 1 {
		t.Fatalf("first server search calls = %d, want the V2 per-server fallback", firstSearchCalls.Load())
	}
	if secondSearchCalls.Load() != 0 {
		t.Fatalf("second server search calls = %d, want 0 after the detail hit", secondSearchCalls.Load())
	}
}

func TestMediaServerRuntimeFindExistingMovieCacheLookupFailureFallsOpen(t *testing.T) {
	t.Parallel()
	var searchCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/Users":
			_, _ = io.WriteString(w, `[{"Id":"user-1","Policy":{"IsAdministrator":true}}]`)
		case "/Items":
			searchCalls.Add(1)
			_, _ = io.WriteString(w, `{"Items":[{"Name":"Example Movie","ProductionYear":2026,`+
				`"ProviderIds":{"Tmdb":"123"}}],"TotalRecordCount":1,"StartIndex":0}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	tests := []struct {
		name   string
		lookup mediaserver.ItemLookup
	}{
		{name: "cache read failure", lookup: &mediaServerItemLookupFake{err: mediaserver.ErrItemLookup}},
		{name: "cache miss", lookup: &mediaServerItemLookupFake{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			runtime := newMediaServerMovieFastPathRuntime(t, tt.lookup, server.URL)
			found, err := runtime.FindExistingMovie(t.Context(), subscriptionflow.ExistingMovieQuery{
				Title: "Example Movie", Year: "2026", MediaSource: "tmdb", MediaID: "123",
			})
			if err != nil || !found {
				t.Fatalf("FindExistingMovie() = (%v, %v), want the live fallback to succeed", found, err)
			}
		})
	}
}

func TestMediaServerCachedRowHelpers(t *testing.T) {
	t.Parallel()
	if itemType, ok := mediaServerItemType("Series"); !ok || itemType != mediaserver.ItemTypeTV {
		t.Errorf("mediaServerItemType(Series) = (%q, %v)", itemType, ok)
	}
	if itemType, ok := mediaServerItemType("Movie"); !ok || itemType != mediaserver.ItemTypeMovie {
		t.Errorf("mediaServerItemType(Movie) = (%q, %v)", itemType, ok)
	}
	if _, ok := mediaServerItemType("Episode"); ok {
		t.Error("mediaServerItemType(Episode) must not cache episode rows")
	}
	if tmdbID, ok := mediaServerTMDBID(map[string]string{"tmdb": "42"}); !ok || tmdbID != 42 {
		t.Errorf("mediaServerTMDBID() = (%d, %v), want (42, true)", tmdbID, ok)
	}
	if tmdbID, ok := mediaServerTMDBID(map[string]string{"TMDB": " 42 "}); !ok || tmdbID != 42 {
		t.Errorf("mediaServerTMDBID() = (%d, %v), want (42, true)", tmdbID, ok)
	}
	if _, ok := mediaServerTMDBID(map[string]string{"tmdb": "abc"}); ok {
		t.Error("mediaServerTMDBID() must reject values V2 cannot convert with int()")
	}
	if tmdbID, ok := mediaServerTMDBID(nil); !ok || tmdbID != 0 {
		t.Errorf("mediaServerTMDBID(nil) = (%d, %v), want (0, true)", tmdbID, ok)
	}

	views := []emby.UserView{{ID: "lib-1"}, {ID: "lib-2"}, {ID: ""}}
	if selected := selectMediaServerSyncLibraries(views, nil); len(selected) != 2 {
		t.Errorf("selectMediaServerSyncLibraries(nil) = %#v, want every identified library", selected)
	}
	if selected := selectMediaServerSyncLibraries(views, []string{"all"}); len(selected) != 2 {
		t.Errorf("selectMediaServerSyncLibraries(all) = %#v, want every identified library", selected)
	}
	selected := selectMediaServerSyncLibraries(views, []string{"lib-2"})
	if len(selected) != 1 || selected[0].ID != "lib-2" {
		t.Errorf("selectMediaServerSyncLibraries(lib-2) = %#v", selected)
	}
	if selected := selectMediaServerSyncLibraries(nil, []string{"lib-1"}); selected != nil {
		t.Errorf("selectMediaServerSyncLibraries(nil views) = %#v, want nil", selected)
	}
}

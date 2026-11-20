package metadata

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestTMDBSupplementerMapsFourPrimarySourcesWithOfflineFixture(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/3/search/multi":
			_, _ = w.Write([]byte(`{"results":[{"id":42,"media_type":"tv","name":"示例动画","original_name":"Example Anime","first_air_date":"2026-01-01","genre_ids":[16],"origin_country":["JP"]}]}`))
		case "/3/tv/42":
			_, _ = w.Write([]byte(`{"id":42,"name":"示例动画","original_name":"Example Anime","original_language":"ja","first_air_date":"2026-01-01","genres":[{"id":16}],"origin_country":["JP"],"seasons":[{"season_number":1,"episode_count":12,"air_date":"2026-01-01"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	supplementer := newTMDBSupplementerForTest(t, server.URL)

	for _, source := range []string{"douban", "bangumi", "anilist", "custom"} {
		t.Run(source, func(t *testing.T) {
			primary := Media{
				Source: source, ID: source + "-7", Type: "tv", Title: "示例动画", Year: 2026,
				Overview: "主源简介", PosterURL: "https://primary.test/poster.jpg", Aliases: []string{"主源别名"},
			}
			got, found, err := supplementer.Supplement(t.Context(), TMDBSupplementInput{Media: primary})
			if err != nil || !found {
				t.Fatalf("Supplement() found=%v err=%v", found, err)
			}
			if got.Source != primary.Source || got.ID != primary.ID || got.Title != primary.Title ||
				got.Overview != primary.Overview || got.PosterURL != primary.PosterURL || !reflect.DeepEqual(got.Aliases, primary.Aliases) {
				t.Fatalf("主源身份或展示字段被覆盖: %#v", got)
			}
			if got.TMDBID != "42" || got.OriginalLanguage != "ja" || !reflect.DeepEqual(got.GenreIDs, []int{16}) ||
				!reflect.DeepEqual(got.OriginCountries, []string{"JP"}) || len(got.SeasonInfo) != 1 {
				t.Fatalf("TMDB 辅助事实未合并: %#v", got)
			}
		})
	}
}

func TestTMDBSupplementerUsesExactNamesYearAndSeasonTiers(t *testing.T) {
	t.Parallel()
	var detailCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/3/search/multi":
			_, _ = w.Write([]byte(`{"results":[
				{"id":1,"media_type":"tv","name":"Other","first_air_date":"2026-01-01"},
				{"id":2,"media_type":"tv","name":"Show Name","first_air_date":"2026-01-01"},
				{"id":3,"media_type":"tv","name":"Show-Name","first_air_date":"2024-01-01"}
			]}`))
		case "/3/tv/2":
			detailCalls.Add(1)
			_, _ = w.Write([]byte(`{"id":2,"name":"Show Name","first_air_date":"2026-01-01","genres":[{"id":18}],"seasons":[{"season_number":2,"episode_count":8,"air_date":"2027-03-01"}]}`))
		case "/3/tv/3":
			detailCalls.Add(1)
			_, _ = w.Write([]byte(`{"id":3,"name":"Show-Name","first_air_date":"2024-01-01","genres":[{"id":16}],"seasons":[{"season_number":2,"episode_count":8,"air_date":"2026-03-01"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	supplementer := newTMDBSupplementerForTest(t, server.URL)
	season := 2
	got, found, err := supplementer.Supplement(t.Context(), TMDBSupplementInput{
		Media:  Media{Source: "bangumi", ID: "9", Type: "tv", Title: "Show.Name", Year: 2024},
		Season: &season, SeasonYear: 2026,
	})
	if err != nil || !found || got.TMDBID != "2" || !reflect.DeepEqual(got.GenreIDs, []int{18}) {
		t.Fatalf("季年份消歧 = %#v found=%v err=%v", got, found, err)
	}
	if detailCalls.Load() != 1 {
		t.Fatalf("详情调用 = %d, want 1", detailCalls.Load())
	}
}

func TestTMDBSupplementerMissAndFailureLeavePrimaryUnchanged(t *testing.T) {
	t.Parallel()
	primary := Media{Source: "douban", ID: "7", Type: "movie", Title: "示例", GenreIDs: []int{35}}
	for _, test := range []struct {
		name       string
		status     int
		body       string
		wantError  bool
		wantSearch int32
	}{
		{name: "无精确名称匹配", status: http.StatusOK, body: `{"results":[{"id":8,"media_type":"movie","title":"另一个标题"}]}`, wantSearch: 1},
		{name: "TMDB 故障", status: http.StatusBadGateway, body: `failure`, wantError: true, wantSearch: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(test.status)
				_, _ = w.Write([]byte(test.body))
			}))
			t.Cleanup(server.Close)
			supplementer := newTMDBSupplementerForTest(t, server.URL)
			got, found, err := supplementer.Supplement(t.Context(), TMDBSupplementInput{Media: primary})
			if (err != nil) != test.wantError || found || !reflect.DeepEqual(got, primary) {
				t.Fatalf("got=%#v found=%v err=%v", got, found, err)
			}
			if calls.Load() != test.wantSearch {
				t.Fatalf("请求数 = %d, want %d", calls.Load(), test.wantSearch)
			}
		})
	}
}

func TestTMDBSupplementerPreservesExistingPrimaryFacts(t *testing.T) {
	t.Parallel()
	var searchCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/search/") {
			searchCalls.Add(1)
			_, _ = w.Write([]byte(`{"results":[{"id":5,"media_type":"movie","title":"Movie","release_date":"2025-01-01"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":5,"title":"Movie","original_language":"en","genres":[{"id":16}],"production_countries":[{"iso_3166_1":"US"}]}`))
	}))
	t.Cleanup(server.Close)
	supplementer := newTMDBSupplementerForTest(t, server.URL)
	primary := Media{Source: "douban", ID: "d5", TMDBID: "5", Type: "movie", Title: "Movie", Year: 2025,
		OriginalLanguage: "zh", GenreIDs: []int{35}, ProductionCountries: []string{"CN"}}
	got, found, err := supplementer.Supplement(t.Context(), TMDBSupplementInput{Media: primary})
	if err != nil || !found {
		t.Fatalf("found=%v err=%v", found, err)
	}
	if got.OriginalLanguage != "zh" || !reflect.DeepEqual(got.GenreIDs, []int{35}) ||
		!reflect.DeepEqual(got.ProductionCountries, []string{"CN"}) {
		t.Fatalf("已有主源事实被覆盖: %#v", got)
	}
	if searchCalls.Load() != 0 {
		t.Fatalf("已有 TMDB ID 仍触发搜索: %d", searchCalls.Load())
	}
}

func TestNewTMDBSupplementerRequiresTMDBProvider(t *testing.T) {
	t.Parallel()
	coordinator := newTestCoordinator(t, []Provider{&stubProvider{name: "douban"}}, Options{
		RequestTimeout: time.Second, CacheTTL: time.Hour, MaxResults: 20,
	})
	if _, err := NewTMDBSupplementer(coordinator); err == nil {
		t.Fatal("缺少 TMDB provider 时未返回错误")
	}
	if _, err := NewTMDBSupplementer(nil); err == nil {
		t.Fatal("nil coordinator 未返回错误")
	}
}

func TestTMDBSupplementerUsesV2YearAndTypeFallbackOrder(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		media    Media
		search   string
		wantID   string
		wantPath string
	}{
		{
			name: "加一年先于减一年", media: Media{Source: "douban", ID: "d1", Type: "movie", Title: "Same", Year: 2025},
			search: `{"results":[{"id":2,"media_type":"movie","title":"Same","release_date":"2026-01-01"},{"id":3,"media_type":"movie","title":"Same","release_date":"2024-01-01"}]}`,
			wantID: "2", wantPath: "/3/movie/2",
		},
		{
			name: "电影分支回退电视", media: Media{Source: "anilist", ID: "a1", Type: "movie", Title: "Same", Year: 2025},
			search: `{"results":[{"id":7,"media_type":"tv","name":"Same","first_air_date":"2025-01-01"}]}`,
			wantID: "7", wantPath: "/3/tv/7",
		},
		{
			name: "无年份回退允许远年", media: Media{Source: "custom", ID: "c1", Type: "movie", Title: "Same", Year: 2025},
			search: `{"results":[{"id":9,"media_type":"movie","title":"Same","release_date":"2030-01-01"}]}`,
			wantID: "9", wantPath: "/3/movie/9",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var detailPath string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/3/search/multi" {
					_, _ = w.Write([]byte(test.search))
					return
				}
				detailPath = r.URL.Path
				_, _ = fmt.Fprintf(w, `{"id":%s,"title":"Same","name":"Same","genres":[{"id":16}]}`, test.wantID)
			}))
			t.Cleanup(server.Close)
			got, found, err := newTMDBSupplementerForTest(t, server.URL).Supplement(t.Context(), TMDBSupplementInput{Media: test.media})
			if err != nil || !found || got.TMDBID != test.wantID || detailPath != test.wantPath {
				t.Fatalf("got=%#v found=%v err=%v detailPath=%q", got, found, err, detailPath)
			}
		})
	}
}

// 以下用例是根据固定 V2 源码手写的离线回归 fixture，不是官方 V2 运行结果录制或执行差分。
func TestTMDBSupplementerFallsBackToOriginalNameAfterWrongTypeExactMatch(t *testing.T) {
	t.Parallel()
	var searchCalls atomic.Int32
	var detailPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/3/search/multi" {
			searchCalls.Add(1)
			switch r.URL.Query().Get("query") {
			case "中文片名":
				_, _ = w.Write([]byte(`{"results":[{"id":1,"media_type":"movie","title":"中文片名","release_date":"2025-01-01"}]}`))
			case "Original Show":
				_, _ = w.Write([]byte(`{"results":[{"id":2,"media_type":"tv","name":"Original Show","first_air_date":"2025-01-01"}]}`))
			default:
				t.Fatalf("未预期的查询名称: %q", r.URL.Query().Get("query"))
			}
			return
		}
		detailPath = r.URL.Path
		_, _ = w.Write([]byte(`{"id":2,"name":"Original Show","first_air_date":"2025-01-01","genres":[{"id":18}]}`))
	}))
	t.Cleanup(server.Close)

	got, found, err := newTMDBSupplementerForTest(t, server.URL).Supplement(t.Context(), TMDBSupplementInput{
		Media: Media{Source: "douban", ID: "d1", Type: "tv", Title: "中文片名", OriginalTitle: "Original Show", Year: 2025},
	})
	if err != nil || !found || got.TMDBID != "2" || detailPath != "/3/tv/2" {
		t.Fatalf("got=%#v found=%v err=%v detailPath=%q", got, found, err, detailPath)
	}
	if searchCalls.Load() != 2 {
		t.Fatalf("搜索请求数 = %d, want 2", searchCalls.Load())
	}
}

func TestTMDBSupplementerRanksBeforeApplyingDetailBudget(t *testing.T) {
	t.Parallel()
	var detailCalls atomic.Int32
	var detailPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/3/search/multi" {
			_, _ = w.Write([]byte(`{"results":[
				{"id":1,"media_type":"movie","title":"Eleven","release_date":"2014-01-01"},
				{"id":2,"media_type":"movie","title":"Eleven","release_date":"2015-01-01"},
				{"id":3,"media_type":"movie","title":"Eleven","release_date":"2016-01-01"},
				{"id":4,"media_type":"movie","title":"Eleven","release_date":"2017-01-01"},
				{"id":5,"media_type":"movie","title":"Eleven","release_date":"2018-01-01"},
				{"id":6,"media_type":"movie","title":"Eleven","release_date":"2019-01-01"},
				{"id":7,"media_type":"movie","title":"Eleven","release_date":"2020-01-01"},
				{"id":8,"media_type":"movie","title":"Eleven","release_date":"2021-01-01"},
				{"id":9,"media_type":"movie","title":"Eleven","release_date":"2022-01-01"},
				{"id":10,"media_type":"movie","title":"Eleven","release_date":"2023-01-01"},
				{"id":99,"media_type":"movie","title":"Eleven","release_date":"2025-01-01"}
			]}`))
			return
		}
		detailCalls.Add(1)
		detailPath = r.URL.Path
		_, _ = w.Write([]byte(`{"id":99,"title":"Eleven","release_date":"2025-01-01","genres":[{"id":16}]}`))
	}))
	t.Cleanup(server.Close)

	got, found, err := newTMDBSupplementerForTest(t, server.URL).Supplement(t.Context(), TMDBSupplementInput{
		Media: Media{Source: "anilist", ID: "a1", Type: "movie", Title: "Eleven", Year: 2025},
	})
	if err != nil || !found || got.TMDBID != "99" || detailPath != "/3/movie/99" {
		t.Fatalf("got=%#v found=%v err=%v detailPath=%q", got, found, err, detailPath)
	}
	if detailCalls.Load() != 1 {
		t.Fatalf("详情请求数 = %d, want 1", detailCalls.Load())
	}
}

func TestTMDBSupplementerUnknownTypeWithYearUsesMovieThenTV(t *testing.T) {
	t.Parallel()
	var detailPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/3/search/multi" {
			_, _ = w.Write([]byte(`{"results":[
				{"id":1,"media_type":"movie","title":"Unknown","release_date":"2030-01-01"},
				{"id":2,"media_type":"tv","name":"Unknown","first_air_date":"2025-01-01"}
			]}`))
			return
		}
		detailPath = r.URL.Path
		_, _ = w.Write([]byte(`{"id":2,"name":"Unknown","first_air_date":"2025-01-01","genres":[{"id":18}]}`))
	}))
	t.Cleanup(server.Close)

	got, found, err := newTMDBSupplementerForTest(t, server.URL).Supplement(t.Context(), TMDBSupplementInput{
		Media: Media{Source: "custom", ID: "c1", Title: "Unknown", Year: 2025},
	})
	if err != nil || !found || got.TMDBID != "2" || detailPath != "/3/tv/2" {
		t.Fatalf("got=%#v found=%v err=%v detailPath=%q", got, found, err, detailPath)
	}
}

func TestTMDBSupplementerBoundsSeasonDetailRequests(t *testing.T) {
	t.Parallel()
	var detailCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/3/search/multi" {
			_, _ = w.Write([]byte(`{"results":[
				{"id":1,"media_type":"tv","name":"Bounded","first_air_date":"2019-01-01"},
				{"id":2,"media_type":"tv","name":"Bounded","first_air_date":"2018-01-01"},
				{"id":3,"media_type":"tv","name":"Bounded","first_air_date":"2017-01-01"},
				{"id":4,"media_type":"tv","name":"Bounded","first_air_date":"2016-01-01"},
				{"id":5,"media_type":"tv","name":"Bounded","first_air_date":"2015-01-01"},
				{"id":6,"media_type":"tv","name":"Bounded","first_air_date":"2014-01-01"},
				{"id":7,"media_type":"tv","name":"Bounded","first_air_date":"2013-01-01"},
				{"id":8,"media_type":"tv","name":"Bounded","first_air_date":"2012-01-01"},
				{"id":9,"media_type":"tv","name":"Bounded","first_air_date":"2011-01-01"},
				{"id":10,"media_type":"tv","name":"Bounded","first_air_date":"2010-01-01"},
				{"id":11,"media_type":"tv","name":"Bounded","first_air_date":"2009-01-01"}
			]}`))
			return
		}
		detailCalls.Add(1)
		_, _ = w.Write([]byte(`{"id":1,"name":"Bounded","seasons":[{"season_number":2,"air_date":"2025-01-01"}]}`))
	}))
	t.Cleanup(server.Close)
	season := 2
	_, found, err := newTMDBSupplementerForTest(t, server.URL).Supplement(t.Context(), TMDBSupplementInput{
		Media: Media{Source: "bangumi", ID: "b1", Type: "tv", Title: "Bounded"}, Season: &season, SeasonYear: 2026,
	})
	if err != nil || !found {
		t.Fatalf("found=%v err=%v", found, err)
	}
	if detailCalls.Load() != maxTMDBSupplementCandidates {
		t.Fatalf("详情请求数 = %d, want %d", detailCalls.Load(), maxTMDBSupplementCandidates)
	}
}

func TestTMDBSupplementerPropagatesSeasonDetailDeadlineBeforeCachedYearFallback(t *testing.T) {
	t.Parallel()
	provider := &scriptedTMDBSupplementProvider{
		items: []Media{
			{ID: "1", Type: "tv", Title: "Show", Year: 2026},
			{ID: "2", Type: "tv", Title: "Show", Year: 2025},
		},
		details: map[string]Media{
			"tv:1": {ID: "1", Type: "tv", Title: "Show", Year: 2026,
				SeasonInfo: []SeasonInfo{{SeasonNumber: 2, AirDate: "2026-01-01"}}},
		},
		errors: map[string]error{"tv:2": context.DeadlineExceeded},
	}
	coordinator := newTestCoordinator(t, []Provider{provider}, Options{
		RequestTimeout: time.Second, CacheTTL: time.Hour, MaxResults: 20,
	})
	supplementer, err := NewTMDBSupplementer(coordinator)
	if err != nil {
		t.Fatal(err)
	}
	season := 2
	primary := Media{Source: "douban", ID: "d1", Type: "tv", Title: "Show", Year: 2026}

	got, found, err := supplementer.Supplement(t.Context(), TMDBSupplementInput{
		Media: primary, Season: &season, SeasonYear: 2030,
	})
	if !errors.Is(err, context.DeadlineExceeded) || found || !reflect.DeepEqual(got, primary) {
		t.Fatalf("got=%#v found=%v err=%v, want unchanged/deadline", got, found, err)
	}
}

func TestTMDBSupplementerSharesDetailBudgetAcrossSeasonAndYearFallback(t *testing.T) {
	t.Parallel()
	var detailCalls atomic.Int32
	var requestedBudgetOverflow atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/3/search/multi" {
			_, _ = w.Write([]byte(`{"results":[
				{"id":1,"media_type":"tv","name":"Budget","first_air_date":"2030-01-01"},
				{"id":2,"media_type":"tv","name":"Budget","first_air_date":"2029-01-01"},
				{"id":3,"media_type":"tv","name":"Budget","first_air_date":"2028-01-01"},
				{"id":4,"media_type":"tv","name":"Budget","first_air_date":"2027-01-01"},
				{"id":5,"media_type":"tv","name":"Budget","first_air_date":"2026-01-01"},
				{"id":6,"media_type":"tv","name":"Budget","first_air_date":"2025-01-01"},
				{"id":7,"media_type":"tv","name":"Budget","first_air_date":"2024-01-01"},
				{"id":8,"media_type":"tv","name":"Budget","first_air_date":"2023-01-01"},
				{"id":9,"media_type":"tv","name":"Budget","first_air_date":"2022-01-01"},
				{"id":10,"media_type":"tv","name":"Budget","first_air_date":"2021-01-01"},
				{"id":99,"media_type":"tv","name":"Budget","first_air_date":"2020-01-01"}
			]}`))
			return
		}
		detailCalls.Add(1)
		if r.URL.Path == "/3/tv/99" {
			requestedBudgetOverflow.Store(true)
		}
		_, _ = w.Write([]byte(`{"id":1,"name":"Budget","seasons":[{"season_number":2,"air_date":"2039-01-01"}]}`))
	}))
	t.Cleanup(server.Close)
	season := 2
	primary := Media{Source: "bangumi", ID: "b-budget", Type: "tv", Title: "Budget", Year: 2020, GenreIDs: []int{35}}

	got, found, err := newTMDBSupplementerForTest(t, server.URL).Supplement(t.Context(), TMDBSupplementInput{
		Media: primary, Season: &season, SeasonYear: 2040,
	})
	if err != nil || found || !reflect.DeepEqual(got, primary) {
		t.Fatalf("预算耗尽应为未修改的干净 miss: got=%#v found=%v err=%v", got, found, err)
	}
	if detailCalls.Load() != maxTMDBSupplementCandidates || requestedBudgetOverflow.Load() {
		t.Fatalf("详情请求数=%d overflow=%v, want %d/false", detailCalls.Load(), requestedBudgetOverflow.Load(), maxTMDBSupplementCandidates)
	}
}

func TestTMDBSupplementerShortCircuitsAndClonesInput(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	t.Cleanup(server.Close)
	supplementer := newTMDBSupplementerForTest(t, server.URL)
	primary := Media{Source: "themoviedb", ID: "42", Type: "movie", Title: "Movie", GenreIDs: []int{16}, Aliases: []string{"Alias"}}
	got, found, err := supplementer.Supplement(t.Context(), TMDBSupplementInput{Media: primary})
	if err != nil || !found || got.TMDBID != "42" || calls.Load() != 0 {
		t.Fatalf("got=%#v found=%v err=%v calls=%d", got, found, err, calls.Load())
	}
	got.GenreIDs[0] = 99
	got.Aliases[0] = "changed"
	if primary.GenreIDs[0] != 16 || primary.Aliases[0] != "Alias" {
		t.Fatalf("输出修改污染输入: %#v", primary)
	}

	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	got, found, err = supplementer.Supplement(canceled, TMDBSupplementInput{Media: Media{Source: "douban", ID: "1", Type: "movie", Title: "Movie"}})
	if err == nil || found || got.ID != "1" || calls.Load() != 0 {
		t.Fatalf("取消请求 got=%#v found=%v err=%v calls=%d", got, found, err, calls.Load())
	}
}

func newTMDBSupplementerForTest(t *testing.T, baseURL string) *TMDBSupplementer {
	t.Helper()
	tmdb := NewTMDB(newMetadataTestHTTPClient(t, 1<<20), baseURL, "test-token")
	coordinator := newTestCoordinator(t, []Provider{tmdb}, Options{
		RequestTimeout: time.Second, CacheTTL: time.Hour, MaxResults: 20,
	})
	supplementer, err := NewTMDBSupplementer(coordinator)
	if err != nil {
		t.Fatal(err)
	}
	return supplementer
}

type scriptedTMDBSupplementProvider struct {
	items   []Media
	details map[string]Media
	errors  map[string]error
}

func (*scriptedTMDBSupplementProvider) Name() string { return "themoviedb" }

func (p *scriptedTMDBSupplementProvider) Search(context.Context, string) ([]Media, error) {
	return append([]Media(nil), p.items...), nil
}

func (p *scriptedTMDBSupplementProvider) Detail(_ context.Context, id string) (Media, error) {
	if err := p.errors[id]; err != nil {
		return Media{}, err
	}
	return p.details[id], nil
}

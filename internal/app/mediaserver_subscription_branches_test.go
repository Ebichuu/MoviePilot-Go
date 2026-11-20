package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/moviepilot-go/moviepilot-go/internal/mediaconfig"
	"github.com/moviepilot-go/moviepilot-go/internal/subscriptionflow"
)

// newSubscriptionRuntimeForServer 用假站点地址搭一个单服务器运行时。
func newSubscriptionRuntimeForServer(t *testing.T, handler http.HandlerFunc) *mediaServerRuntime {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	setting := mediaServerRuntimeTestServer(nil)
	setting.Config.Host = server.URL
	return newMediaServerRuntimeCompletionPlanTest(t, []mediaconfig.Server{setting})
}

// TestMediaServerSubscriptionEntryGuards 覆盖电影/剧集查询的三个入口守卫。
func TestMediaServerSubscriptionEntryGuards(t *testing.T) {
	runtime := &mediaServerRuntime{}

	if found, err := runtime.FindExistingMovie(nil, subscriptionflow.ExistingMovieQuery{Title: "Movie"}); err == nil || found {
		t.Fatalf("nil 上下文的电影查询 = (%v, %v)", found, err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if found, err := runtime.FindExistingMovie(cancelled, subscriptionflow.ExistingMovieQuery{Title: "Movie"}); !errors.Is(err, context.Canceled) || found {
		t.Fatalf("已取消上下文的电影查询 = (%v, %v)", found, err)
	}
	if found, err := runtime.FindExistingMovie(t.Context(), subscriptionflow.ExistingMovieQuery{Title: "   "}); err != nil || found {
		t.Fatalf("空标题的电影查询 = (%v, %v)", found, err)
	}

	if episodes, err := runtime.FindExistingEpisodes(nil, subscriptionflow.ExistingEpisodesQuery{Title: "Show"}); err == nil || episodes != nil {
		t.Fatalf("nil 上下文的剧集查询 = (%#v, %v)", episodes, err)
	}
	if episodes, err := runtime.FindExistingEpisodes(cancelled, subscriptionflow.ExistingEpisodesQuery{Title: "Show"}); !errors.Is(err, context.Canceled) || episodes != nil {
		t.Fatalf("已取消上下文的剧集查询 = (%#v, %v)", episodes, err)
	}
	if episodes, err := runtime.FindExistingEpisodes(t.Context(), subscriptionflow.ExistingEpisodesQuery{Title: "  "}); err != nil || len(episodes) != 0 {
		t.Fatalf("空标题的剧集查询 = (%#v, %v)", episodes, err)
	}
}

// TestMediaServerSubscriptionMovieSearchFailures 覆盖电影搜索的无效结果与请求失败：
// 两种错误都只跳过当前服务器，最终返回「不存在」。
func TestMediaServerSubscriptionMovieSearchFailures(t *testing.T) {
	items := make([]string, 0, 12)
	for index := 0; index < 11; index++ {
		items = append(items, fmt.Sprintf(`{"Name":"Movie %d","ProductionYear":2026}`, index))
	}
	cases := map[string]string{
		"分页起点非零":  `{"Items":[],"TotalRecordCount":0,"StartIndex":1}`,
		"条目数超过上限": fmt.Sprintf(`{"Items":[%s],"TotalRecordCount":11,"StartIndex":0}`, strings.Join(items, ",")),
		"总数小于条目数": `{"Items":[{"Name":"Movie","ProductionYear":2026}],"TotalRecordCount":0,"StartIndex":0}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			runtime := newSubscriptionRuntimeForServer(t, func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, body)
			})
			found, err := runtime.FindExistingMovie(t.Context(), subscriptionflow.ExistingMovieQuery{
				Title: "Movie", Year: "2026",
			})
			if err != nil || found {
				t.Fatalf("无效结果的电影查询 = (%v, %v)", found, err)
			}
		})
	}

	t.Run("服务端错误", func(t *testing.T) {
		t.Parallel()
		runtime := newSubscriptionRuntimeForServer(t, func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "boom", http.StatusInternalServerError)
		})
		found, err := runtime.FindExistingMovie(t.Context(), subscriptionflow.ExistingMovieQuery{
			Title: "Movie", Year: "2026",
		})
		if err != nil || found {
			t.Fatalf("服务端错误的电影查询 = (%v, %v)", found, err)
		}
	})
}

// TestMediaServerSubscriptionSeriesSearchFailures 覆盖剧集搜索的无效结果、空条目 ID
// 与服务端错误：都不应继续拉取剧集列表。
func TestMediaServerSubscriptionSeriesSearchFailures(t *testing.T) {
	cases := map[string]string{
		"分页起点非零": `{"Items":[{"Id":"series-1","Name":"Show","ProductionYear":2026}],` +
			`"TotalRecordCount":1,"StartIndex":1}`,
		"条目 ID 为空": `{"Items":[{"Id":"","Name":"Show","ProductionYear":2026}],` +
			`"TotalRecordCount":1,"StartIndex":0}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			episodeCalls := 0
			runtime := newSubscriptionRuntimeForServer(t, func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.URL.Path, "/Episodes") {
					episodeCalls++
				}
				_, _ = io.WriteString(w, body)
			})
			episodes, err := runtime.FindExistingEpisodes(t.Context(), subscriptionflow.ExistingEpisodesQuery{
				Title: "Show", Year: "2026",
			})
			if err != nil || len(episodes) != 0 {
				t.Fatalf("剧集搜索结果的查询 = (%#v, %v)", episodes, err)
			}
			if episodeCalls != 0 {
				t.Fatalf("不该继续拉取剧集列表，实际 %d 次", episodeCalls)
			}
		})
	}

	t.Run("服务端错误", func(t *testing.T) {
		t.Parallel()
		runtime := newSubscriptionRuntimeForServer(t, func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "boom", http.StatusInternalServerError)
		})
		episodes, err := runtime.FindExistingEpisodes(t.Context(), subscriptionflow.ExistingEpisodesQuery{
			Title: "Show", Year: "2026",
		})
		if err != nil || len(episodes) != 0 {
			t.Fatalf("服务端错误的剧集查询 = (%#v, %v)", episodes, err)
		}
	})
}

// TestMediaServerSubscriptionEpisodePaginationFailures 覆盖剧集列表的无效分页、
// 空页越过总数与请求失败，全部按「跳过该服务器」处理。
func TestMediaServerSubscriptionEpisodePaginationFailures(t *testing.T) {
	const seriesResponse = `{"Items":[{"Id":"series-1","Name":"Show","ProductionYear":2026}],` +
		`"TotalRecordCount":1,"StartIndex":0}`

	// 有效页：200 条（与 existingEpisodePageSize 一致）。
	fullPage := make([]string, 0, existingEpisodePageSize)
	for index := 0; index < existingEpisodePageSize; index++ {
		fullPage = append(fullPage, fmt.Sprintf(`{"ParentIndexNumber":1,"IndexNumber":%d}`, index+1))
	}
	fullPageJSON := "[" + strings.Join(fullPage, ",") + "]"

	cases := map[string]func(startIndex string) (int, string){
		"起始序号不匹配": func(string) (int, string) {
			return http.StatusOK, fmt.Sprintf(`{"Items":%s,"TotalRecordCount":%d,"StartIndex":1}`,
				fullPageJSON, existingEpisodePageSize)
		},
		"第二页为空但总数未到": func(startIndex string) (int, string) {
			if startIndex == "0" || startIndex == "" {
				return http.StatusOK, fmt.Sprintf(`{"Items":%s,"TotalRecordCount":%d,"StartIndex":0}`,
					fullPageJSON, existingEpisodePageSize*2)
			}
			return http.StatusOK, fmt.Sprintf(`{"Items":[],"TotalRecordCount":%d,"StartIndex":%s}`,
				existingEpisodePageSize*2, startIndex)
		},
		"服务端错误": func(startIndex string) (int, string) {
			if startIndex == "" || startIndex == "0" {
				return http.StatusOK, fmt.Sprintf(`{"Items":%s,"TotalRecordCount":%d,"StartIndex":0}`,
					fullPageJSON, existingEpisodePageSize*2)
			}
			return http.StatusInternalServerError, "boom"
		},
	}
	for name, page := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			runtime := newSubscriptionRuntimeForServer(t, func(w http.ResponseWriter, r *http.Request) {
				if !strings.Contains(r.URL.Path, "/Episodes") {
					_, _ = io.WriteString(w, seriesResponse)
					return
				}
				status, body := page(r.URL.Query().Get("StartIndex"))
				if status != http.StatusOK {
					http.Error(w, body, status)
					return
				}
				_, _ = io.WriteString(w, body)
			})
			episodes, err := runtime.FindExistingEpisodes(t.Context(), subscriptionflow.ExistingEpisodesQuery{
				Title: "Show", Year: "2026",
			})
			if err != nil || len(episodes) != 0 {
				t.Fatalf("无效分页的剧集查询 = (%d 条, %v)", len(episodes), err)
			}
		})
	}
}

// TestMediaServerSubscriptionContextCancelledDuringRequest 覆盖请求进行中上下文被取消：
// 电影搜索、剧集搜索、剧集列表三条链路都必须把 context 错误上抛，而不是静默跳过。
func TestMediaServerSubscriptionContextCancelledDuringRequest(t *testing.T) {
	const seriesResponse = `{"Items":[{"Id":"series-1","Name":"Show","ProductionYear":2026}],` +
		`"TotalRecordCount":1,"StartIndex":0}`
	cases := map[string]struct {
		mode             string
		cancelOnEpisodes bool
	}{
		"电影搜索": {mode: "movie"},
		"剧集搜索": {mode: "episodes"},
		"剧集列表": {mode: "episodes", cancelOnEpisodes: true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			runtime := newSubscriptionRuntimeForServer(t, func(w http.ResponseWriter, r *http.Request) {
				// 剧集列表阶段：让搜索成功，把取消推迟到拉取剧集列表那一步。
				if tc.cancelOnEpisodes && !strings.Contains(r.URL.Path, "/Episodes") {
					_, _ = io.WriteString(w, seriesResponse)
					return
				}
				cancel()
				http.Error(w, "cancelled", http.StatusInternalServerError)
			})
			if tc.mode == "movie" {
				found, err := runtime.FindExistingMovie(ctx, subscriptionflow.ExistingMovieQuery{Title: "Movie"})
				if !errors.Is(err, context.Canceled) || found {
					t.Fatalf("取消中的电影查询 = (%v, %v)", found, err)
				}
				return
			}
			episodes, err := runtime.FindExistingEpisodes(ctx, subscriptionflow.ExistingEpisodesQuery{Title: "Show"})
			if !errors.Is(err, context.Canceled) || episodes != nil {
				t.Fatalf("取消中的剧集查询 = (%#v, %v)", episodes, err)
			}
		})
	}
}

// TestMediaServerSubscriptionSeriesTitleMismatch 覆盖剧集搜索结果都不匹配标题时
// 走到循环末尾的「未找到」出口。
func TestMediaServerSubscriptionSeriesTitleMismatch(t *testing.T) {
	t.Parallel()
	episodeCalls := 0
	runtime := newSubscriptionRuntimeForServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/Episodes") {
			episodeCalls++
		}
		_, _ = io.WriteString(w, `{"Items":[{"Id":"series-1","Name":"Other Show","ProductionYear":2026}],`+
			`"TotalRecordCount":1,"StartIndex":0}`)
	})
	episodes, err := runtime.FindExistingEpisodes(t.Context(), subscriptionflow.ExistingEpisodesQuery{
		Title: "Wanted Show", Year: "2026",
	})
	if err != nil || len(episodes) != 0 || episodeCalls != 0 {
		t.Fatalf("标题不匹配的剧集查询 = (%#v, %v, episodes=%d)", episodes, err, episodeCalls)
	}
}

// TestMediaServerSubscriptionCachedItemContextError 覆盖「缓存命中、详情查询进行中
// 上下文被取消」的出口：必须直接上抛 context 错误，不再退化成标题搜索。
func TestMediaServerSubscriptionCachedItemContextError(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	requests := make([]string, 0, 2)
	runtime := newSubscriptionRuntimeForServer(t, func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.URL.Path)
		if strings.Contains(r.URL.Path, "/Items/") {
			cancel()
		}
		http.Error(w, "cancelled", http.StatusInternalServerError)
	})
	runtime.mu.Lock()
	runtime.itemLookup = &mediaServerItemLookupFake{itemID: "item-42", found: true}
	for _, entry := range runtime.entries {
		entry.userMu.Lock()
		entry.userID, entry.userResolved = "user-1", true
		entry.userMu.Unlock()
	}
	runtime.mu.Unlock()

	found, err := runtime.FindExistingMovie(ctx, subscriptionflow.ExistingMovieQuery{Title: "Movie"})
	if !errors.Is(err, context.Canceled) || found {
		t.Fatalf("取消中的缓存详情查询 = (%v, %v)", found, err)
	}
	if len(requests) != 1 || !strings.Contains(requests[0], "/Items/item-42") {
		t.Fatalf("请求序列 = %#v，期望只查询一次缓存条目", requests)
	}
}

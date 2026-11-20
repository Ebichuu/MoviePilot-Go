package indexer

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

// 本文件覆盖 M-Team 用户数据在取消上下文时的各段退出路径：
// 消息搜索失败、消息遍历、标记已读、做种列表遍历、做种分页循环、Tracker 请求。

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func mtorrentJSONResponse(body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

const (
	mtorrentProfileBody  = `{"code":0,"data":{"id":9,"username":"bob","role":"Member","memberStatus":{"createdDate":"2020-01-02"},"memberCount":{"uploaded":100,"downloaded":50,"shareRate":2,"bonus":7}}}`
	mtorrentEmptySearch  = `{"code":0,"data":{"data":[]}}`
	mtorrentUnreadSearch = `{"code":0,"data":{"data":[{"id":"5","title":"公告","createdDate":"2020-01-02","context":"正文","unread":true}]}}`
	mtorrentSeedingBody  = `{"code":0,"data":{"data":[{"torrent":{"size":10,"source":1}}]}}`
	mtorrentTrackerBody  = `{"code":0,"data":{"seeder":5}}`
)

// mtorrentCancelAdapter 用自定义传输替换网络层，按路径返回固定响应，
// 便于在指定请求处取消上下文。
func mtorrentCancelAdapter(t *testing.T, behaviors map[string]func() (*http.Response, error)) *MTorrentAdapter {
	t.Helper()
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if handler, ok := behaviors[r.URL.Path]; ok {
			return handler()
		}
		switch r.URL.Path {
		case "/api/member/profile":
			return mtorrentJSONResponse(mtorrentProfileBody), nil
		case "/api/msg/search":
			return mtorrentJSONResponse(mtorrentEmptySearch), nil
		case "/api/msg/markRead":
			return mtorrentJSONResponse(`{"code":0}`), nil
		case "/api/member/getUserTorrentList":
			return mtorrentJSONResponse(mtorrentSeedingBody), nil
		case "/api/tracker/myPeerStatus":
			return mtorrentJSONResponse(mtorrentTrackerBody), nil
		default:
			return mtorrentJSONResponse(`{"code":0}`), nil
		}
	})
	client := newIndexerHTTPClient(t, 1)
	client.client.Transport = transport
	adapter, err := NewMTorrentAdapter(client, 31, "M-Team", "https://mtorrent.example", map[string]any{
		"api_url": "https://mtorrent.example", "apikey": "fixture-key",
	})
	if err != nil {
		t.Fatalf("构造适配器: %v", err)
	}
	return adapter
}

func TestMTorrentUserDataCancelBranches(t *testing.T) {
	t.Run("消息搜索失败且上下文已取消", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		adapter := mtorrentCancelAdapter(t, map[string]func() (*http.Response, error){
			"/api/msg/search": func() (*http.Response, error) {
				cancel()
				return nil, context.Canceled
			},
		})
		if _, err := adapter.FetchSiteUserData(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("消息遍历期间取消", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		adapter := mtorrentCancelAdapter(t, map[string]func() (*http.Response, error){
			"/api/msg/search": func() (*http.Response, error) {
				cancel()
				return mtorrentJSONResponse(mtorrentUnreadSearch), nil
			},
		})
		if _, err := adapter.FetchSiteUserData(ctx); err == nil {
			t.Fatal("上下文取消后应返回错误")
		}
	})

	t.Run("标记已读期间取消", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		adapter := mtorrentCancelAdapter(t, map[string]func() (*http.Response, error){
			"/api/msg/search": func() (*http.Response, error) {
				return mtorrentJSONResponse(mtorrentUnreadSearch), nil
			},
			"/api/msg/markRead": func() (*http.Response, error) {
				cancel()
				return nil, context.Canceled
			},
		})
		if _, err := adapter.FetchSiteUserData(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("做种列表遍历期间取消", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		adapter := mtorrentCancelAdapter(t, map[string]func() (*http.Response, error){
			"/api/member/getUserTorrentList": func() (*http.Response, error) {
				cancel()
				return mtorrentJSONResponse(mtorrentSeedingBody), nil
			},
		})
		if _, err := adapter.FetchSiteUserData(ctx); err == nil {
			t.Fatal("上下文取消后应返回错误")
		}
	})

	t.Run("做种分页循环开始前取消", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		adapter := mtorrentCancelAdapter(t, map[string]func() (*http.Response, error){
			"/api/tracker/myPeerStatus": func() (*http.Response, error) {
				cancel()
				return mtorrentJSONResponse(mtorrentTrackerBody), nil
			},
		})
		if _, err := adapter.FetchSiteUserData(ctx); err == nil {
			t.Fatal("上下文取消后应返回错误")
		}
	})

	t.Run("Tracker 请求失败且上下文已取消", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		adapter := mtorrentCancelAdapter(t, map[string]func() (*http.Response, error){
			"/api/tracker/myPeerStatus": func() (*http.Response, error) {
				cancel()
				return nil, context.Canceled
			},
		})
		if _, err := adapter.FetchSiteUserData(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v", err)
		}
	})
}

package indexer

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

// 本文件覆盖各适配器搜索遍历的「命中上限即停」与「遍历中取消」分支。

func TestRousiSearchStopsAtLimit(t *testing.T) {
	endpoint := probeServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":"0","data":{"torrents":[
			{"id":"7","title":"示例剧集"},
			{"id":"8","title":"另一剧集"}
		]}}`))
	})
	adapter, err := NewRousiAdapter(newIndexerHTTPClient(t, 1), 9, "RousiPro", endpoint, map[string]any{
		"apikey": "k",
	})
	if err != nil {
		t.Fatalf("构造适配器: %v", err)
	}
	resources, err := adapter.Search(context.Background(), Query{Keyword: "某剧", Page: 1, Limit: 1})
	if err != nil {
		t.Fatalf("搜索失败: %v", err)
	}
	if len(resources) != 1 {
		t.Fatalf("命中上限后应停止，实际 %d 条", len(resources))
	}
}

func TestTorrentLeechSearchStopsAtLimit(t *testing.T) {
	endpoint := probeServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"torrentList":[
			{"fid":"1","name":"first","filename":"a.torrent"},
			{"fid":"2","name":"second","filename":"b.torrent"}
		]}`))
	})
	adapter, err := NewTorrentLeechAdapter(newIndexerHTTPClient(t, 1), 9, "TL", endpoint, map[string]any{
		"cookie": "session=abc",
	})
	if err != nil {
		t.Fatalf("构造适配器: %v", err)
	}
	resources, err := adapter.Search(context.Background(), Query{Keyword: "dune", Page: 1, Limit: 1})
	if err != nil {
		t.Fatalf("搜索失败: %v", err)
	}
	if len(resources) != 1 {
		t.Fatalf("命中上限后应停止，实际 %d 条", len(resources))
	}
}

func TestSunnyPTSearchCancelDuringIteration(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := newIndexerHTTPClient(t, 1)
	client.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		response := mtorrentJSONResponse(`{"code":0,"data":{"items":[
			{"id":"2","title":"保留","details_url":"/torrents/2"},
			{"id":"3","title":"另一条","details_url":"/torrents/3"}
		]}}`)
		cancel()
		return response, nil
	})
	adapter, err := NewSunnyPTAdapter(client, 11, "SunnyPT", "https://sunny.example", map[string]any{
		"api_url": "https://sunny.example", "apikey": "k",
	})
	if err != nil {
		t.Fatalf("构造适配器: %v", err)
	}
	if _, err := adapter.Search(ctx, Query{Keyword: "某剧"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("遍历中取消 = %v", err)
	}
}

func TestRousiSearchCancelDuringIteration(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := newIndexerHTTPClient(t, 1)
	client.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		response := mtorrentJSONResponse(`{"code":"0","data":{"torrents":[
			{"id":"7","title":"示例剧集"},{"id":"8","title":"另一剧集"}]}}`)
		cancel()
		return response, nil
	})
	adapter, err := NewRousiAdapter(client, 9, "RousiPro", "https://rousi.example", map[string]any{
		"apikey": "k",
	})
	if err != nil {
		t.Fatalf("构造适配器: %v", err)
	}
	if _, err := adapter.Search(ctx, Query{Keyword: "某剧"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("遍历中取消 = %v", err)
	}
}

func TestTorrentLeechSearchCancelDuringIteration(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := newIndexerHTTPClient(t, 1)
	client.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		response := mtorrentJSONResponse(`{"torrentList":[
			{"fid":"1","name":"first","filename":"a.torrent"},
			{"fid":"2","name":"second","filename":"b.torrent"}]}`)
		cancel()
		return response, nil
	})
	adapter, err := NewTorrentLeechAdapter(client, 9, "TL", "https://tle.example", map[string]any{
		"cookie": "session=abc",
	})
	if err != nil {
		t.Fatalf("构造适配器: %v", err)
	}
	if _, err := adapter.Search(ctx, Query{Keyword: "dune"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("遍历中取消 = %v", err)
	}
}

func TestSunnyPTSearchPageNormalizationAndBadJSON(t *testing.T) {
	var page string
	endpoint := probeServer(t, func(w http.ResponseWriter, r *http.Request) {
		page = r.URL.Query().Get("page")
		_, _ = w.Write([]byte(`{"code":0,"data":{"items":[]}}`))
	})
	adapter, err := NewSunnyPTAdapter(newIndexerHTTPClient(t, 1), 11, "SunnyPT", endpoint, map[string]any{
		"api_url": endpoint, "apikey": "k",
	})
	if err != nil {
		t.Fatalf("构造适配器: %v", err)
	}
	if _, err := adapter.Search(context.Background(), Query{Keyword: "某剧", Page: -1, Limit: 5}); err != nil {
		t.Fatalf("负页码搜索: %v", err)
	}
	if page != "1" {
		t.Fatalf("负页码未归一，page = %q", page)
	}

	brokenEndpoint := probeServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("<html>不是 JSON</html>"))
	})
	broken, err := NewSunnyPTAdapter(newIndexerHTTPClient(t, 1), 11, "SunnyPT", brokenEndpoint, map[string]any{
		"api_url": brokenEndpoint, "apikey": "k",
	})
	if err != nil {
		t.Fatalf("构造适配器: %v", err)
	}
	if _, err := broken.Search(context.Background(), Query{Keyword: "某剧"}); err == nil {
		t.Fatal("非 JSON 响应应报错")
	}
}

func TestRousiUserDataTruncatesLongRegistration(t *testing.T) {
	endpoint := probeServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/profile" {
			_, _ = w.Write([]byte(`{"code":0,"data":{"id":77,"username":"rousi-user",
				"role_text":"Uploader","registered_at":"2024-01-01T00:00:00.123456789+08:00",
				"uploaded":3000,"downloaded":1000,"ratio":2.345}}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":0,"data":{"messages":[],"total_pages":1}}`))
	})
	adapter, err := NewRousiAdapter(newIndexerHTTPClient(t, 1), 5, "Rousi", endpoint, map[string]any{
		"passkey": "k",
	})
	if err != nil {
		t.Fatalf("构造适配器: %v", err)
	}
	data, err := adapter.FetchSiteUserData(context.Background())
	if err != nil {
		t.Fatalf("用户数据: %v", err)
	}
	if data.JoinAt == nil || len(*data.JoinAt) != 19 {
		t.Fatalf("注册时间未截断: %v", data.JoinAt)
	}
}

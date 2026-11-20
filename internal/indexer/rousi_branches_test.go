package indexer

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 本文件覆盖 RousiPro 适配器的用户数据与下载分支：
// 业务码失败缺 message、注册时间截断、站内信分页失败与非上下文错误被吞掉、
// 搜索响应非 JSON、下载详情/下载文件失败。

const rousiProfileWithLongJoinAt = `{"code":0,"data":{"id":7,"username":"alice","level_text":"Elite",
	"registered_at":"2020-01-02 03:04:05.123456","uploaded":100,"downloaded":50,"ratio":1.234,
	"karma":3.5,"seeding_leeching_data":{"seeding_count":2,"seeding_size":10,"leeching_count":1,"leeching_size":5}}}`

func rousiBranchServer(t *testing.T, profile, firstPage, secondPage string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/profile":
			_, _ = w.Write([]byte(profile))
		case "/api/messages":
			if r.URL.Query().Get("page") == "0" {
				_, _ = w.Write([]byte(firstPage))
				return
			}
			_, _ = w.Write([]byte(secondPage))
		case "/api/messages/read-all":
			w.WriteHeader(http.StatusOK)
		case "/api/v1/torrents":
			_, _ = w.Write([]byte(`{"code":"0","data":{"torrents":[{"id":"7","title":"示例剧集"}]}}`))
		case "/api/v1/torrents/7":
			_, _ = w.Write([]byte(`{"code":"0","data":{"download_url":"/download/7.torrent"}}`))
		case "/download/7.torrent":
			_, _ = w.Write([]byte("d4:infod4:name5:rousiee"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func newRousiBranchAdapter(t *testing.T, client *HTTPClient, endpoint string) *RousiAdapter {
	t.Helper()
	adapter, err := NewRousiAdapter(client, 33, "RousiPro", endpoint, map[string]any{
		"passkey": "pass-key", "token": "session-token",
	})
	if err != nil {
		t.Fatalf("构造适配器: %v", err)
	}
	return adapter
}

func TestRousiUserDataReportsErrorCode(t *testing.T) {
	server := rousiBranchServer(t, `{"code":401}`, `{"code":0,"data":{}}`, `{"code":0,"data":{}}`)
	adapter := newRousiBranchAdapter(t, newIndexerHTTPClient(t, 1), server.URL)
	data, err := adapter.FetchSiteUserData(context.Background())
	if err != nil {
		t.Fatalf("业务码失败不应返回错误: %v", err)
	}
	if data.ErrMsg == nil || *data.ErrMsg != "未知错误" {
		t.Fatalf("ErrMsg = %v", data.ErrMsg)
	}
}

func TestRousiUserDataTrimsLongJoinAt(t *testing.T) {
	server := rousiBranchServer(t, rousiProfileWithLongJoinAt, `{"code":0,"data":{"messages":[],"total_pages":1}}`, "")
	adapter := newRousiBranchAdapter(t, newIndexerHTTPClient(t, 1), server.URL)
	data, err := adapter.FetchSiteUserData(context.Background())
	if err != nil {
		t.Fatalf("FetchSiteUserData: %v", err)
	}
	if data.JoinAt == nil || len(*data.JoinAt) != 19 {
		t.Fatalf("JoinAt = %v", data.JoinAt)
	}
	if data.Seeding != 2 || data.Leeching != 1 || data.Upload != 100 || data.Download != 50 {
		t.Fatalf("用户数据 = %+v", data)
	}
	if data.UserLevel == nil || *data.UserLevel != "Elite" {
		t.Fatalf("UserLevel = %v", data.UserLevel)
	}
}

func TestRousiUserDataMessageFailures(t *testing.T) {
	profile := rousiProfileWithLongJoinAt
	pageZero := `{"code":0,"data":{"messages":[{"title":"t","created_at":"2020-01-02 03:04:05","content":"c"}],"total_pages":2}}`
	pageOne := `{"code":0,"data":{"messages":[{"title":"t2","content":"c2"}],"total_pages":2}}`

	t.Run("首页请求被取消", func(t *testing.T) {
		server := rousiBranchServer(t, profile, pageZero, pageOne)
		client := newIndexerHTTPClient(t, 1)
		client.client.Transport = &failAfterRequests{base: http.DefaultTransport, after: 1}
		adapter := newRousiBranchAdapter(t, client, server.URL)
		if _, err := adapter.FetchSiteUserData(context.Background()); err == nil {
			t.Fatal("站内信首页被取消时应返回错误")
		}
	})

	t.Run("后续分页被取消", func(t *testing.T) {
		server := rousiBranchServer(t, profile, pageZero, pageOne)
		client := newIndexerHTTPClient(t, 1)
		client.client.Transport = &failAfterRequests{base: http.DefaultTransport, after: 2}
		adapter := newRousiBranchAdapter(t, client, server.URL)
		if _, err := adapter.FetchSiteUserData(context.Background()); err == nil {
			t.Fatal("站内信后续分页被取消时应返回错误")
		}
	})

	t.Run("非上下文错误被吞掉", func(t *testing.T) {
		server := rousiBranchServer(t, profile, pageZero, pageOne)
		client := newIndexerHTTPClient(t, 1)
		client.client.Transport = &failAfterRequestsWith{
			base: http.DefaultTransport, after: 1, err: errors.New("站内信接口不可用"),
		}
		adapter := newRousiBranchAdapter(t, client, server.URL)
		data, err := adapter.FetchSiteUserData(context.Background())
		if err != nil {
			t.Fatalf("站内信接口失败应被吞掉: %v", err)
		}
		if len(data.MessageUnreadContents) != 0 {
			t.Fatalf("不应解析出站内信: %#v", data.MessageUnreadContents)
		}
	})
}

func TestRousiSearchAndDownloadFailures(t *testing.T) {
	server := rousiBranchServer(t, rousiProfileWithLongJoinAt, `{"code":0,"data":{}}`, "")
	adapter := newRousiBranchAdapter(t, newIndexerHTTPClient(t, 1), server.URL)

	resources, err := adapter.Search(context.Background(), Query{Page: -3, Limit: 5, Keyword: "示例"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(resources) != 1 || resources[0].TorrentID != "7" {
		t.Fatalf("resources = %#v", resources)
	}

	nonJSON := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("<html>拦截页</html>"))
	}))
	defer nonJSON.Close()
	brokenAdapter := newRousiBranchAdapter(t, newIndexerHTTPClient(t, 1), nonJSON.URL)
	if _, err := brokenAdapter.Search(context.Background(), Query{}); err == nil {
		t.Fatal("搜索响应不是 JSON 时应返回错误")
	}
	if _, err := brokenAdapter.ResolveDownload(context.Background(), "7"); err == nil {
		t.Fatal("下载详情不是 JSON 时应返回错误")
	}

	t.Run("下载详情请求失败", func(t *testing.T) {
		closed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		endpoint := closed.URL
		closed.Close()
		if _, err := newRousiBranchAdapter(t, newIndexerHTTPClient(t, 1), endpoint).ResolveDownload(context.Background(), "7"); err == nil {
			t.Fatal("详情请求失败时应返回错误")
		}
	})

	t.Run("下载文件请求失败", func(t *testing.T) {
		client := newIndexerHTTPClient(t, 1)
		client.client.Transport = &failAfterRequestsWith{
			base: http.DefaultTransport, after: 1, err: errors.New("下载接口不可用"),
		}
		downloadAdapter := newRousiBranchAdapter(t, client, server.URL)
		if _, err := downloadAdapter.ResolveDownload(context.Background(), "7"); err == nil {
			t.Fatal("下载文件失败时应返回错误")
		}
	})

	t.Run("正常下载", func(t *testing.T) {
		download, err := adapter.ResolveDownload(context.Background(), "7")
		if err != nil || string(download.Body) != "d4:infod4:name5:rousiee" {
			t.Fatalf("下载结果 = %+v（%v）", download, err)
		}
		if !strings.HasSuffix(download.Filename, "7.torrent") {
			t.Fatalf("Filename = %q", download.Filename)
		}
	})
}

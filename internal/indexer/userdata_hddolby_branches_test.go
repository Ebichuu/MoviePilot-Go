package indexer

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// 本文件覆盖 HDDolby 用户数据抓取的失败与跨站分支：
// 用户接口传输失败、Peer 接口失败与非法响应、消息分页/条目跨站、
// 消息接口传输失败被吞掉。

const hddolbyUserPayload = `{"status":0,"data":[{"id":"42","username":"alice","unread_messages":"2"}]}`

func hddolbyBranchServer(t *testing.T, messages string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/user/data":
			_, _ = w.Write([]byte(hddolbyUserPayload))
		case "/api/v1/user/peers":
			_, _ = w.Write([]byte(`{"status":0,"data":[]}`))
		case "/messages.php":
			_, _ = w.Write([]byte(messages))
		default:
			http.NotFound(w, r)
		}
	}))
}

func newHDDolbyBranchAdapter(t *testing.T, client *HTTPClient, endpoint string) *HDDolbyAdapter {
	t.Helper()
	adapter, err := NewHDDolbyAdapter(client, 51, "HDDolby", endpoint, map[string]any{
		"api_url": endpoint, "apikey": "api-key", "cookie": "session=fixture",
	})
	if err != nil {
		t.Fatalf("构造适配器: %v", err)
	}
	return adapter
}

func TestHDDolbyUserDataTransportFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	endpoint := server.URL
	server.Close()
	adapter := newHDDolbyBranchAdapter(t, newIndexerHTTPClient(t, 1), endpoint)
	if _, err := adapter.FetchSiteUserData(context.Background()); err == nil {
		t.Fatal("连接不可用时应返回错误")
	}
}

func TestHDDolbyUserDataPeerFailures(t *testing.T) {
	t.Run("Peer 请求失败", func(t *testing.T) {
		server := hddolbyBranchServer(t, "")
		defer server.Close()
		client := newIndexerHTTPClient(t, 1)
		client.client.Transport = &failAfterRequests{base: http.DefaultTransport, after: 1}
		adapter := newHDDolbyBranchAdapter(t, client, server.URL)
		if _, err := adapter.FetchSiteUserData(context.Background()); err == nil {
			t.Fatal("Peer 请求被取消时应返回错误")
		}
	})

	t.Run("Peer 响应非法", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/v1/user/data" {
				_, _ = w.Write([]byte(hddolbyUserPayload))
				return
			}
			_, _ = w.Write([]byte("not-json"))
		}))
		defer server.Close()
		adapter := newHDDolbyBranchAdapter(t, newIndexerHTTPClient(t, 1), server.URL)
		if _, err := adapter.FetchSiteUserData(context.Background()); err == nil {
			t.Fatal("Peer 响应不是 JSON 时应返回错误")
		}
	})
}

func TestHDDolbyUserDataRejectsCrossSiteMessageLinks(t *testing.T) {
	t.Run("分页链接跨站", func(t *testing.T) {
		server := hddolbyBranchServer(t, `<table></table><a href="https://other.example/messages.php?page=2">下一页</a>`)
		defer server.Close()
		adapter := newHDDolbyBranchAdapter(t, newIndexerHTTPClient(t, 1), server.URL)
		if _, err := adapter.FetchSiteUserData(context.Background()); err == nil {
			t.Fatal("跨站分页链接应返回错误")
		}
	})

	t.Run("条目链接跨站", func(t *testing.T) {
		server := hddolbyBranchServer(t, `<table><tr><td><a href="https://other.example/viewmessage.php?id=1">new</a></td></tr></table>`)
		defer server.Close()
		adapter := newHDDolbyBranchAdapter(t, newIndexerHTTPClient(t, 1), server.URL)
		if _, err := adapter.FetchSiteUserData(context.Background()); err == nil {
			t.Fatal("跨站条目链接应返回错误")
		}
	})
}

// failAfterRequestsWith 让第 N 次之后的请求以指定的非上下文错误失败。
type failAfterRequestsWith struct {
	base  http.RoundTripper
	after int32
	err   error
	calls int32
}

func (t *failAfterRequestsWith) RoundTrip(req *http.Request) (*http.Response, error) {
	if atomic.AddInt32(&t.calls, 1) > t.after {
		return nil, t.err
	}
	return t.base.RoundTrip(req)
}

func TestHDDolbyUserDataSwallowsMessageTransportFailure(t *testing.T) {
	server := hddolbyBranchServer(t, `<table></table>`)
	defer server.Close()
	client := newIndexerHTTPClient(t, 1)
	client.client.Transport = &failAfterRequestsWith{
		base: http.DefaultTransport, after: 2, err: errors.New("消息接口不可用"),
	}
	adapter := newHDDolbyBranchAdapter(t, client, server.URL)
	data, err := adapter.FetchSiteUserData(context.Background())
	if err != nil {
		t.Fatalf("消息接口传输失败应被吞掉，得到 %v", err)
	}
	if len(data.MessageUnreadContents) != 0 {
		t.Fatalf("不应解析出站内信内容，得到 %#v", data.MessageUnreadContents)
	}
}

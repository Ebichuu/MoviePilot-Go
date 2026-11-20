package indexer

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// 本文件覆盖 SunnyPT 用户数据抓取的失败与畸形响应分支：
// 传输失败、非 JSON、尾随内容、结构不符、业务码失败、空 data、
// 消息条目字段缺失，以及各阶段的上下文取消。

const sunnyPTEmptyMessages = `{"code":0,"data":{"items":[],"unread_count":0,"has_more":false}}`

func sunnyPTFixtureServer(t *testing.T, profile, messages string) *httptest.Server {
	t.Helper()
	if messages == "" {
		messages = sunnyPTEmptyMessages
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/mp/profile":
			_, _ = w.Write([]byte(profile))
		case "/api/v1/mp/messages":
			_, _ = w.Write([]byte(messages))
		default:
			http.NotFound(w, r)
		}
	}))
}

func newSunnyPTFixtureAdapter(t *testing.T, client *HTTPClient, endpoint string) *SunnyPTAdapter {
	t.Helper()
	adapter, err := NewSunnyPTAdapter(client, 41, "SunnyPT", endpoint, map[string]any{
		"api_url": endpoint + "/api/v1/mp", "apikey": "fixture-key",
	})
	if err != nil {
		t.Fatalf("构造适配器: %v", err)
	}
	return adapter
}

func TestSunnyPTUserDataMalformedResponses(t *testing.T) {
	tests := []struct {
		name       string
		profile    string
		wantErrMsg string
		wantSilent bool
	}{
		{name: "非 JSON", profile: "not-json", wantErrMsg: "不是有效 JSON"},
		{name: "尾随内容", profile: `{"code":0,"data":{"id":1}} trailing`, wantErrMsg: "不是有效 JSON"},
		{name: "结构不是对象", profile: `[1,2,3]`, wantErrMsg: "结构无效"},
		{name: "业务码失败且无提示", profile: `{"code":7}`, wantErrMsg: "获取用户信息失败"},
		{name: "业务码失败带提示", profile: `{"code":7,"msg":"接口拒绝"}`, wantErrMsg: "接口拒绝"},
		{name: "空 data", profile: `{"code":0,"data":{}}`, wantSilent: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := sunnyPTFixtureServer(t, tt.profile, "")
			defer server.Close()
			adapter := newSunnyPTFixtureAdapter(t, newIndexerHTTPClient(t, 1), server.URL)
			data, err := adapter.FetchSiteUserData(context.Background())
			if err != nil {
				t.Fatalf("FetchSiteUserData() error = %v", err)
			}
			if tt.wantSilent {
				if data.ErrMsg != nil {
					t.Fatalf("空 data 不应写入错误信息，得到 %q", *data.ErrMsg)
				}
				return
			}
			if data.ErrMsg == nil || !strings.Contains(*data.ErrMsg, tt.wantErrMsg) {
				t.Fatalf("ErrMsg = %v, want 含 %q", data.ErrMsg, tt.wantErrMsg)
			}
		})
	}
}

func TestSunnyPTUserDataTransportFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	endpoint := server.URL
	server.Close()

	adapter := newSunnyPTFixtureAdapter(t, newIndexerHTTPClient(t, 1), endpoint)
	data, err := adapter.FetchSiteUserData(context.Background())
	if err != nil {
		t.Fatalf("传输失败不应返回错误，得到 %v", err)
	}
	if data.ErrMsg == nil || !strings.Contains(*data.ErrMsg, "未收到 API 响应") {
		t.Fatalf("ErrMsg = %v", data.ErrMsg)
	}
}

func TestSunnyPTUserDataSkipsIncompleteMessages(t *testing.T) {
	messages := `{"code":0,"data":{"items":[` +
		`{"title":"缺时间","content":"正文","unread":true},` +
		`{"title":"缺正文","created_at":"2026-07-22 09:10:11","unread":true},` +
		`"不是对象",` +
		`{"title":"已读","content":"正文","created_at":"2026-07-22 09:10:11","unread":false},` +
		`{"id":77,"title":"正常","content":"正文","created_at":"2026-07-22 09:10:11","unread":true}` +
		`],"unread_count":4,"has_more":false}}`
	server := sunnyPTFixtureServer(t, `{"code":0,"data":{"id":9,"username":"sunny"}}`, messages)
	defer server.Close()
	adapter := newSunnyPTFixtureAdapter(t, newIndexerHTTPClient(t, 1), server.URL)
	data, err := adapter.FetchSiteUserData(context.Background())
	if err != nil {
		t.Fatalf("FetchSiteUserData() error = %v", err)
	}
	if len(data.MessageUnreadContents) != 1 {
		t.Fatalf("只应保留唯一条完整未读消息，得到 %#v", data.MessageUnreadContents)
	}
	if data.MessageUnread != 4 {
		t.Fatalf("未读数 = %d, want 4", data.MessageUnread)
	}
}

// failAfterRequests 让第 N 次之后的请求直接以取消错误失败，
// 用于稳定命中「消息分页请求失败」这类分支。
type failAfterRequests struct {
	base  http.RoundTripper
	after int32
	calls int32
}

func (t *failAfterRequests) RoundTrip(req *http.Request) (*http.Response, error) {
	if atomic.AddInt32(&t.calls, 1) > t.after {
		return nil, context.Canceled
	}
	return t.base.RoundTrip(req)
}

func TestSunnyPTUserDataCancellation(t *testing.T) {
	t.Run("资料读取后取消", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/api/v1/mp/profile":
				_, _ = w.Write([]byte(`{"code":0,"data":{"id":9,"username":"sunny"}}`))
				cancel()
			default:
				_, _ = w.Write([]byte(sunnyPTEmptyMessages))
			}
		}))
		defer server.Close()
		adapter := newSunnyPTFixtureAdapter(t, newIndexerHTTPClient(t, 1), server.URL)
		if _, err := adapter.FetchSiteUserData(ctx); err == nil {
			t.Fatal("进入消息分页前上下文已取消应返回错误")
		}
	})

	t.Run("消息请求失败", func(t *testing.T) {
		server := sunnyPTFixtureServer(t, `{"code":0,"data":{"id":9,"username":"sunny"}}`, "")
		defer server.Close()
		client := newIndexerHTTPClient(t, 1)
		client.client.Transport = &failAfterRequests{base: http.DefaultTransport, after: 1}
		adapter := newSunnyPTFixtureAdapter(t, client, server.URL)
		if _, err := adapter.FetchSiteUserData(context.Background()); err == nil {
			t.Fatal("消息请求被取消时应返回错误")
		}
	})
}

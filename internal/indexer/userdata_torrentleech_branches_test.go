package indexer

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// 本文件覆盖 TorrentLeech 用户数据抓取里剩下的分支：
// 首页/资料页/做种页的上下文取消、从页面 URL 推断用户 ID、表格回退取用户名、
// 以及做种表格里的短行跳过。

// cancelAfterReads 在第 N 个响应体读完后取消上下文，用于稳定命中「第 N 次请求失败」。
type cancelAfterReads struct {
	base   http.RoundTripper
	after  int32
	cancel context.CancelFunc
	reads  int32
}

func (t *cancelAfterReads) RoundTrip(req *http.Request) (*http.Response, error) {
	response, err := t.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	response.Body = &cancelAfterReadBody{inner: response.Body, owner: t}
	return response, nil
}

type cancelAfterReadBody struct {
	inner io.ReadCloser
	owner *cancelAfterReads
}

func (b *cancelAfterReadBody) Read(p []byte) (int, error) {
	n, err := b.inner.Read(p)
	if err == io.EOF && atomic.AddInt32(&b.owner.reads, 1) == b.owner.after {
		b.owner.cancel()
	}
	return n, err
}

func (b *cancelAfterReadBody) Close() error { return b.inner.Close() }

func torrentLeechTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			_, _ = w.Write([]byte(`<a href="/logout">logout</a>` +
				`<a href="/profile/55/">我</a>`))
		case "/profile/55/view":
			_, _ = w.Write([]byte(`<table class="profileViewTable">` +
				`<tr><td>Username</td><td>bob</td></tr>` +
				`<tr><td>Class</td><td>Elite User</td></tr>` +
				`</table>`))
		case "/profile/55/seeding":
			_, _ = w.Write([]byte(`<table><tbody>` +
				`<tr><td>短行</td><td>1 GB</td></tr>` +
				`<tr><td>正常</td><td>3 GB</td><td>x</td><td>x</td><td>x</td><td>x</td><td>9</td></tr>` +
				`</tbody></table>`))
		default:
			http.NotFound(w, r)
		}
	}))
}

func TestTorrentLeechUserDataCancellationAtEachStep(t *testing.T) {
	tests := []struct {
		name       string
		cancelAt   int32
		cancelOnly bool
	}{
		{name: "请求前已取消", cancelOnly: true},
		{name: "首页读完后取消", cancelAt: 1},
		{name: "资料页读完后取消", cancelAt: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := torrentLeechTestServer(t)
			defer server.Close()
			client := newIndexerHTTPClient(t, 1)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tt.cancelOnly {
				cancel()
			} else {
				client.client.Transport = &cancelAfterReads{base: http.DefaultTransport, after: tt.cancelAt, cancel: cancel}
			}
			adapter, err := NewTorrentLeechAdapter(client, 33, "TorrentLeech", server.URL, map[string]any{"cookie": "session=fixture"})
			if err != nil {
				t.Fatalf("构造适配器: %v", err)
			}
			if _, err := adapter.FetchSiteUserData(ctx); err == nil {
				t.Fatal("上下文取消时应返回错误")
			}
		})
	}
}

func TestTorrentLeechUserDataFallsBackToBodyAndTable(t *testing.T) {
	server := torrentLeechTestServer(t)
	defer server.Close()
	adapter, err := NewTorrentLeechAdapter(newIndexerHTTPClient(t, 1), 34, "TorrentLeech", server.URL, map[string]any{"cookie": "session=fixture"})
	if err != nil {
		t.Fatalf("构造适配器: %v", err)
	}
	data, err := adapter.FetchSiteUserData(context.Background())
	if err != nil {
		t.Fatalf("FetchSiteUserData() error = %v", err)
	}
	if data.UserID != "55" {
		t.Fatalf("应从页面链接推断用户 ID，得到 %q", data.UserID)
	}
	if data.Username == nil || !strings.Contains(*data.Username, "bob") {
		t.Fatalf("应从表格回退取用户名，得到 %#v", data.Username)
	}
	if data.UserLevel == nil || *data.UserLevel != "Elite User" {
		t.Fatalf("等级 = %#v", data.UserLevel)
	}
	if data.Seeding != 1 || len(data.SeedingInfo) != 1 || data.SeedingInfo[0][0] != 9 {
		t.Fatalf("短行应被跳过，做种统计 = %#v", data.SeedingInfo)
	}
}

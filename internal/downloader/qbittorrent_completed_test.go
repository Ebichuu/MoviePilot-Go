package downloader

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// qbTestServer 模拟 qBittorrent Web API 的最小面：登录、任务列表、文件清单。
type qbTestServer struct {
	torrents  string
	files     map[string]string
	loginBody string
	filesHits int32
	listHits  int32
}

func (s *qbTestServer) handler(w http.ResponseWriter, r *http.Request) {
	switch {
	case strings.HasSuffix(r.URL.Path, "/auth/login"):
		body := s.loginBody
		if body == "" {
			body = "Ok."
		}
		_, _ = w.Write([]byte(body))
	case strings.HasSuffix(r.URL.Path, "/torrents/info"):
		atomic.AddInt32(&s.listHits, 1)
		_, _ = w.Write([]byte(s.torrents))
	case strings.HasSuffix(r.URL.Path, "/torrents/files"):
		atomic.AddInt32(&s.filesHits, 1)
		hash := r.URL.Query().Get("hash")
		payload, ok := s.files[hash]
		if !ok {
			payload = "{}"
		}
		_, _ = w.Write([]byte(payload))
	case strings.HasSuffix(r.URL.Path, "/app/webapiVersion"):
		_, _ = w.Write([]byte("2.8.3"))
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func newQBAdapter(t *testing.T, server *httptest.Server) *QBittorrent {
	t.Helper()
	adapter, err := NewQBittorrent(QBittorrentConfig{
		URL: server.URL, Username: "user", Password: "pass", Timeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("构造适配器失败: %v", err)
	}
	return adapter
}

func TestQBittorrentConnect(t *testing.T) {
	state := &qbTestServer{}
	server := httptest.NewServer(http.HandlerFunc(state.handler))
	defer server.Close()
	adapter := newQBAdapter(t, server)
	if err := adapter.Connect(context.Background()); err != nil {
		t.Fatalf("登录应成功: %v", err)
	}

	// qBittorrent 用 200 + "Fails." 表示凭据错误，必须转成错误而不是当成功。
	failing := &qbTestServer{loginBody: "Fails."}
	failingServer := httptest.NewServer(http.HandlerFunc(failing.handler))
	defer failingServer.Close()
	if err := newQBAdapter(t, failingServer).Connect(context.Background()); err == nil {
		t.Fatal("凭据错误应报登录失败")
	}
}

func TestQBittorrentCloseIdleConnectionsIsNilSafe(t *testing.T) {
	var nilAdapter *QBittorrent
	nilAdapter.CloseIdleConnections() // 不能 panic

	state := &qbTestServer{}
	server := httptest.NewServer(http.HandlerFunc(state.handler))
	defer server.Close()
	newQBAdapter(t, server).CloseIdleConnections()
}

func TestQBittorrentListCompleted(t *testing.T) {
	state := &qbTestServer{torrents: `[
		{"hash":"ABCDEF0123456789ABCDEF0123456789ABCDEF01","name":"影片 A","size":100,"total_size":100,
		 "progress":1,"state":"uploading","tags":"MoviePilot-已完成","completion_on":1700000000},
		{"hash":"bad-hash","name":"坏种子","size":1,"total_size":1,"progress":1,"state":"uploading"},
		{"hash":"1111111111111111111111111111111111111111","name":"影片 B","size":200,"total_size":200,
		 "progress":1,"state":"uploading"}
	]`}
	server := httptest.NewServer(http.HandlerFunc(state.handler))
	defer server.Close()
	adapter := newQBAdapter(t, server)

	tasks, err := adapter.ListCompleted(context.Background())
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	// 非法 hash 的种子被跳过，其余保留。
	if len(tasks) != 2 {
		t.Fatalf("任务数 = %d: %+v", len(tasks), tasks)
	}
	// 这里的 hash 只做长度/字符校验，不改写大小写；比较时按不区分大小写断言。
	if !strings.EqualFold(tasks[0].Hash, "abcdef0123456789abcdef0123456789abcdef01") {
		t.Fatalf("hash = %q", tasks[0].Hash)
	}
	if tasks[0].Title != "影片 A" {
		t.Fatalf("名称 = %q", tasks[0].Title)
	}
	// completion_on 存在时必须给出完成时间，历史页依赖它排序。
	if tasks[0].CompletedAt.IsZero() {
		t.Fatal("应记录完成时间")
	}
	if !tasks[0].CompletedAt.Equal(time.Unix(1700000000, 0).UTC()) {
		t.Fatalf("完成时间 = %v", tasks[0].CompletedAt)
	}
	// 没有标签的任务也应出现在历史里（历史不按标签过滤）。
	if tasks[1].Hash != "1111111111111111111111111111111111111111" {
		t.Fatalf("第二个任务 = %+v", tasks[1])
	}
}

func TestQBittorrentListCompletedGuards(t *testing.T) {
	var nilAdapter *QBittorrent
	if _, err := nilAdapter.ListCompleted(context.Background()); err == nil {
		t.Fatal("nil 适配器应报错")
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	state := &qbTestServer{}
	server := httptest.NewServer(http.HandlerFunc(state.handler))
	defer server.Close()
	if _, err := newQBAdapter(t, server).ListCompleted(cancelled); err == nil {
		t.Fatal("已取消上下文应报错")
	}
	if _, err := newQBAdapter(t, server).ListCompleted(nil); err == nil {
		t.Fatal("nil 上下文不应被吞掉")
	}
}

func TestGetFilesForSelectionRetriesUntilMetadataReady(t *testing.T) {
	hash := "2222222222222222222222222222222222222222"
	state := &qbTestServer{}
	var hits int32
	// 前两次返回空清单（元数据尚未就绪），第三次给出文件。
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/torrents/files") {
			if atomic.AddInt32(&hits, 1) <= 2 {
				_, _ = w.Write([]byte(`[]`))
				return
			}
			_, _ = w.Write([]byte(`[{"name":"a.mkv","size":10,"progress":1}]`))
			return
		}
		state.handler(w, r)
	}))
	defer server.Close()

	adapter := newQBAdapter(t, server)
	adapter.filesRetryInterval = time.Millisecond

	files, err := adapter.getFilesForSelection(context.Background(), hash)
	if err != nil {
		t.Fatalf("重试后应成功: %v", err)
	}
	if files == nil || len(*files) != 1 {
		t.Fatalf("文件清单 = %+v", files)
	}
	if got := atomic.LoadInt32(&hits); got != 3 {
		t.Fatalf("应重试到第 3 次，实际 %d 次", got)
	}
}

func TestGetFilesForSelectionExhaustsRetries(t *testing.T) {
	hash := "3333333333333333333333333333333333333333"
	// 合法但始终为空的清单：元数据一直没就绪。
	state := &qbTestServer{files: map[string]string{hash: "[]"}}
	server := httptest.NewServer(http.HandlerFunc(state.handler))
	defer server.Close()
	adapter := newQBAdapter(t, server)
	adapter.filesRetryInterval = time.Millisecond

	files, err := adapter.getFilesForSelection(context.Background(), hash)
	if files != nil || err != nil {
		t.Fatalf("耗尽重试应返回空清单与 nil 错误: %+v %v", files, err)
	}
	if hits := atomic.LoadInt32(&state.filesHits); hits != int32(torrentFilesRetryTimes) {
		t.Fatalf("重试次数 = %d，期望 %d", hits, torrentFilesRetryTimes)
	}

	// 上下文取消必须立刻退出，而不是把 5 次重试走完。
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := adapter.getFilesForSelection(cancelled, "4444444444444444444444444444444444444444"); err == nil {
		t.Fatal("已取消上下文应立即报错")
	}

	// 响应体无法解析时必须返回最后一次错误，不能把「读不懂」当成「空清单」。
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"not":"a list"}`))
	}))
	defer broken.Close()
	brokenAdapter := newQBAdapter(t, broken)
	brokenAdapter.filesRetryInterval = time.Millisecond
	if _, err := brokenAdapter.getFilesForSelection(context.Background(), hash); err == nil {
		t.Fatal("无法解析的响应应返回错误")
	}
}

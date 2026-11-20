package telegrambot

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// failingBot 让指定方法返回 ok=false，其它方法正常返回。
type failingBot struct {
	mu    sync.Mutex
	fail  map[string]bool
	calls []fakeRequest
}

func (b *failingBot) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		body := map[string]any{}
		_ = json.Unmarshal(raw, &body)
		method := r.URL.Path[strings.LastIndexByte(r.URL.Path, '/')+1:]
		b.mu.Lock()
		b.calls = append(b.calls, fakeRequest{method: method, body: body})
		shouldFail := b.fail[method]
		b.mu.Unlock()
		if shouldFail {
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "description": "failed"})
			return
		}
		result := any(map[string]any{"ok": true})
		if method == "sendMessage" {
			result = map[string]any{"message_id": 77}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": result})
	}
}

func (b *failingBot) byMethod(method string) []fakeRequest {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []fakeRequest
	for _, call := range b.calls {
		if call.method == method {
			out = append(out, call)
		}
	}
	return out
}

func newFailingClient(t *testing.T, failing ...string) (*apiClient, *failingBot) {
	t.Helper()
	bot := &failingBot{fail: map[string]bool{}}
	for _, method := range failing {
		bot.fail[method] = true
	}
	server := httptest.NewServer(bot.handler())
	t.Cleanup(server.Close)
	return newAPIClient(server.URL, "token", server.Client()), bot
}

func callbackQuery(chatID int64, messageID int, data string) Update {
	return Update{
		UpdateID: 2,
		CallbackQuery: &CallbackQuery{
			ID:      "cb-1",
			From:    User{ID: 99},
			Message: &Message{MessageID: messageID, Chat: Chat{ID: chatID}},
			Data:    data,
		},
	}
}

func TestUpdateChatID(t *testing.T) {
	cases := []struct {
		name   string
		update Update
		want   int64
	}{
		{"普通消息", textMessage(12, "/help"), 12},
		{"按钮回调", callbackQuery(13, 5, "x"), 13},
		{"回调无消息", Update{CallbackQuery: &CallbackQuery{ID: "cb"}}, 0},
		{"空更新", Update{}, 0},
	}
	for _, testCase := range cases {
		if got := updateChatID(testCase.update); got != testCase.want {
			t.Fatalf("%s = %d, want %d", testCase.name, got, testCase.want)
		}
	}
}

func TestManagerDefaultsAndPollerKey(t *testing.T) {
	manager := NewManager(nil, nil)
	if manager.logger == nil || manager.router != nil || manager.now == nil || manager.running == nil {
		t.Fatalf("默认 manager = %#v", manager)
	}
	if key := pollerKey(Config{Token: " t ", APIURL: " u ", ChatID: " c "}); key != "t\x00u\x00c" {
		t.Fatalf("pollerKey = %q", key)
	}
}

func TestPollerRunGuards(t *testing.T) {
	var nilPoller *Poller
	nilPoller.Run(context.Background())

	api := &apiClient{token: "t"}
	router := NewRouter(nil, nil)
	(&Poller{router: router}).Run(context.Background())
	(&Poller{api: api}).Run(context.Background())
	(&Poller{api: api, router: router}).Run(nil)
}

func TestPollerRunErrorBackoffStopsOnCancel(t *testing.T) {
	bot := &failingBot{fail: map[string]bool{"getUpdates": true}}
	server := httptest.NewServer(bot.handler())
	defer server.Close()

	poller := &Poller{
		api:      newAPIClient(server.URL, "token", server.Client()),
		router:   NewRouter(nil, nil),
		logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		allowAny: true,
		pollTime: time.Second,
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		poller.Run(ctx)
	}()
	// 等第一次失败进入退避，再取消：退避的 select 必须被 ctx.Done 打断。
	deadline := time.Now().Add(2 * time.Second)
	for len(bot.byMethod("getUpdates")) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("取消后轮询未退出")
	}
}

func TestPollerRunEmptyBatchWaitsMinInterval(t *testing.T) {
	api := &fakeBotAPI{}
	client, _ := newFakeClient(t, api)
	poller := &Poller{
		api:      client,
		router:   NewRouter(nil, nil),
		logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		allowAny: true,
		pollTime: time.Second,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	started := time.Now()
	poller.Run(ctx)
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("空批次等待过久: %v", elapsed)
	}
	if len(api.byMethod("getUpdates")) == 0 {
		t.Fatal("未发起 getUpdates")
	}
}

func TestPollerDispatchFilters(t *testing.T) {
	api := &fakeBotAPI{}
	client, _ := newFakeClient(t, api)
	router := NewRouter(nil, stubServices{})
	poller := &Poller{
		api:     client,
		router:  router,
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		allowed: map[int64]struct{}{100: {}},
	}

	// 未授权会话、无 chat id 的更新都不触发回复。
	poller.dispatch(context.Background(), textMessage(200, "/help"))
	poller.dispatch(context.Background(), Update{UpdateID: 9})
	if calls := api.byMethod("sendMessage"); len(calls) != 0 {
		t.Fatalf("未授权会话不应回复: %#v", calls)
	}

	poller.dispatch(context.Background(), textMessage(100, "/help"))
	if calls := api.byMethod("sendMessage"); len(calls) != 1 {
		t.Fatalf("授权会话应回复: %#v", calls)
	}

	anyChat := &Poller{api: client, router: router, logger: poller.logger, allowAny: true}
	anyChat.dispatch(context.Background(), textMessage(999, "/help"))
	if calls := api.byMethod("sendMessage"); len(calls) != 2 {
		t.Fatalf("allowAny 应回复: %#v", calls)
	}
}

func TestManagerSyncLifecycle(t *testing.T) {
	api := &fakeBotAPI{}
	client, _ := newFakeClient(t, api)
	router := NewRouter(nil, stubServices{})

	manager := NewManager(nil, nil)
	manager.sync(context.Background(), func(context.Context) []Config {
		return []Config{{Name: "a", Token: "t1", Enabled: true}}
	})
	if len(manager.running) != 0 {
		t.Fatal("nil router 不应启动轮询")
	}

	manager = NewManager(nil, router)
	// source 为 nil 时不启动任何轮询。
	manager.sync(context.Background(), nil)
	if len(manager.running) != 0 {
		t.Fatal("nil source 不应启动轮询")
	}

	// 未启用或空 token 被过滤。
	manager.sync(context.Background(), func(context.Context) []Config {
		return []Config{{Name: "off", Token: "t", Enabled: false}, {Name: "empty", Token: "  ", Enabled: true}}
	})
	if len(manager.running) != 0 {
		t.Fatalf("过滤失败: %d", len(manager.running))
	}

	// 启用后启动一个轮询；重复 sync 不重复启动。
	configs := func(context.Context) []Config {
		return []Config{{Name: "on", Token: "t1", APIURL: client.baseURL, Enabled: true}}
	}
	manager.sync(context.Background(), configs)
	if len(manager.running) != 1 {
		t.Fatalf("轮询数量 = %d", len(manager.running))
	}
	var handle *runningPoller
	for _, item := range manager.running {
		handle = item
	}
	manager.sync(context.Background(), configs)
	if len(manager.running) != 1 {
		t.Fatalf("重复 sync 后数量 = %d", len(manager.running))
	}

	// 配置消失后取消并移除。
	manager.sync(context.Background(), func(context.Context) []Config { return nil })
	if len(manager.running) != 0 {
		t.Fatalf("移除后数量 = %d", len(manager.running))
	}
	select {
	case <-handle.done:
	case <-time.After(3 * time.Second):
		t.Fatal("移除后轮询未退出")
	}

	// Stop 之后 sync 不再启动。
	manager.Stop()
	manager.sync(context.Background(), configs)
	if len(manager.running) != 0 {
		t.Fatal("Stop 后不应再启动轮询")
	}
}

func TestManagerRunStopsOnCancel(t *testing.T) {
	manager := NewManager(nil, NewRouter(nil, nil))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		manager.Run(ctx, func(context.Context) []Config { return nil })
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("取消后 Run 未返回")
	}
	if len(manager.running) != 0 {
		t.Fatalf("取消后仍在运行: %d", len(manager.running))
	}
}

func TestManagerStopNilAndEmpty(t *testing.T) {
	var nilManager *Manager
	nilManager.Stop()
	NewManager(nil, nil).Stop()
}

func TestManagerRecoversPollerPanic(t *testing.T) {
	bot := &failingBot{fail: map[string]bool{}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/getUpdates") {
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": []map[string]any{{
				"update_id": 1,
				"message":   map[string]any{"message_id": 1, "chat": map[string]any{"id": 5}, "text": "/search x"},
			}}})
			return
		}
		bot.handler()(w, r)
	}))
	defer server.Close()

	router := NewRouter(nil, panickingServices{})
	manager := NewManager(nil, router)
	manager.sync(context.Background(), func(context.Context) []Config {
		return []Config{{Name: "panic", Token: "t", APIURL: server.URL, Enabled: true}}
	})
	// 等协程跑到 panic 分支后 Stop，验证不会把进程带走。
	time.Sleep(100 * time.Millisecond)
	var handle *runningPoller
	for _, item := range manager.running {
		handle = item
	}
	manager.Stop()
	if handle == nil {
		t.Fatal("轮询未启动")
	}
	select {
	case <-handle.done:
	case <-time.After(3 * time.Second):
		t.Fatal("panic 后轮询协程未退出")
	}
}

type panickingServices struct{}

func (panickingServices) SearchTitles(context.Context, string, int) ([]SearchResult, error) {
	panic("搜索实现崩了")
}

func (panickingServices) Subscriptions(context.Context) ([]string, error) {
	panic("订阅实现崩了")
}

func (panickingServices) Sites(context.Context) ([]string, error) {
	panic("站点实现崩了")
}

func TestPollerRunSurvivesCanceledContext(t *testing.T) {
	api := &fakeBotAPI{}
	client, _ := newFakeClient(t, api)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	poller := &Poller{
		api:      client,
		router:   NewRouter(nil, nil),
		logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		allowAny: true,
	}
	poller.Run(ctx)
	if len(api.byMethod("getUpdates")) != 0 {
		t.Fatal("已取消上下文不应发起轮询")
	}
}

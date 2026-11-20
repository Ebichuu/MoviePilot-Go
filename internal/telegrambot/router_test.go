package telegrambot

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeBotAPI struct {
	mu       sync.Mutex
	requests []fakeRequest
	updates  [][]Update // one batch per getUpdates call
	calls    int
}

type fakeRequest struct {
	method string
	body   map[string]any
}

func (f *fakeBotAPI) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		body := map[string]any{}
		if len(raw) > 0 {
			_ = json.Unmarshal(raw, &body)
		}
		method := r.URL.Path[strings.LastIndexByte(r.URL.Path, '/')+1:]
		f.mu.Lock()
		f.requests = append(f.requests, fakeRequest{method: method, body: body})
		var result any = map[string]any{"ok": true}
		switch method {
		case "getUpdates":
			index := f.calls
			f.calls++
			if index < len(f.updates) {
				result = f.updates[index]
			} else {
				result = []Update{}
			}
		case "sendMessage":
			result = map[string]any{"message_id": 4242, "chat": map[string]any{"id": body["chat_id"]}}
		case "getMe":
			result = map[string]any{"id": 1, "is_bot": true, "username": "moviepilot_bot"}
		}
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": result})
	}
}

func (f *fakeBotAPI) byMethod(method string) []fakeRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []fakeRequest
	for _, request := range f.requests {
		if request.method == method {
			out = append(out, request)
		}
	}
	return out
}

type stubServices struct {
	results []SearchResult
	err     error
}

func (s stubServices) SearchTitles(context.Context, string, int) ([]SearchResult, error) {
	return s.results, s.err
}

func (s stubServices) Subscriptions(context.Context) ([]string, error) {
	return []string{"沙丘 · 已订阅第 1 季"}, nil
}

func (s stubServices) Sites(context.Context) ([]string, error) {
	return []string{"MTeam · 已启用", "PTT · 已禁用"}, nil
}

func newFakeClient(t *testing.T, api *fakeBotAPI) (*apiClient, func()) {
	t.Helper()
	server := httptest.NewServer(api.handler())
	t.Cleanup(server.Close)
	return newAPIClient(server.URL, "test-token", server.Client()), server.Close
}

func textMessage(chatID int64, text string) Update {
	return Update{
		UpdateID: 1,
		Message: &Message{
			MessageID: 11,
			Chat:      Chat{ID: chatID, Type: "private"},
			From:      &User{ID: 99},
			Text:      text,
		},
	}
}

func TestRouterSearchRepliesWithButtons(t *testing.T) {
	t.Parallel()
	api := &fakeBotAPI{}
	client, _ := newFakeClient(t, api)
	router := NewRouter(slog.Default(), stubServices{results: []SearchResult{
		{Title: "沙丘2.2024.1080p", Site: "MTeam", Size: 13 << 30, DetailURL: "https://example.invalid/1"},
		{Title: "沙丘2.2024.2160p", Site: "PTT", Size: 45 << 30},
	}})

	router.HandleUpdate(context.Background(), client, textMessage(100, "/search 沙丘"))

	sent := api.byMethod("sendMessage")
	if len(sent) != 1 {
		t.Fatalf("sendMessage calls = %d, want 1", len(sent))
	}
	text, _ := sent[0].body["text"].(string)
	if !strings.Contains(text, "沙丘2.2024.1080p") || !strings.Contains(text, "13.0 GB") {
		t.Fatalf("reply text = %q", text)
	}
	markup, ok := sent[0].body["reply_markup"].(map[string]any)
	if !ok {
		t.Fatalf("reply_markup missing: %#v", sent[0].body)
	}
	rows, _ := markup["inline_keyboard"].([]any)
	if len(rows) != 3 {
		t.Fatalf("keyboard rows = %d, want 3 (2 results + close)", len(rows))
	}
	first, _ := rows[0].([]any)
	button, _ := first[0].(map[string]any)
	if button["callback_data"] != "d:0" {
		t.Fatalf("first button = %#v, want d:0", button)
	}
}

func TestRouterCallbackShowsDetailAndAnswers(t *testing.T) {
	t.Parallel()
	api := &fakeBotAPI{}
	client, _ := newFakeClient(t, api)
	router := NewRouter(slog.Default(), stubServices{results: []SearchResult{
		{Title: "沙丘2.2024.1080p", Site: "MTeam", Size: 13 << 30, Seeders: 42, DetailURL: "https://example.invalid/1"},
	}})
	router.HandleUpdate(context.Background(), client, textMessage(100, "/search 沙丘"))

	callback := Update{UpdateID: 2, CallbackQuery: &CallbackQuery{
		ID:      "cb-1",
		From:    User{ID: 99},
		Message: &Message{MessageID: 4242, Chat: Chat{ID: 100}},
		Data:    "d:0",
	}}
	router.HandleUpdate(context.Background(), client, callback)

	if answers := api.byMethod("answerCallbackQuery"); len(answers) != 1 {
		t.Fatalf("answerCallbackQuery = %d, want 1", len(answers))
	}
	edits := api.byMethod("editMessageText")
	if len(edits) != 1 {
		t.Fatalf("editMessageText = %d, want 1", len(edits))
	}
	text, _ := edits[0].body["text"].(string)
	if !strings.Contains(text, "做种：42") || !strings.Contains(text, "MTeam") {
		t.Fatalf("detail text = %q", text)
	}
	if edits[0].body["message_id"] != float64(4242) {
		t.Fatalf("edited message_id = %#v", edits[0].body["message_id"])
	}
}

func TestRouterHelpAndSubscriptionList(t *testing.T) {
	t.Parallel()
	api := &fakeBotAPI{}
	client, _ := newFakeClient(t, api)
	router := NewRouter(slog.Default(), stubServices{})

	router.HandleUpdate(context.Background(), client, textMessage(100, "/help"))
	router.HandleUpdate(context.Background(), client, textMessage(100, "/subs"))
	router.HandleUpdate(context.Background(), client, textMessage(100, "/sites"))

	sent := api.byMethod("sendMessage")
	if len(sent) != 3 {
		t.Fatalf("sendMessage calls = %d, want 3", len(sent))
	}
	if text, _ := sent[0].body["text"].(string); !strings.Contains(text, "/search") {
		t.Fatalf("help text = %q", text)
	}
	if text, _ := sent[1].body["text"].(string); !strings.Contains(text, "沙丘 · 已订阅第 1 季") {
		t.Fatalf("subscriptions text = %q", text)
	}
	if text, _ := sent[2].body["text"].(string); !strings.Contains(text, "MTeam · 已启用") {
		t.Fatalf("sites text = %q", text)
	}
}

func TestRouterSearchErrorAndEmpty(t *testing.T) {
	t.Parallel()
	api := &fakeBotAPI{}
	client, _ := newFakeClient(t, api)
	router := NewRouter(slog.Default(), stubServices{err: errors.New("boom")})
	router.HandleUpdate(context.Background(), client, textMessage(100, "/search 沙丘"))
	router.HandleUpdate(context.Background(), client, textMessage(100, "/search"))

	sent := api.byMethod("sendMessage")
	if len(sent) != 2 {
		t.Fatalf("sendMessage calls = %d, want 2", len(sent))
	}
	if text, _ := sent[0].body["text"].(string); !strings.Contains(text, "搜索失败") {
		t.Fatalf("error text = %q", text)
	}
	if text, _ := sent[1].body["text"].(string); !strings.Contains(text, "用法") {
		t.Fatalf("usage text = %q", text)
	}
}

func TestPollerDispatchesAllowedChatAndAdvancesOffset(t *testing.T) {
	t.Parallel()
	api := &fakeBotAPI{updates: [][]Update{
		{textMessage(100, "/help")},
		{},
	}}
	client, _ := newFakeClient(t, api)
	router := NewRouter(slog.Default(), stubServices{})
	poller := newPoller(Config{Name: "tg", Token: "test-token", ChatID: "100", Enabled: true}, router, slog.Default())
	poller.api = client
	poller.pollTime = 100 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		poller.Run(ctx)
		close(done)
	}()
	deadline := time.After(1500 * time.Millisecond)
	for {
		if len(api.byMethod("sendMessage")) > 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("poller never dispatched the allowed message")
		case <-time.After(20 * time.Millisecond):
		}
	}
	cancel()
	<-done

	if len(api.byMethod("sendMessage")) != 1 {
		t.Fatalf("sendMessage calls = %d, want 1", len(api.byMethod("sendMessage")))
	}
	polls := api.byMethod("getUpdates")
	if len(polls) < 2 {
		t.Fatalf("getUpdates calls = %d, want at least 2", len(polls))
	}
	offset, _ := polls[1].body["offset"].(float64)
	if offset != 2 {
		t.Fatalf("second getUpdates offset = %#v, want 2", polls[1].body["offset"])
	}
}

func TestPollerIgnoresOtherChats(t *testing.T) {
	t.Parallel()
	api := &fakeBotAPI{updates: [][]Update{{textMessage(999, "/help")}, {}}}
	client, _ := newFakeClient(t, api)
	router := NewRouter(slog.Default(), stubServices{})
	poller := newPoller(Config{Name: "tg", Token: "test-token", ChatID: "100", Enabled: true}, router, slog.Default())
	poller.api = client
	poller.pollTime = 50 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
	defer cancel()
	poller.Run(ctx)

	if got := len(api.byMethod("sendMessage")); got != 0 {
		t.Fatalf("sendMessage calls = %d, want 0 for unauthorized chat", got)
	}
}

func TestManagerStartsOnePerTokenAndStops(t *testing.T) {
	t.Parallel()
	api := &fakeBotAPI{}
	_, _ = newFakeClient(t, api)
	router := NewRouter(slog.Default(), stubServices{})
	manager := NewManager(slog.Default(), router)

	ctx, cancel := context.WithCancel(context.Background())
	source := func(context.Context) []Config {
		return []Config{
			{Name: "a", Token: "token-a", ChatID: "1", Enabled: true},
			{Name: "b", Token: "token-a", ChatID: "1", Enabled: true},
			{Name: "c", Token: "", ChatID: "1", Enabled: true},
			{Name: "d", Token: "token-d", ChatID: "1", Enabled: false},
		}
	}
	go manager.Run(ctx, source)

	deadline := time.After(2 * time.Second)
	for {
		manager.mu.Lock()
		count := len(manager.running)
		manager.mu.Unlock()
		if count == 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("running pollers = %d, want 1", count)
		case <-time.After(20 * time.Millisecond):
		}
	}
	cancel()
	manager.Stop()
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if len(manager.running) != 0 {
		t.Fatalf("running pollers after stop = %d, want 0", len(manager.running))
	}
}

func TestParseAllowedChats(t *testing.T) {
	t.Parallel()
	allowed, allowAny := parseAllowedChats("100, 200 300")
	if allowAny {
		t.Fatalf("allowAny = true, want false")
	}
	for _, id := range []int64{100, 200, 300} {
		if _, ok := allowed[id]; !ok {
			t.Fatalf("chat %d missing from allowlist", id)
		}
	}
	if _, allowAny := parseAllowedChats("  "); !allowAny {
		t.Fatalf("empty chat_id should allow any chat")
	}
}

func TestFormatSize(t *testing.T) {
	t.Parallel()
	cases := map[int64]string{
		0:             "未知大小",
		512:           "512.0 B",
		1536:          "1.5 KB",
		13 << 30:      "13.0 GB",
		2 * (1 << 40): "2.0 TB",
	}
	for size, want := range cases {
		if got := formatSize(size); got != want {
			t.Fatalf("formatSize(%d) = %q, want %q", size, got, want)
		}
	}
}

package telegrambot

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// listServices 让订阅/站点列表按用例返回不同结果。
type listServices struct {
	lines []string
	err   error
}

func (s listServices) SearchTitles(context.Context, string, int) ([]SearchResult, error) {
	return nil, nil
}

func (s listServices) Subscriptions(context.Context) ([]string, error) {
	return s.lines, s.err
}

func (s listServices) Sites(context.Context) ([]string, error) {
	return s.lines, s.err
}

func TestHandleMessageBranches(t *testing.T) {
	api := &fakeBotAPI{}
	client, _ := newFakeClient(t, api)
	router := NewRouter(discardLogger(), stubServices{results: []SearchResult{{Title: "沙丘", Site: "MTeam", Size: 1 << 30}}})

	cases := []struct {
		name string
		chat int64
		text string
		want string
	}{
		{"无 chat id", 0, "/help", ""},
		{"普通文本", 7, "你好", "发送 /help 查看可用命令。"},
		{"帮助", 7, "/help", "/search"},
		{"start", 7, "/start", "/search"},
		{"带机器人后缀", 7, "/help@moviepilot_bot", "/search"},
		{"查看 id", 7, "/id", "chat_id: 7"},
		{"版本", 7, "/version", "Telegram 入站已启用"},
		{"未知命令", 7, "/nope", "未知命令 /nope"},
		{"搜索缺参数", 7, "/search", "用法：/search"},
		{"大写命令", 7, "/SEARCH 沙丘", "沙丘"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			before := len(api.byMethod("sendMessage"))
			router.HandleUpdate(context.Background(), client, textMessage(testCase.chat, testCase.text))
			calls := api.byMethod("sendMessage")
			if testCase.want == "" {
				if len(calls) != before {
					t.Fatal("不应发送消息")
				}
				return
			}
			if len(calls) == before {
				t.Fatal("未发送消息")
			}
			text, _ := calls[len(calls)-1].body["text"].(string)
			if !strings.Contains(text, testCase.want) {
				t.Fatalf("文本 = %q，期望包含 %q", text, testCase.want)
			}
		})
	}

	// /id 同时带出 user id。
	router.HandleUpdate(context.Background(), client, textMessage(7, "/id"))
	calls := api.byMethod("sendMessage")
	text, _ := calls[len(calls)-1].body["text"].(string)
	if !strings.Contains(text, "user_id: 99") {
		t.Fatalf("id 文本 = %q", text)
	}
}

func TestHandleSearchWithoutServices(t *testing.T) {
	api := &fakeBotAPI{}
	client, _ := newFakeClient(t, api)
	NewRouter(discardLogger(), nil).HandleUpdate(context.Background(), client, textMessage(7, "/search 沙丘"))
	calls := api.byMethod("sendMessage")
	if len(calls) != 1 {
		t.Fatalf("调用次数 = %d", len(calls))
	}
	if text, _ := calls[0].body["text"].(string); !strings.Contains(text, "搜索服务未就绪") {
		t.Fatalf("文本 = %q", text)
	}
}

func TestHandleListBranches(t *testing.T) {
	cases := []struct {
		name     string
		services Services
		want     string
	}{
		{"服务未注入", nil, "订阅查询未就绪。"},
		{"查询失败", listServices{err: errors.New("db down")}, "订阅查询失败"},
		{"空列表", listServices{lines: []string{}}, "暂无订阅。"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			api := &fakeBotAPI{}
			client, _ := newFakeClient(t, api)
			NewRouter(discardLogger(), testCase.services).HandleUpdate(context.Background(), client, textMessage(7, "/subs"))
			calls := api.byMethod("sendMessage")
			if len(calls) != 1 {
				t.Fatalf("调用次数 = %d", len(calls))
			}
			if text, _ := calls[0].body["text"].(string); !strings.Contains(text, testCase.want) {
				t.Fatalf("文本 = %q，期望 %q", text, testCase.want)
			}
		})
	}

	// 超过 30 条时截断并追加提示（头部计数含提示行）。
	lines := make([]string, 35)
	for index := range lines {
		lines[index] = "条目"
	}
	api := &fakeBotAPI{}
	client, _ := newFakeClient(t, api)
	NewRouter(discardLogger(), listServices{lines: lines}).HandleUpdate(context.Background(), client, textMessage(7, "/sites"))
	calls := api.byMethod("sendMessage")
	text, _ := calls[len(calls)-1].body["text"].(string)
	if !strings.Contains(text, "站点（31）") || !strings.Contains(text, "…其余 5 条请在 Web 端查看") {
		t.Fatalf("截断文本 = %q", text)
	}
}

func TestHandleCallbackBranches(t *testing.T) {
	api := &fakeBotAPI{}
	client, _ := newFakeClient(t, api)
	router := NewRouter(discardLogger(), stubServices{})
	results := make([]SearchResult, 12)
	for index := range results {
		results[index] = SearchResult{Title: "标题", Site: "站点", Size: int64(index+1) << 20}
	}
	router.storeSession(7, "沙丘", results)

	// 收起按钮：删除消息。
	router.HandleUpdate(context.Background(), client, callbackQuery(7, 21, "x"))
	if len(api.byMethod("deleteMessage")) != 1 {
		t.Fatalf("收起未删除消息: %#v", api.byMethod("deleteMessage"))
	}
	// 没有消息的收起：只应答。
	router.HandleUpdate(context.Background(), client, callbackQuery(0, 0, "x"))
	if len(api.byMethod("deleteMessage")) != 1 {
		t.Fatal("无消息时不应删除")
	}

	cases := []struct {
		name string
		data string
		want string
	}{
		{"翻页", "p:1", "第 2/3 页"},
		{"翻页越界", "p:99", "第 3/3 页"},
		{"返回列表", "b:0", "第 1/3 页"},
		{"详情", "d:0", "下载请回到 Web 端"},
		{"未知按钮", "zzz", "按钮已失效"},
		{"非法页码", "p:abc", ""},
		{"非法详情", "d:abc", ""},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if testCase.want == "" {
				before := len(api.byMethod("sendMessage"))
				router.HandleUpdate(context.Background(), client, callbackQuery(7, 0, testCase.data))
				if len(api.byMethod("sendMessage")) != before {
					t.Fatal("非法按钮不应回复")
				}
				return
			}
			router.HandleUpdate(context.Background(), client, callbackQuery(7, 0, testCase.data))
			calls := api.byMethod("sendMessage")
			if len(calls) == 0 {
				t.Fatal("未回复")
			}
			text, _ := calls[len(calls)-1].body["text"].(string)
			if !strings.Contains(text, testCase.want) {
				t.Fatalf("文本 = %q，期望 %q", text, testCase.want)
			}
		})
	}

	// 没有 chat id 的未知按钮不回复。
	before := len(api.byMethod("sendMessage"))
	router.HandleUpdate(context.Background(), client, callbackQuery(0, 0, "zzz"))
	if len(api.byMethod("sendMessage")) != before {
		t.Fatal("无 chat id 不应回复")
	}

	// 上下文中没有会话时提示重新搜索。
	emptyRouter := NewRouter(discardLogger(), stubServices{})
	emptyRouter.HandleUpdate(context.Background(), client, callbackQuery(8, 5, "p:0"))
	calls := api.byMethod("sendMessage")
	text, _ := calls[len(calls)-1].body["text"].(string)
	if !strings.Contains(text, "搜索上下文已过期") {
		t.Fatalf("过期会话 = %q", text)
	}
}

func TestRenderSearchPageEditFallback(t *testing.T) {
	results := make([]SearchResult, 12)
	for index := range results {
		results[index] = SearchResult{Title: "标题", Site: "站点", Size: int64(index+1) << 20, DetailURL: "https://example.test/detail"}
	}

	// 编辑成功时不再补发新消息。
	api := &fakeBotAPI{}
	client, _ := newFakeClient(t, api)
	router := NewRouter(discardLogger(), stubServices{})
	router.storeSession(7, "沙丘", results)
	router.renderSearchPage(context.Background(), client, 7, 33, 0)
	if len(api.byMethod("sendMessage")) != 0 || len(api.byMethod("editMessageText")) != 1 {
		t.Fatalf("编辑路径 = send:%d edit:%d", len(api.byMethod("sendMessage")), len(api.byMethod("editMessageText")))
	}

	// 编辑失败时回落到新消息，并把负数页码夹到第一页。
	failing, failingBot := newFailingClient(t, "editMessageText")
	router.renderSearchPage(context.Background(), failing, 7, 33, -5)
	if len(failingBot.byMethod("sendMessage")) != 1 || len(failingBot.byMethod("editMessageText")) != 1 {
		t.Fatalf("回落路径 = send:%d edit:%d", len(failingBot.byMethod("sendMessage")), len(failingBot.byMethod("editMessageText")))
	}
	text, _ := failingBot.byMethod("sendMessage")[0].body["text"].(string)
	if !strings.Contains(text, "第 1/3 页") {
		t.Fatalf("页码 = %q", text)
	}
	// 键盘结构：12 条结果每页 5 条，第一页应有 5 个详情行 + 下一页 + 收起。
	keyboard, ok := failingBot.byMethod("sendMessage")[0].body["reply_markup"].(map[string]any)
	if !ok {
		t.Fatalf("缺少键盘: %#v", failingBot.byMethod("sendMessage")[0].body)
	}
	rows, _ := keyboard["inline_keyboard"].([]any)
	if len(rows) != 7 {
		t.Fatalf("键盘行数 = %d", len(rows))
	}
}

func TestRenderDetailBranches(t *testing.T) {
	api := &fakeBotAPI{}
	client, _ := newFakeClient(t, api)
	router := NewRouter(discardLogger(), stubServices{})

	// 无会话、越界下标都提示过期。
	for _, index := range []int{0, -1, 99} {
		router.renderDetail(context.Background(), client, 7, 0, index)
		calls := api.byMethod("sendMessage")
		text, _ := calls[len(calls)-1].body["text"].(string)
		if !strings.Contains(text, "该结果已过期") {
			t.Fatalf("index=%d 文本 = %q", index, text)
		}
	}

	router.storeSession(7, "沙丘", []SearchResult{{Title: "沙丘", Site: "MTeam", Size: 1 << 30}})
	router.renderDetail(context.Background(), client, 7, 0, 0)
	calls := api.byMethod("sendMessage")
	text, _ := calls[len(calls)-1].body["text"].(string)
	if strings.Contains(text, "做种：") || strings.Contains(text, "详情页：") {
		t.Fatalf("空字段不应出现: %q", text)
	}

	router.storeSession(7, "沙丘", []SearchResult{{
		Title: "沙丘", Site: "MTeam", Size: 1 << 30, Seeders: 5, DetailURL: "https://example.test/d",
	}})
	router.renderDetail(context.Background(), client, 7, 0, 0)
	calls = api.byMethod("sendMessage")
	text, _ = calls[len(calls)-1].body["text"].(string)
	if !strings.Contains(text, "做种：5") || !strings.Contains(text, "详情页：https://example.test/d") {
		t.Fatalf("详情文本 = %q", text)
	}

	// 带 messageID 时走编辑。
	before := len(api.byMethod("sendMessage"))
	router.renderDetail(context.Background(), client, 7, 44, 0)
	if len(api.byMethod("sendMessage")) != before || len(api.byMethod("editMessageText")) == 0 {
		t.Fatal("应编辑已有消息")
	}
}

func TestStoreAndLoadSessionLifecycle(t *testing.T) {
	router := NewRouter(discardLogger(), stubServices{})
	now := time.Now()
	router.now = func() time.Time { return now }

	// 超过 30 分钟的会话在写入时被清理。
	router.sessions[1] = &searchSession{keyword: "旧", touched: now.Add(-31 * time.Minute)}
	router.storeSession(2, "新", nil)
	if _, ok := router.sessions[1]; ok {
		t.Fatal("过期会话未被清理")
	}
	if session := router.loadSession(2); session == nil || session.keyword != "新" {
		t.Fatalf("会话 = %#v", session)
	}

	// 上限 64：写满后再写入会淘汰一个旧会话。
	for index := int64(10); index < 10+searchSessionLimit; index++ {
		router.storeSession(index, "k", nil)
	}
	if len(router.sessions) < searchSessionLimit {
		t.Fatalf("会话数量 = %d", len(router.sessions))
	}
	router.storeSession(999, "溢出", nil)
	if len(router.sessions) != searchSessionLimit {
		t.Fatalf("淘汰后数量 = %d", len(router.sessions))
	}

	// 读取时过期会被删除并返回 nil。
	router.sessions[1000] = &searchSession{touched: now.Add(-time.Hour)}
	if session := router.loadSession(1000); session != nil {
		t.Fatalf("过期会话 = %#v", session)
	}
	if _, ok := router.sessions[1000]; ok {
		t.Fatal("过期会话未删除")
	}

	// 读取会刷新 touched。
	old := now.Add(-10 * time.Minute)
	router.sessions[1001] = &searchSession{touched: old}
	if session := router.loadSession(1001); session == nil || !session.touched.After(old) {
		t.Fatalf("刷新失败: %#v", session)
	}
}

func TestSplitCommand(t *testing.T) {
	cases := []struct {
		text     string
		command  string
		argument string
	}{
		{"", "", ""},
		{"   ", "", ""},
		{"/help", "/help", ""},
		{"/SEARCH  沙  丘 ", "/search", "沙 丘"},
		{"/search@moviepilot_bot 沙丘", "/search", "沙丘"},
		{"裸文本 参数", "裸文本", "参数"},
	}
	for _, testCase := range cases {
		command, argument := splitCommand(testCase.text)
		if command != testCase.command || argument != testCase.argument {
			t.Fatalf("splitCommand(%q) = (%q, %q), want (%q, %q)",
				testCase.text, command, argument, testCase.command, testCase.argument)
		}
	}
}

func TestHandleCallbackFailureLogging(t *testing.T) {
	// 应答与删除都失败时只记 debug 日志，不影响后续处理。
	client, bot := newFailingClient(t, "answerCallbackQuery", "deleteMessage")
	router := NewRouter(discardLogger(), stubServices{})
	router.storeSession(7, "沙丘", []SearchResult{{Title: "沙丘", Site: "MTeam"}})
	router.HandleUpdate(context.Background(), client, callbackQuery(7, 21, "x"))
	if len(bot.byMethod("deleteMessage")) != 1 || len(bot.byMethod("answerCallbackQuery")) != 1 {
		t.Fatalf("调用 = delete:%d answer:%d", len(bot.byMethod("deleteMessage")), len(bot.byMethod("answerCallbackQuery")))
	}
}

func TestReplyLogsFailureWithoutPanicking(t *testing.T) {
	router := NewRouter(discardLogger(), stubServices{})
	client := newAPIClient("http://127.0.0.1:1", "", nil)
	router.reply(context.Background(), client, 7, "文本", nil)
}

func TestHandleUpdateGuards(t *testing.T) {
	var nilRouter *Router
	nilRouter.HandleUpdate(context.Background(), nil, Update{})
	router := NewRouter(discardLogger(), stubServices{})
	router.HandleUpdate(context.Background(), nil, textMessage(7, "/help"))
	router.HandleUpdate(context.Background(), newAPIClient("http://127.0.0.1:1", "t", nil), Update{UpdateID: 3})
}

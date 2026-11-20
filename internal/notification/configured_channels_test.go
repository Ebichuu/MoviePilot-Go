package notification

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/moviepilot-go/moviepilot-go/internal/notificationconfig"
)

func buildChannelSender(t *testing.T, channel Channel, config map[string]any) (*configuredChannelSender, bool) {
	t.Helper()
	sender, ok := newConfiguredChannelSender(slog.New(slog.NewJSONHandler(io.Discard, nil)), channel,
		notificationconfig.Config{Name: "test", Config: config})
	if !ok {
		return nil, false
	}
	return sender.(*configuredChannelSender), true
}

func TestNewConfiguredChannelSenderValidation(t *testing.T) {
	cases := []struct {
		name    string
		channel Channel
		config  map[string]any
		want    bool
	}{
		{"discord 有 token", ChannelDiscord, map[string]any{"DISCORD_BOT_TOKEN": "t"}, true},
		{"discord 空 token", ChannelDiscord, map[string]any{"DISCORD_BOT_TOKEN": "  "}, false},
		{"feishu 完整", ChannelFeishu, map[string]any{
			"FEISHU_APP_ID": "id", "FEISHU_APP_SECRET": "s", "FEISHU_OPEN_ID": "o"}, true},
		{"feishu 只有 chat_id", ChannelFeishu, map[string]any{
			"FEISHU_APP_ID": "id", "FEISHU_APP_SECRET": "s", "FEISHU_CHAT_ID": "c"}, true},
		{"feishu 缺接收方", ChannelFeishu, map[string]any{
			"FEISHU_APP_ID": "id", "FEISHU_APP_SECRET": "s"}, false},
		{"feishu 缺 secret", ChannelFeishu, map[string]any{
			"FEISHU_APP_ID": "id", "FEISHU_OPEN_ID": "o"}, false},
		{"slack 有 token", ChannelSlack, map[string]any{"SLACK_OAUTH_TOKEN": "t"}, true},
		{"synology 完整", ChannelSynologyChat, map[string]any{
			"SYNOLOGYCHAT_WEBHOOK": "https://nas.local/webhook", "SYNOLOGYCHAT_TOKEN": "t"}, true},
		{"synology 缺 token", ChannelSynologyChat, map[string]any{
			"SYNOLOGYCHAT_WEBHOOK": "https://nas.local/webhook"}, false},
		{"vocechat 完整", ChannelVoceChat, map[string]any{
			"VOCECHAT_HOST": "https://chat.local", "VOCECHAT_API_KEY": "k", "VOCECHAT_CHANNEL_ID": "1"}, true},
		{"vocechat 缺 channel", ChannelVoceChat, map[string]any{
			"VOCECHAT_HOST": "https://chat.local", "VOCECHAT_API_KEY": "k"}, false},
		{"qq 完整", ChannelQQ, map[string]any{"QQ_APP_ID": "a", "QQ_APP_SECRET": "s"}, true},
		{"qq 缺 secret", ChannelQQ, map[string]any{"QQ_APP_ID": "a"}, false},
		{"wechat 企业应用", ChannelWeChat, map[string]any{
			"WECHAT_CORPID": "c", "WECHAT_APP_SECRET": "s", "WECHAT_APP_ID": "a"}, true},
		// Bot 模式走 websocket，不是 HTTP 发送器：必须判定为不可用。
		{"wechat bot 模式", ChannelWeChat, map[string]any{
			"WECHAT_MODE": "bot", "WECHAT_CORPID": "c", "WECHAT_APP_SECRET": "s", "WECHAT_APP_ID": "a"}, false},
		{"未知渠道", Channel("other"), map[string]any{"X": "y"}, false},
	}
	for _, testCase := range cases {
		sender, ok := buildChannelSender(t, testCase.channel, testCase.config)
		if ok != testCase.want {
			t.Fatalf("%s: ok = %v，期望 %v", testCase.name, ok, testCase.want)
		}
		if ok {
			if sender.Name() != testCase.channel {
				t.Fatalf("%s: Name() = %q", testCase.name, sender.Name())
			}
			if sender.client == nil || sender.client.Timeout == 0 {
				t.Fatalf("%s: 必须带超时的 HTTP 客户端", testCase.name)
			}
		} else if sender != nil {
			t.Fatalf("%s: 无效配置不应返回发送器", testCase.name)
		}
	}
}

func TestConfiguredChannelValueNormalization(t *testing.T) {
	sender := &configuredChannelSender{config: map[string]any{
		"text":    "  value  ",
		"number":  json.Number("42"),
		"integer": 7,
		"flag":    true,
		"nil":     nil,
	}}
	cases := map[string]string{
		"text": "value", "number": "42", "integer": "7", "flag": "true",
		"nil": "", "missing": "",
	}
	for key, want := range cases {
		if got := sender.value(key); got != want {
			t.Fatalf("value(%q) = %q，期望 %q", key, got, want)
		}
	}
}

func TestConfiguredChannelSendGuards(t *testing.T) {
	sender := &configuredChannelSender{channel: ChannelDiscord, client: &http.Client{}}
	if err := sender.Send(nil, Message{}); err == nil {
		t.Fatal("nil 上下文应报错")
	}
	if err := (&configuredChannelSender{}).Send(context.Background(), Message{}); err == nil {
		t.Fatal("nil 客户端应报错")
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sender.Send(cancelled, Message{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("已取消上下文错误 = %v", err)
	}
	unknown := &configuredChannelSender{channel: Channel("nope"), client: &http.Client{}}
	if err := unknown.Send(context.Background(), Message{}); err == nil {
		t.Fatal("未知渠道应报错")
	}
}

func TestConfiguredChannelPostJSONContract(t *testing.T) {
	type response struct {
		body   string
		status int
	}
	cases := []struct {
		name     string
		response response
		wantErr  bool
	}{
		{"空响应体成功", response{"", http.StatusOK}, false},
		{"ok 为真", response{`{"ok":true}`, http.StatusOK}, false},
		{"ok 为假", response{`{"ok":false}`, http.StatusOK}, true},
		{"code 为 0", response{`{"code":0}`, http.StatusOK}, false},
		{"code 非 0", response{`{"code":1}`, http.StatusOK}, true},
		{"errcode 为 0", response{`{"errcode":0}`, http.StatusOK}, false},
		{"errcode 非 0", response{`{"errcode":40013}`, http.StatusOK}, true},
		{"HTTP 500", response{`{}`, http.StatusInternalServerError}, true},
		{"非 JSON", response{`not-json`, http.StatusOK}, true},
	}
	for _, testCase := range cases {
		var gotHeader, gotBody string
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotHeader = r.Header.Get("Authorization")
			payload, _ := io.ReadAll(r.Body)
			gotBody = string(payload)
			if r.Header.Get("Content-Type") != "application/json" {
				t.Errorf("Content-Type = %q", r.Header.Get("Content-Type"))
			}
			w.WriteHeader(testCase.response.status)
			_, _ = w.Write([]byte(testCase.response.body))
		}))

		sender := &configuredChannelSender{client: server.Client()}
		_, err := sender.postJSON(context.Background(), server.URL, map[string]string{"text": "hi"},
			map[string]string{"Authorization": "Bearer token"})
		server.Close()
		if (err != nil) != testCase.wantErr {
			t.Fatalf("%s: err = %v，期望错误=%v", testCase.name, err, testCase.wantErr)
		}
		if gotHeader != "Bearer token" {
			t.Fatalf("%s: 自定义头未透传: %q", testCase.name, gotHeader)
		}
		if !strings.Contains(gotBody, `"text":"hi"`) {
			t.Fatalf("%s: 请求体 = %q", testCase.name, gotBody)
		}
	}
}

func TestConfiguredChannelGetJSONAndArray(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/object":
			_, _ = w.Write([]byte(`{"code":0,"value":"x"}`))
		case "/rejected":
			_, _ = w.Write([]byte(`{"code":5}`))
		case "/array":
			_, _ = w.Write([]byte(`[{"id":"1"},{"id":"2"}]`))
		case "/not-array":
			_, _ = w.Write([]byte(`{"id":"1"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	sender := &configuredChannelSender{client: server.Client()}

	result, err := sender.getJSON(context.Background(), server.URL+"/object", nil)
	if err != nil || result["value"] != "x" {
		t.Fatalf("getJSON = %v %v", result, err)
	}
	if _, err := sender.getJSON(context.Background(), server.URL+"/rejected", nil); err == nil {
		t.Fatal("code 非 0 的响应应判为拒绝")
	}
	if _, err := sender.getJSON(context.Background(), server.URL+"/missing", nil); err == nil {
		t.Fatal("404 应报错")
	}

	items, err := sender.getJSONArray(context.Background(), server.URL+"/array", nil)
	if err != nil || len(items) != 2 {
		t.Fatalf("getJSONArray = %v %v", items, err)
	}
	// 期望数组时拿到对象必须报错，不能当成空列表放过。
	if _, err := sender.getJSONArray(context.Background(), server.URL+"/not-array", nil); err == nil {
		t.Fatal("对象响应应判为非法")
	}
}

func TestSendSynologyChatFetchesRecipientsAndPosts(t *testing.T) {
	var (
		listQuery url.Values
		formValue url.Values
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			listQuery = r.URL.Query()
			_, _ = w.Write([]byte(`{"data":{"users":[{"user_id":7},{"user_id":9,"deleted":true},{"user_id":0}]}}`))
		default:
			_ = r.ParseForm()
			formValue = r.PostForm
			_, _ = w.Write([]byte(`{"error":{"code":0}}`))
		}
	}))
	defer server.Close()

	sender := &configuredChannelSender{
		channel: ChannelSynologyChat,
		client:  server.Client(),
		config: map[string]any{
			"SYNOLOGYCHAT_WEBHOOK": server.URL + "/webhook",
			"SYNOLOGYCHAT_TOKEN":   "tk",
		},
	}

	err := sender.Send(context.Background(), Message{Title: "标题", Text: "正文", Image: "https://img/x.jpg", Link: "https://site/1"})
	if err != nil {
		t.Fatalf("发送失败: %v", err)
	}
	if listQuery.Get("api") != "SYNO.Chat.External" || listQuery.Get("method") != "user_list" || listQuery.Get("token") != "tk" {
		t.Fatalf("用户列表请求参数 = %v", listQuery)
	}
	if len(formValue["payload"]) != 1 {
		t.Fatalf("payload 字段 = %v", formValue)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(formValue["payload"][0]), &payload); err != nil {
		t.Fatalf("payload 不是 JSON: %v", err)
	}
	// 已删除用户与 id=0 必须被过滤掉。
	targets, _ := payload["user_ids"].([]any)
	if len(targets) != 1 || targets[0].(float64) != 7 {
		t.Fatalf("接收人 = %v", payload["user_ids"])
	}
	text, _ := payload["text"].(string)
	if !strings.Contains(text, "标题") || !strings.Contains(text, "查看详情") {
		t.Fatalf("正文 = %q", text)
	}
	if payload["file_url"] != "https://img/x.jpg" {
		t.Fatalf("图片字段 = %v", payload["file_url"])
	}
}

func TestSendSynologyChatRejections(t *testing.T) {
	// 非法 webhook：必须在发请求前就拒绝。
	sender := &configuredChannelSender{channel: ChannelSynologyChat, client: &http.Client{}, config: map[string]any{
		"SYNOLOGYCHAT_WEBHOOK": "not-a-url", "SYNOLOGYCHAT_TOKEN": "t",
	}}
	if err := sender.Send(context.Background(), Message{Title: "t"}); err == nil {
		t.Fatal("非法 webhook 应报错")
	}
	// 非数字接收人。
	sender.config["SYNOLOGYCHAT_WEBHOOK"] = "https://nas.local/webhook"
	if err := sender.Send(context.Background(), Message{UserID: "abc"}); err == nil {
		t.Fatal("非法接收人应报错")
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// 空用户列表 → 没有接收人。
		_, _ = w.Write([]byte(`{"data":{"users":[]}}`))
	}))
	defer server.Close()
	sender.client = server.Client()
	sender.config["SYNOLOGYCHAT_WEBHOOK"] = server.URL + "/webhook"
	if err := sender.Send(context.Background(), Message{Title: "t"}); err == nil {
		t.Fatal("没有接收人时应报错")
	}

	// 服务端返回 error.code 非 0 → 判为拒绝。
	rejecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"data":{"users":[{"user_id":7}]}}`))
			return
		}
		_, _ = w.Write([]byte(`{"error":{"code":12}}`))
	}))
	defer rejecting.Close()
	sender.client = rejecting.Client()
	sender.config["SYNOLOGYCHAT_WEBHOOK"] = rejecting.URL + "/webhook"
	if err := sender.Send(context.Background(), Message{Title: "t"}); err == nil {
		t.Fatal("error.code 非 0 应报错")
	}
}

func TestSendVoceChatPayloadAndEndpoint(t *testing.T) {
	var (
		gotPath    string
		gotAPIKey  string
		gotType    string
		gotContent string
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAPIKey = r.Header.Get("x-api-key")
		gotType = r.Header.Get("Content-Type")
		payload, _ := io.ReadAll(r.Body)
		gotContent = string(payload)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()

	sender := &configuredChannelSender{
		channel: ChannelVoceChat,
		client:  server.Client(),
		config: map[string]any{
			"VOCECHAT_HOST":       server.URL,
			"VOCECHAT_API_KEY":    "key-1",
			"VOCECHAT_CHANNEL_ID": "5",
		},
	}

	err := sender.Send(context.Background(), Message{
		Title: "标题", Text: "正文", Image: "https://img/x.png", Link: "https://site/2",
	})
	if err != nil {
		t.Fatalf("发送失败: %v", err)
	}
	if gotPath != "/api/bot/send_to_group/5" {
		t.Fatalf("路径 = %q", gotPath)
	}
	if gotAPIKey != "key-1" {
		t.Fatalf("x-api-key = %q", gotAPIKey)
	}
	// VoceChat 用 markdown 正文而不是 JSON。
	if !strings.HasPrefix(gotType, "text/markdown") {
		t.Fatalf("Content-Type = %q", gotType)
	}
	for _, needle := range []string{"**标题**", "正文", "![](https://img/x.png)", "[查看详情](https://site/2)"} {
		if !strings.Contains(gotContent, needle) {
			t.Fatalf("正文缺少 %q: %q", needle, gotContent)
		}
	}

	// 指定 UID# 时改走单聊接口。
	if err := sender.Send(context.Background(), Message{UserID: "UID#42", Title: "t"}); err != nil {
		t.Fatalf("单聊发送失败: %v", err)
	}
	if gotPath != "/api/bot/send_to_user/42" {
		t.Fatalf("单聊路径 = %q", gotPath)
	}
}

func TestSendVoceChatRejections(t *testing.T) {
	sender := &configuredChannelSender{
		channel: ChannelVoceChat,
		client:  &http.Client{},
		config:  map[string]any{"VOCECHAT_HOST": "ftp://bad"},
	}
	if err := sender.Send(context.Background(), Message{Title: "t"}); err == nil {
		t.Fatal("非法 host 应报错")
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()
	sender.client = server.Client()
	sender.config["VOCECHAT_HOST"] = server.URL
	if err := sender.Send(context.Background(), Message{Title: "t"}); err == nil {
		t.Fatal("非 200 应报错")
	}
}

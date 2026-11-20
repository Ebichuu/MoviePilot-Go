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
	"sync"
	"testing"
	"time"

	"github.com/moviepilot-go/moviepilot-go/internal/notificationconfig"
)

// rewriteTransport 把硬编码的官方域名重定向到本地桩服务器，
// 这样 Discord/Feishu/Slack/QQ/WeChat 的固定 endpoint 也能被测试打到。
type rewriteTransport struct{ target *url.URL }

func (t rewriteTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	clone := request.Clone(request.Context())
	clone.URL.Scheme = t.target.Scheme
	clone.URL.Host = t.target.Host
	clone.Host = ""
	return http.DefaultTransport.RoundTrip(clone)
}

type siteRequest struct {
	method string
	path   string
	query  string
	auth   string
	ctype  string
	body   []byte
}

type siteFixture struct {
	sender *configuredChannelSender
	mu     sync.Mutex
	sent   []siteRequest
}

func (f *siteFixture) calls() []siteRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]siteRequest(nil), f.sent...)
}

func (f *siteFixture) last() siteRequest {
	calls := f.calls()
	if len(calls) == 0 {
		return siteRequest{}
	}
	return calls[len(calls)-1]
}

func newSiteFixture(t *testing.T, channel Channel, config map[string]any, handler http.HandlerFunc) *siteFixture {
	t.Helper()
	fixture := &siteFixture{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		fixture.mu.Lock()
		fixture.sent = append(fixture.sent, siteRequest{
			method: r.Method, path: r.URL.Path, query: r.URL.RawQuery,
			auth: r.Header.Get("Authorization"), ctype: r.Header.Get("Content-Type"), body: body,
		})
		fixture.mu.Unlock()
		if handler != nil {
			handler(w, r)
			return
		}
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	t.Cleanup(server.Close)
	target, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("解析桩地址: %v", err)
	}
	fixture.sender = &configuredChannelSender{
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		channel: channel,
		name:    "测试渠道",
		config:  config,
		client:  &http.Client{Transport: rewriteTransport{target: target}, Timeout: 5 * time.Second},
	}
	return fixture
}

func TestSendDiscord(t *testing.T) {
	message := Message{Title: "标题", Text: "正文", Link: "https://example.test/p"}

	t.Run("使用配置的频道", func(t *testing.T) {
		fixture := newSiteFixture(t, ChannelDiscord, map[string]any{
			"DISCORD_BOT_TOKEN": "token", "DISCORD_CHANNEL_ID": "C1",
		}, nil)
		if err := fixture.sender.Send(context.Background(), message); err != nil {
			t.Fatalf("Send: %v", err)
		}
		call := fixture.last()
		if call.method != http.MethodPost || call.path != "/api/v10/channels/C1/messages" || call.auth != "Bot token" {
			t.Fatalf("请求 = %#v", call)
		}
		if !strings.Contains(string(call.body), `"content":"标题\n正文\nhttps://example.test/p"`) {
			t.Fatalf("请求体 = %s", call.body)
		}
		if !strings.Contains(string(call.body), `"allowed_mentions":{"parse":[]}`) {
			t.Fatalf("allowed_mentions = %s", call.body)
		}
	})

	t.Run("私聊需要先建会话", func(t *testing.T) {
		fixture := newSiteFixture(t, ChannelDiscord, map[string]any{
			"DISCORD_BOT_TOKEN": "token", "DISCORD_CHANNEL_ID": "C1",
		}, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/v10/users/@me/channels" {
				_, _ = io.WriteString(w, `{"ok":true,"id":"DM7"}`)
				return
			}
			_, _ = io.WriteString(w, `{"ok":true}`)
		})
		if err := fixture.sender.Send(context.Background(), Message{UserID: "U7", Text: "hi"}); err != nil {
			t.Fatalf("Send: %v", err)
		}
		calls := fixture.calls()
		if len(calls) != 2 || calls[0].path != "/api/v10/users/@me/channels" ||
			calls[1].path != "/api/v10/channels/DM7/messages" {
			t.Fatalf("调用 = %#v", calls)
		}
		if !strings.Contains(string(calls[0].body), `"recipient_id":"U7"`) {
			t.Fatalf("建会话请求体 = %s", calls[0].body)
		}
	})

	t.Run("无频道时按公会首个文字频道", func(t *testing.T) {
		fixture := newSiteFixture(t, ChannelDiscord, map[string]any{
			"DISCORD_BOT_TOKEN": "token", "DISCORD_GUILD_ID": "G1",
		}, func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/channels") {
				_, _ = io.WriteString(w, `[{"type":2,"id":"VOICE"},{"type":0,"id":"TEXT"}]`)
				return
			}
			_, _ = io.WriteString(w, `{"ok":true}`)
		})
		if err := fixture.sender.Send(context.Background(), Message{Text: "hi"}); err != nil {
			t.Fatalf("Send: %v", err)
		}
		if fixture.last().path != "/api/v10/channels/TEXT/messages" {
			t.Fatalf("目标 = %q", fixture.last().path)
		}
	})

	t.Run("无目的地", func(t *testing.T) {
		fixture := newSiteFixture(t, ChannelDiscord, map[string]any{"DISCORD_BOT_TOKEN": "token"}, nil)
		if err := fixture.sender.Send(context.Background(), Message{Text: "hi"}); err == nil ||
			!strings.Contains(err.Error(), "discord destination is not configured") {
			t.Fatalf("错误 = %v", err)
		}
	})

	t.Run("图片走 embed 且超长正文被截断", func(t *testing.T) {
		fixture := newSiteFixture(t, ChannelDiscord, map[string]any{
			"DISCORD_BOT_TOKEN": "token", "DISCORD_CHANNEL_ID": "C1",
		}, nil)
		if err := fixture.sender.Send(context.Background(), Message{
			Title: "标题", Text: "正文", Image: "https://example.test/i.png", Link: "https://example.test/p",
		}); err != nil {
			t.Fatalf("Send: %v", err)
		}
		body := string(fixture.last().body)
		if !strings.Contains(body, `"embeds"`) ||
			!strings.Contains(body, `"image":{"url":"https://example.test/i.png"}`) ||
			!strings.Contains(body, `"url":"https://example.test/p"`) {
			t.Fatalf("embed 请求体 = %s", body)
		}

		long := strings.Repeat("a", 2100) // 按字节截断，用单字节字符便于精确断言
		if err := fixture.sender.Send(context.Background(), Message{Text: long}); err != nil {
			t.Fatalf("超长正文 = %v", err)
		}
		var payload map[string]any
		if err := json.Unmarshal(fixture.last().body, &payload); err != nil {
			t.Fatalf("解析请求体: %v", err)
		}
		if content, _ := payload["content"].(string); len(content) != 2000 {
			t.Fatalf("正文长度 = %d", len(content))
		}
	})

	t.Run("建会话失败", func(t *testing.T) {
		fixture := newSiteFixture(t, ChannelDiscord, map[string]any{
			"DISCORD_BOT_TOKEN": "token", "DISCORD_CHANNEL_ID": "C1",
		}, func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, `{"ok":false,"description":"nope"}`)
		})
		if err := fixture.sender.Send(context.Background(), Message{UserID: "U7"}); err == nil {
			t.Fatal("应返回错误")
		}
	})
}

func TestSendFeishu(t *testing.T) {
	feishuHandler := func(tokenPayload, messagePayload string) func(http.ResponseWriter, *http.Request) {
		return func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.Contains(r.URL.Path, "tenant_access_token"):
				_, _ = io.WriteString(w, tokenPayload)
			case strings.Contains(r.URL.Path, "im/v1/messages"):
				_, _ = io.WriteString(w, messagePayload)
			default:
				_, _ = io.WriteString(w, `{"ok":true}`)
			}
		}
	}
	tokenOK := feishuHandler(`{"ok":true,"tenant_access_token":"ft","expire":"7200"}`, `{"code":0}`)

	t.Run("使用配置的 chat id 并缓存 token", func(t *testing.T) {
		fixture := newSiteFixture(t, ChannelFeishu, map[string]any{
			"FEISHU_APP_ID": "app", "FEISHU_APP_SECRET": "secret",
			"FEISHU_CHAT_ID": "oc_1", "FEISHU_OPEN_ID": "ou_1",
		}, tokenOK)
		if err := fixture.sender.Send(context.Background(), Message{Title: "标题", Text: "正文"}); err != nil {
			t.Fatalf("Send: %v", err)
		}
		calls := fixture.calls()
		if len(calls) != 2 {
			t.Fatalf("调用数 = %d", len(calls))
		}
		if calls[0].path != "/open-apis/auth/v3/tenant_access_token/internal" {
			t.Fatalf("token 路径 = %q", calls[0].path)
		}
		if !strings.Contains(calls[1].query, "receive_id_type=chat_id") || calls[1].auth != "Bearer ft" {
			t.Fatalf("消息请求 = %#v", calls[1])
		}
		if !strings.Contains(string(calls[1].body), `"receive_id":"oc_1"`) ||
			!strings.Contains(string(calls[1].body), `"msg_type":"text"`) {
			t.Fatalf("请求体 = %s", calls[1].body)
		}

		// token 缓存：第二次发送不再取 token。
		if err := fixture.sender.Send(context.Background(), Message{Text: "再次"}); err != nil {
			t.Fatalf("第二次 Send: %v", err)
		}
		if len(fixture.calls()) != 3 {
			t.Fatalf("缓存失效：调用数 = %d", len(fixture.calls()))
		}
	})

	t.Run("用户 id 走 open_id 且带图片链接", func(t *testing.T) {
		fixture := newSiteFixture(t, ChannelFeishu, map[string]any{
			"FEISHU_APP_ID": "app", "FEISHU_APP_SECRET": "secret", "FEISHU_OPEN_ID": "ou_1",
		}, tokenOK)
		if err := fixture.sender.Send(context.Background(), Message{UserID: "ou_9", Image: "https://example.test/i.png"}); err != nil {
			t.Fatalf("Send: %v", err)
		}
		calls := fixture.calls()
		if !strings.Contains(calls[1].query, "receive_id_type=open_id") ||
			!strings.Contains(string(calls[1].body), `"receive_id":"ou_9"`) {
			t.Fatalf("请求 = %#v / %s", calls[1], calls[1].body)
		}
		if !strings.Contains(string(calls[1].body), "i.png") {
			t.Fatalf("图片链接缺失: %s", calls[1].body)
		}
	})

	t.Run("缺接收方", func(t *testing.T) {
		fixture := newSiteFixture(t, ChannelFeishu, map[string]any{
			"FEISHU_APP_ID": "app", "FEISHU_APP_SECRET": "secret",
		}, tokenOK)
		if err := fixture.sender.Send(context.Background(), Message{Text: "x"}); err == nil ||
			!strings.Contains(err.Error(), "feishu destination is not configured") {
			t.Fatalf("错误 = %v", err)
		}
	})

	t.Run("未返回 token", func(t *testing.T) {
		fixture := newSiteFixture(t, ChannelFeishu, map[string]any{
			"FEISHU_APP_ID": "app", "FEISHU_APP_SECRET": "secret", "FEISHU_OPEN_ID": "ou_1",
		}, feishuHandler(`{"ok":true}`, `{"code":0}`))
		if err := fixture.sender.Send(context.Background(), Message{Text: "x"}); err == nil ||
			!strings.Contains(err.Error(), "feishu access token was not returned") {
			t.Fatalf("错误 = %v", err)
		}
	})

	t.Run("token 过期时间非法时按 60 秒兜底", func(t *testing.T) {
		fixture := newSiteFixture(t, ChannelFeishu, map[string]any{
			"FEISHU_APP_ID": "app", "FEISHU_APP_SECRET": "secret", "FEISHU_OPEN_ID": "ou_1",
		}, feishuHandler(`{"ok":true,"tenant_access_token":"ft","expire":"abc"}`, `{"code":0}`))
		if err := fixture.sender.Send(context.Background(), Message{Text: "x"}); err != nil {
			t.Fatalf("Send: %v", err)
		}
		if fixture.sender.token != "ft" || time.Until(fixture.sender.tokenExpires) > time.Minute {
			t.Fatalf("token 缓存 = %q / %v", fixture.sender.token, time.Until(fixture.sender.tokenExpires))
		}
	})
}

func TestSendSlack(t *testing.T) {
	slackHandler := func(list func(r *http.Request) string) func(http.ResponseWriter, *http.Request) {
		return func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/conversations.list") {
				_, _ = io.WriteString(w, list(r))
				return
			}
			_, _ = io.WriteString(w, `{"ok":true}`)
		}
	}

	t.Run("频道 id 直接发送", func(t *testing.T) {
		fixture := newSiteFixture(t, ChannelSlack, map[string]any{
			"SLACK_OAUTH_TOKEN": "token", "SLACK_CHANNEL": "C123",
		}, slackHandler(func(*http.Request) string { return `{"ok":true,"channels":[]}` }))
		if err := fixture.sender.Send(context.Background(), Message{Title: "标题", Text: "正文"}); err != nil {
			t.Fatalf("Send: %v", err)
		}
		call := fixture.last()
		if call.path != "/api/chat.postMessage" || call.auth != "Bearer token" {
			t.Fatalf("请求 = %#v", call)
		}
		if !strings.Contains(string(call.body), `"channel":"C123"`) || !strings.Contains(string(call.body), `"mrkdwn":true`) {
			t.Fatalf("请求体 = %s", call.body)
		}
	})

	t.Run("频道名需要查询", func(t *testing.T) {
		fixture := newSiteFixture(t, ChannelSlack, map[string]any{
			"SLACK_OAUTH_TOKEN": "token", "SLACK_CHANNEL": "general",
		}, slackHandler(func(*http.Request) string {
			return `{"ok":true,"channels":[{"name":"random","id":"C2"},{"name":"general","id":"C1"}]}`
		}))
		if err := fixture.sender.Send(context.Background(), Message{Text: "hi"}); err != nil {
			t.Fatalf("Send: %v", err)
		}
		calls := fixture.calls()
		if len(calls) != 2 || !strings.Contains(calls[0].query, "limit=200") {
			t.Fatalf("调用 = %#v", calls)
		}
		if !strings.Contains(string(calls[1].body), `"channel":"C1"`) {
			t.Fatalf("请求体 = %s", calls[1].body)
		}
	})

	t.Run("分页查询", func(t *testing.T) {
		fixture := newSiteFixture(t, ChannelSlack, map[string]any{
			"SLACK_OAUTH_TOKEN": "token", "SLACK_CHANNEL": "general",
		}, slackHandler(func(r *http.Request) string {
			if r.URL.Query().Get("cursor") == "" {
				return `{"ok":true,"channels":[],"response_metadata":{"next_cursor":"next"}}`
			}
			return `{"ok":true,"channels":[{"name":"general","id":"C9"}]}`
		}))
		if err := fixture.sender.Send(context.Background(), Message{Text: "hi"}); err != nil {
			t.Fatalf("Send: %v", err)
		}
		calls := fixture.calls()
		if len(calls) != 3 || !strings.Contains(calls[1].query, "cursor=next") {
			t.Fatalf("调用 = %#v", calls)
		}
		if !strings.Contains(string(calls[2].body), `"channel":"C9"`) {
			t.Fatalf("请求体 = %s", calls[2].body)
		}
	})

	t.Run("找不到频道", func(t *testing.T) {
		fixture := newSiteFixture(t, ChannelSlack, map[string]any{
			"SLACK_OAUTH_TOKEN": "token",
		}, slackHandler(func(*http.Request) string { return `{"ok":true,"channels":[]}` }))
		if err := fixture.sender.Send(context.Background(), Message{Text: "hi"}); err == nil ||
			!strings.Contains(err.Error(), "slack destination is not configured") {
			t.Fatalf("错误 = %v", err)
		}
	})

	t.Run("图片加链接生成 blocks", func(t *testing.T) {
		fixture := newSiteFixture(t, ChannelSlack, map[string]any{
			"SLACK_OAUTH_TOKEN": "token", "SLACK_CHANNEL": "D1",
		}, slackHandler(func(*http.Request) string { return `{"ok":true,"channels":[]}` }))
		if err := fixture.sender.Send(context.Background(), Message{
			Title: "标题", Text: "正文", Image: "https://example.test/i.png", Link: "https://example.test/p",
		}); err != nil {
			t.Fatalf("Send: %v", err)
		}
		body := string(fixture.last().body)
		if !strings.Contains(body, `"blocks"`) || !strings.Contains(body, `"actionId-url"`) ||
			!strings.Contains(body, `"image_url":"https://example.test/i.png"`) {
			t.Fatalf("blocks = %s", body)
		}
	})

	t.Run("查询失败", func(t *testing.T) {
		fixture := newSiteFixture(t, ChannelSlack, map[string]any{
			"SLACK_OAUTH_TOKEN": "token", "SLACK_CHANNEL": "general",
		}, slackHandler(func(*http.Request) string { return `{"ok":false}` }))
		if err := fixture.sender.Send(context.Background(), Message{Text: "hi"}); err == nil {
			t.Fatal("应返回错误")
		}
	})
}

func TestSendQQ(t *testing.T) {
	qqHandler := func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "getAppAccessToken") {
			_, _ = io.WriteString(w, `{"ok":true,"access_token":"qt","expires_in":"7200"}`)
			return
		}
		_, _ = io.WriteString(w, `{"ok":true}`)
	}

	t.Run("默认用户目标", func(t *testing.T) {
		fixture := newSiteFixture(t, ChannelQQ, map[string]any{
			"QQ_APP_ID": "1", "QQ_APP_SECRET": "s", "QQ_OPENID": "openid-1",
		}, qqHandler)
		if err := fixture.sender.Send(context.Background(), Message{Title: "标题"}); err != nil {
			t.Fatalf("Send: %v", err)
		}
		calls := fixture.calls()
		if len(calls) != 2 || calls[0].path != "/app/getAppAccessToken" {
			t.Fatalf("调用 = %#v", calls)
		}
		if calls[0].body == nil || !strings.Contains(string(calls[0].body), `"appId":"1"`) {
			t.Fatalf("取 token 请求体 = %s", calls[0].body)
		}
		if calls[1].path != "/v2/users/openid-1/messages" || calls[1].auth != "QQBot qt" {
			t.Fatalf("消息请求 = %#v", calls[1])
		}
		// 第二次发送复用 token。
		if err := fixture.sender.Send(context.Background(), Message{Text: "again"}); err != nil {
			t.Fatalf("第二次 Send: %v", err)
		}
		if len(fixture.calls()) != 3 {
			t.Fatalf("token 缓存失效：调用数 = %d", len(fixture.calls()))
		}
	})

	t.Run("群目标前缀与默认群", func(t *testing.T) {
		fixture := newSiteFixture(t, ChannelQQ, map[string]any{
			"QQ_APP_ID": "1", "QQ_APP_SECRET": "s", "QQ_GROUP_OPENID": "group-1",
		}, qqHandler)
		if err := fixture.sender.Send(context.Background(), Message{Text: "hi"}); err != nil {
			t.Fatalf("Send: %v", err)
		}
		if fixture.last().path != "/v2/groups/group-1/messages" {
			t.Fatalf("默认群路径 = %q", fixture.last().path)
		}
		for _, target := range []string{"group:g1", "GROUP#g2"} {
			if err := fixture.sender.Send(context.Background(), Message{UserID: target}); err != nil {
				t.Fatalf("Send(%q): %v", target, err)
			}
			if !strings.HasPrefix(fixture.last().path, "/v2/groups/") {
				t.Fatalf("%q 路径 = %q", target, fixture.last().path)
			}
		}
		if err := fixture.sender.Send(context.Background(), Message{UserID: "USER#u1", Image: "https://example.test/i.png"}); err != nil {
			t.Fatalf("USER# 目标: %v", err)
		}
		if fixture.last().path != "/v2/users/u1/messages" {
			t.Fatalf("USER# 路径 = %q", fixture.last().path)
		}
	})

	t.Run("缺目标", func(t *testing.T) {
		fixture := newSiteFixture(t, ChannelQQ, map[string]any{
			"QQ_APP_ID": "1", "QQ_APP_SECRET": "s",
		}, qqHandler)
		if err := fixture.sender.Send(context.Background(), Message{Text: "x"}); err == nil ||
			!strings.Contains(err.Error(), "qq destination is not configured") {
			t.Fatalf("错误 = %v", err)
		}
	})

	t.Run("未返回 token", func(t *testing.T) {
		fixture := newSiteFixture(t, ChannelQQ, map[string]any{
			"QQ_APP_ID": "1", "QQ_APP_SECRET": "s", "QQ_OPENID": "u",
		}, func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, `{"ok":true}`)
		})
		if err := fixture.sender.Send(context.Background(), Message{Text: "x"}); err == nil ||
			!strings.Contains(err.Error(), "qq access token was not returned") {
			t.Fatalf("错误 = %v", err)
		}
	})
}

func TestSendWeChat(t *testing.T) {
	wechatHandler := func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/cgi-bin/gettoken") {
			_, _ = io.WriteString(w, `{"ok":true,"access_token":"wt","expires_in":"7200"}`)
			return
		}
		_, _ = io.WriteString(w, `{"ok":true,"errcode":0}`)
	}

	t.Run("文本按 2048 字节分片", func(t *testing.T) {
		fixture := newSiteFixture(t, ChannelWeChat, map[string]any{
			"WECHAT_CORPID": "corp", "WECHAT_APP_SECRET": "secret", "WECHAT_APP_ID": "1000002",
		}, wechatHandler)
		long := strings.Repeat("字", 1500) // 4500 字节，超过 2048 上限
		if err := fixture.sender.Send(context.Background(), Message{
			Title: "标题", Text: long, Link: "https://example.test/p",
		}); err != nil {
			t.Fatalf("Send: %v", err)
		}
		calls := fixture.calls()
		if len(calls) != 4 { // 1 次取 token + 3 次发送
			t.Fatalf("调用数 = %d", len(calls))
		}
		for _, call := range calls[1:] {
			if !strings.Contains(call.query, "access_token=wt") {
				t.Fatalf("token 查询 = %q", call.query)
			}
			var payload map[string]any
			if err := json.Unmarshal(call.body, &payload); err != nil {
				t.Fatalf("解析请求体: %v", err)
			}
			if payload["msgtype"] != "text" || payload["touser"] != "@all" || payload["agentid"] != float64(1000002) {
				t.Fatalf("请求体 = %#v", payload)
			}
		}
	})

	t.Run("图片走 news 且尊重代理地址", func(t *testing.T) {
		fixture := newSiteFixture(t, ChannelWeChat, map[string]any{
			"WECHAT_CORPID": "corp", "WECHAT_APP_SECRET": "secret", "WECHAT_APP_ID": "1000002",
			"WECHAT_PROXY": "https://proxy.example.test/",
		}, wechatHandler)
		if err := fixture.sender.Send(context.Background(), Message{
			UserID: "user1", Title: "标题", Text: "正文",
			Image: "https://example.test/i.png", Link: "https://example.test/p",
		}); err != nil {
			t.Fatalf("Send: %v", err)
		}
		calls := fixture.calls()
		if len(calls) != 2 {
			t.Fatalf("调用数 = %d", len(calls))
		}
		if calls[0].path != "/cgi-bin/gettoken" {
			t.Fatalf("gettoken 路径 = %q", calls[0].path)
		}
		var payload map[string]any
		if err := json.Unmarshal(calls[1].body, &payload); err != nil {
			t.Fatalf("解析请求体: %v", err)
		}
		if payload["msgtype"] != "news" || payload["touser"] != "user1" {
			t.Fatalf("请求体 = %#v", payload)
		}
	})

	t.Run("app id 非数字", func(t *testing.T) {
		fixture := newSiteFixture(t, ChannelWeChat, map[string]any{
			"WECHAT_CORPID": "corp", "WECHAT_APP_SECRET": "secret", "WECHAT_APP_ID": "abc",
		}, wechatHandler)
		if err := fixture.sender.Send(context.Background(), Message{Text: "x"}); err == nil ||
			!strings.Contains(err.Error(), "wechat app id is invalid") {
			t.Fatalf("错误 = %v", err)
		}
	})

	t.Run("未返回 token", func(t *testing.T) {
		fixture := newSiteFixture(t, ChannelWeChat, map[string]any{
			"WECHAT_CORPID": "corp", "WECHAT_APP_SECRET": "secret", "WECHAT_APP_ID": "1000002",
		}, func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, `{"ok":true}`)
		})
		if err := fixture.sender.Send(context.Background(), Message{Text: "x"}); err == nil ||
			!strings.Contains(err.Error(), "wechat access token was not returned") {
			t.Fatalf("错误 = %v", err)
		}
	})
}

func TestRegistryWebPushAndTelegramFactory(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	sender, err := buildTelegramSender(logger, notificationconfig.Telegram{BotToken: "token", ChatID: "1"})
	if err != nil || sender.Name() != ChannelTelegram {
		t.Fatalf("buildTelegramSender = (%#v, %v)", sender, err)
	}
	if _, err := buildTelegramSender(logger, notificationconfig.Telegram{}); err == nil {
		t.Fatal("空 token 应报错")
	}

	var nilRegistry *Registry
	nilRegistry.SetWebPushAvailable(true)

	registry := NewRegistry(logger)
	registry.current.Store(&registryState{
		snapshot: notificationconfig.Snapshot{Configs: []notificationconfig.Config{{
			Name: "浏览器推送", Type: string(ChannelWebPush), Enabled: true, Switchs: []any{"download"},
		}}},
		senders:  map[string]ChannelSender{},
		channels: map[Channel]map[string]ChannelSender{},
	})

	// 未启用 WebPush 时不投影目标。
	if targets := registry.Targets(Message{Type: "download"}); len(targets) != 0 {
		t.Fatalf("未启用 WebPush 目标 = %#v", targets)
	}
	registry.SetWebPushAvailable(true)
	targets := registry.Targets(Message{Type: "download"})
	if len(targets) != 1 || targets[0].Channel != ChannelWebPush || targets[0].Source != "浏览器推送" {
		t.Fatalf("WebPush 目标 = %#v", targets)
	}
	// 类型不匹配时不投影。
	if targets := registry.Targets(Message{Type: "organize"}); len(targets) != 0 {
		t.Fatalf("类型不匹配 = %#v", targets)
	}
	// 未启用的配置不投影。
	registry.current.Store(&registryState{
		snapshot: notificationconfig.Snapshot{Configs: []notificationconfig.Config{{
			Name: "禁用", Type: string(ChannelWebPush), Enabled: false,
		}}},
		senders:  map[string]ChannelSender{},
		channels: map[Channel]map[string]ChannelSender{},
	})
	if targets := registry.Targets(Message{Type: "download"}); len(targets) != 0 {
		t.Fatalf("禁用配置 = %#v", targets)
	}
}

func TestWaitTelegramRetry(t *testing.T) {
	if err := waitTelegramRetry(context.Background(), time.Millisecond); err != nil {
		t.Fatalf("正常等待 = %v", err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := waitTelegramRetry(canceled, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("取消等待 = %v", err)
	}
}

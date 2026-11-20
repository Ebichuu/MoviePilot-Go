package telegrambot

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// scriptedAPI 起一个可编程的 Bot API 桩，记录每次调用的方法与请求体。
type scriptedAPI struct {
	t        *testing.T
	methods  []string
	bodies   []map[string]any
	respond  func(method string, body map[string]any) (int, string)
	server   *httptest.Server
	client   *apiClient
	requests int
}

func newScriptedAPI(t *testing.T, respond func(method string, body map[string]any) (int, string)) *scriptedAPI {
	t.Helper()
	scripted := &scriptedAPI{t: t, respond: respond}
	scripted.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		body := map[string]any{}
		_ = json.Unmarshal(raw, &body)
		method := r.URL.Path[strings.LastIndexByte(r.URL.Path, '/')+1:]
		scripted.methods = append(scripted.methods, method)
		scripted.bodies = append(scripted.bodies, body)
		scripted.requests++
		status, payload := scripted.respond(method, body)
		w.WriteHeader(status)
		_, _ = io.WriteString(w, payload)
	}))
	t.Cleanup(scripted.server.Close)
	scripted.client = newAPIClient(scripted.server.URL, "token", scripted.server.Client())
	return scripted
}

func okResult(t *testing.T, result any) (int, string) {
	t.Helper()
	encoded, err := json.Marshal(map[string]any{"ok": true, "result": result})
	if err != nil {
		t.Fatalf("编码响应: %v", err)
	}
	return http.StatusOK, string(encoded)
}

func TestNewAPIClientDefaults(t *testing.T) {
	client := newAPIClient("", " tok ", nil)
	if client.baseURL != defaultAPIURL || client.token != "tok" || client.client == nil {
		t.Fatalf("默认客户端 = %#v", client)
	}
	if client.endpoint(" getMe ") != defaultAPIURL+"/bottok/getMe" {
		t.Fatalf("endpoint = %q", client.endpoint(" getMe "))
	}
	client = newAPIClient("https://proxy.test/", "x", nil)
	if client.baseURL != "https://proxy.test" {
		t.Fatalf("去尾斜杠 = %q", client.baseURL)
	}
	if client.client.Timeout != defaultPollTime+30*time.Second {
		t.Fatalf("默认超时 = %v", client.client.Timeout)
	}
}

func TestAPICallErrorBranches(t *testing.T) {
	if err := (&apiClient{token: ""}).call(context.Background(), "getMe", nil, nil); !errors.Is(err, ErrEmptyToken) {
		t.Fatalf("空 token = %v", err)
	}

	scripted := newScriptedAPI(t, func(string, map[string]any) (int, string) { return okResult(t, nil) })
	if err := scripted.client.call(context.Background(), "x", map[string]any{"f": func() {}}, nil); !errors.Is(err, ErrAPIRequest) {
		t.Fatalf("无法编码的载荷 = %v", err)
	}
	if err := (&apiClient{baseURL: "http://[::1", token: "t"}).call(context.Background(), "getMe", nil, nil); !errors.Is(err, ErrAPIRequest) {
		t.Fatalf("非法地址 = %v", err)
	}

	closed := newScriptedAPI(t, func(string, map[string]any) (int, string) { return okResult(t, nil) })
	closed.server.Close()
	if err := closed.client.call(context.Background(), "getMe", nil, nil); !errors.Is(err, ErrAPIRequest) {
		t.Fatalf("连接失败 = %v", err)
	}

	cases := []struct {
		name    string
		status  int
		payload string
		want    string
		out     any
	}{
		{"非 JSON 响应", http.StatusBadGateway, "<html>bad gateway</html>", "HTTP 502", &User{}},
		{"ok=false 带描述", http.StatusBadRequest, `{"ok":false,"description":"Bad Request: chat not found"}`, "chat not found", &User{}},
		{"ok=false 无描述", http.StatusForbidden, `{"ok":false}`, "HTTP 403", &User{}},
		// 对象结果无法解到数组里，走 result 解码失败分支。
		{"结果类型不符", http.StatusOK, `{"ok":true,"result":{"a":1}}`, "cannot unmarshal", &[]Update{}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			scripted := newScriptedAPI(t, func(string, map[string]any) (int, string) {
				return testCase.status, testCase.payload
			})
			err := scripted.client.call(context.Background(), "getMe", nil, testCase.out)
			if !errors.Is(err, ErrAPIRequest) || !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("错误 = %v，期望包含 %q", err, testCase.want)
			}
		})
	}

	// out 为 nil 或 result 为空时直接成功。
	empty := newScriptedAPI(t, func(string, map[string]any) (int, string) {
		return http.StatusOK, `{"ok":true}`
	})
	if err := empty.client.call(context.Background(), "deleteMessage", map[string]any{}, nil); err != nil {
		t.Fatalf("空 result = %v", err)
	}
	if err := empty.client.call(context.Background(), "getMe", nil, &User{}); err != nil {
		t.Fatalf("out 非 nil 但无 result = %v", err)
	}
}

func TestAPIMethods(t *testing.T) {
	t.Run("getUpdates", func(t *testing.T) {
		scripted := newScriptedAPI(t, func(method string, _ map[string]any) (int, string) {
			return okResult(t, []Update{{UpdateID: 7}})
		})
		updates, err := scripted.client.getUpdates(context.Background(), 6, 0)
		if err != nil || len(updates) != 1 || updates[0].UpdateID != 7 {
			t.Fatalf("updates = (%#v, %v)", updates, err)
		}
		body := scripted.bodies[0]
		// poll<=0 回落到默认 25 秒。
		if body["timeout"] != float64(defaultPollTime/time.Second) || body["offset"] != float64(6) {
			t.Fatalf("请求体 = %#v", body)
		}
		if allowed, ok := body["allowed_updates"].([]any); !ok || len(allowed) != 2 {
			t.Fatalf("allowed_updates = %#v", body["allowed_updates"])
		}
		if _, err := scripted.client.getUpdates(context.Background(), 0, time.Second); err != nil {
			t.Fatalf("带 timeout 的轮询 = %v", err)
		}
		if scripted.bodies[1]["timeout"] != float64(1) {
			t.Fatalf("timeout = %#v", scripted.bodies[1]["timeout"])
		}
	})

	t.Run("getUpdates 错误", func(t *testing.T) {
		scripted := newScriptedAPI(t, func(string, map[string]any) (int, string) {
			return http.StatusBadRequest, `{"ok":false,"description":"invalid offset"}`
		})
		if _, err := scripted.client.getUpdates(context.Background(), 0, time.Second); !errors.Is(err, ErrAPIRequest) {
			t.Fatalf("错误 = %v", err)
		}
	})

	t.Run("sendMessage", func(t *testing.T) {
		scripted := newScriptedAPI(t, func(string, map[string]any) (int, string) {
			return okResult(t, map[string]any{"message_id": 55})
		})
		messageID, err := scripted.client.sendMessage(context.Background(), 42, "  你好  ", nil)
		if err != nil || messageID != 55 {
			t.Fatalf("sendMessage = (%d, %v)", messageID, err)
		}
		if _, ok := scripted.bodies[0]["reply_markup"]; ok {
			t.Fatal("nil 键盘不应带 reply_markup")
		}
		// 空键盘同样不带 reply_markup。
		if _, err := scripted.client.sendMessage(context.Background(), 42, "x", &InlineKeyboard{}); err != nil {
			t.Fatalf("空键盘 = %v", err)
		}
		if _, ok := scripted.bodies[1]["reply_markup"]; ok {
			t.Fatal("空键盘不应带 reply_markup")
		}
		keyboard := &InlineKeyboard{InlineKeyboard: [][]InlineButton{{{Text: "详情", CallbackData: "d:0"}}}}
		if _, err := scripted.client.sendMessage(context.Background(), 42, "x", keyboard); err != nil {
			t.Fatalf("带键盘 = %v", err)
		}
		if _, ok := scripted.bodies[2]["reply_markup"]; !ok {
			t.Fatal("非空键盘应带 reply_markup")
		}

		long := strings.Repeat("长", maxReplyRunes+10)
		if _, err := scripted.client.sendMessage(context.Background(), 42, long, nil); err != nil {
			t.Fatalf("超长文本 = %v", err)
		}
		if text, _ := scripted.bodies[3]["text"].(string); len([]rune(text)) != maxReplyRunes+1 {
			t.Fatalf("超长文本长度 = %d", len([]rune(text)))
		}

		failing := newScriptedAPI(t, func(string, map[string]any) (int, string) {
			return http.StatusBadRequest, `{"ok":false,"description":"bad"}`
		})
		if _, err := failing.client.sendMessage(context.Background(), 1, "x", nil); !errors.Is(err, ErrAPIRequest) {
			t.Fatalf("失败 = %v", err)
		}
	})

	t.Run("editMessageText", func(t *testing.T) {
		scripted := newScriptedAPI(t, func(string, map[string]any) (int, string) { return okResult(t, nil) })
		if err := scripted.client.editMessageText(context.Background(), 1, 2, "文本", nil); err != nil {
			t.Fatalf("editMessageText = %v", err)
		}
		if _, ok := scripted.bodies[0]["reply_markup"]; ok {
			t.Fatal("nil 键盘不应带 reply_markup")
		}
		if err := scripted.client.editMessageText(context.Background(), 1, 2, "文本", &InlineKeyboard{}); err != nil {
			t.Fatalf("editMessageText 带键盘 = %v", err)
		}
		if _, ok := scripted.bodies[1]["reply_markup"]; !ok {
			t.Fatal("非 nil 键盘应带 reply_markup")
		}
		if scripted.bodies[0]["message_id"] != float64(2) {
			t.Fatalf("message_id = %#v", scripted.bodies[0]["message_id"])
		}
	})

	t.Run("answerCallback", func(t *testing.T) {
		scripted := newScriptedAPI(t, func(string, map[string]any) (int, string) { return okResult(t, nil) })
		if err := scripted.client.answerCallback(context.Background(), "cb", "   "); err != nil {
			t.Fatalf("answerCallback = %v", err)
		}
		if _, ok := scripted.bodies[0]["text"]; ok {
			t.Fatal("空白提示不应带 text")
		}
		if err := scripted.client.answerCallback(context.Background(), "cb", " 已刷新 "); err != nil {
			t.Fatalf("answerCallback 带提示 = %v", err)
		}
		if scripted.bodies[1]["text"] != "已刷新" {
			t.Fatalf("提示 = %#v", scripted.bodies[1]["text"])
		}
		long := strings.Repeat("a", 250)
		if err := scripted.client.answerCallback(context.Background(), "cb", long); err != nil {
			t.Fatalf("超长提示 = %v", err)
		}
		if text, _ := scripted.bodies[2]["text"].(string); len([]rune(text)) != 201 {
			t.Fatalf("超长提示长度 = %d", len([]rune(text)))
		}
	})

	t.Run("deleteMessage", func(t *testing.T) {
		scripted := newScriptedAPI(t, func(string, map[string]any) (int, string) { return okResult(t, nil) })
		if err := scripted.client.deleteMessage(context.Background(), 9, 3); err != nil {
			t.Fatalf("deleteMessage = %v", err)
		}
		if scripted.bodies[0]["chat_id"] != float64(9) || scripted.bodies[0]["message_id"] != float64(3) {
			t.Fatalf("请求体 = %#v", scripted.bodies[0])
		}
		failing := newScriptedAPI(t, func(string, map[string]any) (int, string) {
			return http.StatusBadRequest, `{"ok":false,"description":"message to delete not found"}`
		})
		if err := failing.client.deleteMessage(context.Background(), 9, 3); !errors.Is(err, ErrAPIRequest) {
			t.Fatalf("删除失败 = %v", err)
		}
	})

	t.Run("getMe", func(t *testing.T) {
		scripted := newScriptedAPI(t, func(string, map[string]any) (int, string) {
			return okResult(t, map[string]any{"id": 11, "is_bot": true, "username": "moviepilot_bot"})
		})
		user, err := scripted.client.getMe(context.Background())
		if err != nil || user.ID != 11 || !user.IsBot || user.Username != "moviepilot_bot" {
			t.Fatalf("getMe = (%#v, %v)", user, err)
		}
		if len(scripted.bodies[0]) != 0 {
			t.Fatalf("getMe 请求体应为空: %#v", scripted.bodies[0])
		}
		failing := newScriptedAPI(t, func(string, map[string]any) (int, string) {
			return http.StatusUnauthorized, `{"ok":false,"description":"Unauthorized"}`
		})
		if _, err := failing.client.getMe(context.Background()); !errors.Is(err, ErrAPIRequest) {
			t.Fatalf("token 无效 = %v", err)
		}
	})
}

func TestTruncateText(t *testing.T) {
	cases := []struct {
		text  string
		limit int
		want  string
	}{
		{"", 10, ""},
		{"  前后空格  ", 10, "前后空格"},
		{"短", 10, "短"},
		{"正好十个字符啊", 10, "正好十个字符啊"},
		{"超出限制的文本", 3, "超出限…"},
		{"任意", 0, "任意"},
		{"任意", -1, "任意"},
	}
	for _, testCase := range cases {
		if got := truncateText(testCase.text, testCase.limit); got != testCase.want {
			t.Fatalf("truncateText(%q, %d) = %q, want %q", testCase.text, testCase.limit, got, testCase.want)
		}
	}
}

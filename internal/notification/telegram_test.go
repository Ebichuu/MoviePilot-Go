package notification

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestTelegramSenderSendPhotoAndFallback(t *testing.T) {
	t.Parallel()
	disablePreview := true

	tests := []struct {
		name        string
		imageStatus int
		wantMethod  string
	}{
		{name: "图片下载成功使用 sendPhoto", imageStatus: http.StatusOK, wantMethod: "sendPhoto"},
		{name: "图片下载失败降级 sendMessage", imageStatus: http.StatusBadGateway, wantMethod: "sendMessage"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			methods := make(chan string, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/poster" {
					w.WriteHeader(tt.imageStatus)
					if tt.imageStatus == http.StatusOK {
						_, _ = w.Write([]byte("image-bytes"))
					}
					return
				}
				if strings.HasSuffix(r.URL.Path, "/sendPhoto") {
					methods <- "sendPhoto"
					if err := r.ParseMultipartForm(1 << 20); err != nil {
						t.Errorf("ParseMultipartForm() error = %v", err)
					}
					if _, exists := r.MultipartForm.Value["disable_web_page_preview"]; exists {
						t.Error("sendPhoto must not include disable_web_page_preview")
					}
					if got := r.FormValue("caption"); !strings.Contains(got, "title") {
						t.Errorf("caption = %q", got)
					}
					file, _, err := r.FormFile("photo")
					if err != nil {
						t.Errorf("FormFile(photo) error = %v", err)
					} else {
						defer file.Close()
						body, _ := io.ReadAll(file)
						if string(body) != "image-bytes" {
							t.Errorf("photo = %q", body)
						}
					}
				} else if strings.HasSuffix(r.URL.Path, "/sendMessage") {
					methods <- "sendMessage"
				} else {
					t.Errorf("unexpected Telegram path %q", r.URL.Path)
				}
				_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":1}}`))
			}))
			defer server.Close()

			sender, err := NewTelegramSender(slog.Default(), TelegramConfig{
				BotToken: "test-token", ChatID: "default-chat", APIUrl: server.URL,
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := sender.Send(t.Context(), Message{
				Title: "title", Image: server.URL + "/poster", DisableWebPagePreview: &disablePreview,
			}); err != nil {
				t.Fatal(err)
			}
			if got := <-methods; got != tt.wantMethod {
				t.Fatalf("Telegram method = %q, want %q", got, tt.wantMethod)
			}
		})
	}
}

func TestTelegramSenderSplitsV2LengthLimits(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		image     bool
		textSize  int
		wantLimit int
		wantCalls int
	}{
		{name: "文本按 4096 拆分", textSize: 5000, wantLimit: telegramTextLimit, wantCalls: 2},
		{name: "图片说明按 1024 拆分", image: true, textSize: 2500, wantLimit: telegramCaptionLimit, wantCalls: 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			chunks := make(chan string, tt.wantCalls)
			methods := make(chan string, tt.wantCalls)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/poster" {
					_, _ = w.Write([]byte("image"))
					return
				}
				if strings.HasSuffix(r.URL.Path, "/sendPhoto") {
					methods <- "sendPhoto"
					if err := r.ParseMultipartForm(1 << 20); err != nil {
						t.Errorf("ParseMultipartForm() error = %v", err)
					}
					chunks <- r.FormValue("caption")
				} else {
					methods <- "sendMessage"
					var payload struct {
						Text string `json:"text"`
					}
					if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
						t.Errorf("decode payload: %v", err)
					}
					chunks <- payload.Text
				}
				_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":1}}`))
			}))
			defer server.Close()

			sender, err := NewTelegramSender(slog.Default(), TelegramConfig{
				BotToken: "test-token", ChatID: "default-chat", APIUrl: server.URL,
			})
			if err != nil {
				t.Fatal(err)
			}
			imageURL := ""
			if tt.image {
				imageURL = server.URL + "/poster"
			}
			if err := sender.Send(t.Context(), Message{Text: strings.Repeat("a", tt.textSize), Image: imageURL}); err != nil {
				t.Fatal(err)
			}
			for index := range tt.wantCalls {
				method := <-methods
				chunk := <-chunks
				if utf8.RuneCountInString(chunk) > tt.wantLimit {
					t.Fatalf("chunk %d length = %d, limit %d", index, utf8.RuneCountInString(chunk), tt.wantLimit)
				}
				if tt.image && index == 0 && method != "sendPhoto" {
					t.Fatalf("first image chunk method = %q", method)
				}
				if (!tt.image || index > 0) && method != "sendMessage" {
					t.Fatalf("text chunk %d method = %q", index, method)
				}
			}
		})
	}
}

func TestTelegramSenderPerMessageParseMode(t *testing.T) {
	t.Parallel()

	payloads := make(chan map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode payload: %v", err)
		}
		payloads <- payload
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":1}}`))
	}))
	defer server.Close()
	sender, err := NewTelegramSender(slog.Default(), TelegramConfig{
		BotToken: "test-token", ChatID: "default-chat", APIUrl: server.URL,
		ParseMode: "MarkdownV2",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := sender.Send(t.Context(), Message{Title: "<title>", Text: "A&B", ParseMode: "html"}); err != nil {
		t.Fatal(err)
	}
	payload := <-payloads
	if payload["parse_mode"] != "HTML" {
		t.Fatalf("parse_mode = %#v", payload["parse_mode"])
	}
	text, _ := payload["text"].(string)
	if text != "<b>&lt;title&gt;</b>\nA&B" {
		t.Fatalf("HTML text = %q", text)
	}
}

func TestTelegramSenderPreservesV2Formatting(t *testing.T) {
	t.Parallel()

	sender := &TelegramSender{parseMode: "MarkdownV2", logger: slog.Default()}
	tests := []struct {
		name      string
		message   Message
		parseMode string
		want      string
	}{
		{
			name: "HTML 正文原样保留且只转义标题与链接属性",
			message: Message{
				Title: "测试 <标题>",
				Text:  "<blockquote>第一行</blockquote><b>加粗</b>",
				Link:  "https://example.com/?a=1&b=2",
			},
			parseMode: "HTML",
			want: "<b>测试 &lt;标题&gt;</b>\n" +
				"<blockquote>第一行</blockquote><b>加粗</b>\n" +
				"<a href=\"https://example.com/?a=1&amp;b=2\">查看详情</a>",
		},
		{
			name: "Markdown 保留格式语义并正确处理反斜杠",
			message: Message{
				Title: "测试 [标题]",
				Text:  "**粗体** *斜体* ||剧透|| C:\\Media\\A_B",
			},
			parseMode: "MarkdownV2",
			want:      "*测试 \\[标题\\]*\n*粗体* _斜体_ ||剧透|| C:\\\\Media\\\\A\\_B",
		},
		{
			name:      "标题末尾单个换行按 V2 移除",
			message:   Message{Title: "标题\n", Text: "正文"},
			parseMode: "MarkdownV2",
			want:      "*标题*\n正文",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := sender.formatMessageWithMode(tt.message, tt.parseMode); got != tt.want {
				t.Fatalf("formatMessageWithMode() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestTelegramSenderSplitsWithoutBreakingFormattingEntities(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		message   Message
		parseMode string
		validate  func(*testing.T, []string)
	}{
		{
			name:      "Markdown 长标题的粗体实体在每片闭合",
			message:   Message{Title: strings.Repeat("长", telegramTextLimit+300)},
			parseMode: "MarkdownV2",
			validate: func(t *testing.T, chunks []string) {
				t.Helper()
				if len(chunks) != 2 {
					t.Fatalf("chunks = %d, want 2", len(chunks))
				}
				for index, chunk := range chunks {
					if !strings.HasPrefix(chunk, "*") || !strings.HasSuffix(chunk, "*") {
						t.Fatalf("chunk %d has broken bold entity: %q", index, chunk)
					}
				}
			},
		},
		{
			name:      "HTML 长标题的标签在每片闭合",
			message:   Message{Title: strings.Repeat("长", telegramTextLimit+300)},
			parseMode: "HTML",
			validate: func(t *testing.T, chunks []string) {
				t.Helper()
				if len(chunks) != 2 {
					t.Fatalf("chunks = %d, want 2", len(chunks))
				}
				for index, chunk := range chunks {
					if !strings.HasPrefix(chunk, "<b>") || !strings.HasSuffix(chunk, "</b>") {
						t.Fatalf("chunk %d has broken HTML entity: %q", index, chunk)
					}
				}
			},
		},
		{
			name: "Markdown 长链接保持为单个实体",
			message: Message{
				Text: "正文",
				Link: "https://example.com/" + strings.Repeat("a", telegramTextLimit+100) + "(end)",
			},
			parseMode: "MarkdownV2",
			validate: func(t *testing.T, chunks []string) {
				t.Helper()
				if len(chunks) != 1 {
					t.Fatalf("chunks = %d, want one entity-aware chunk", len(chunks))
				}
				if strings.Count(chunks[0], "[查看详情](") != 1 ||
					!strings.HasSuffix(chunks[0], "(end\\))") {
					t.Fatalf("long link entity was split or malformed: %q", chunks[0])
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			chunks := splitTelegramMessage(
				senderForParseMode(tt.parseMode).formatMessageWithMode(tt.message, tt.parseMode),
				telegramTextLimit,
				tt.parseMode,
			)
			tt.validate(t, chunks)
		})
	}
}

func TestSplitTelegramMessageSplitsOversizedMarkdownLinks(t *testing.T) {
	t.Parallel()

	const linkSuffix = "](https://example.com/path)"
	for _, limit := range []int{telegramCaptionLimit, telegramTextLimit} {
		limit := limit
		t.Run(fmt.Sprintf("limit_%d", limit), func(t *testing.T) {
			t.Parallel()
			chunks := splitTelegramMessage(
				"["+strings.Repeat("中", limit+1)+linkSuffix,
				limit,
				"MarkdownV2",
			)
			if len(chunks) != 2 {
				t.Fatalf("chunks = %d, want 2", len(chunks))
			}
			for index, chunk := range chunks {
				if !strings.HasPrefix(chunk, "[") || !strings.HasSuffix(chunk, linkSuffix) {
					t.Fatalf("chunk %d is not a complete Markdown link: %q", index, chunk)
				}
				width := 0
				for _, token := range tokenizeTelegramMarkdown(chunk) {
					width += token.width
				}
				if width > limit {
					t.Fatalf("chunk %d rendered width = %d, limit %d", index, width, limit)
				}
			}
		})
	}
}

func TestSplitTelegramMessageLeavesBoundaryOpenersForNextChunk(t *testing.T) {
	t.Parallel()

	for _, limit := range []int{telegramCaptionLimit, telegramTextLimit} {
		limit := limit
		for _, tt := range []struct {
			name      string
			parseMode string
			suffix    string
		}{
			{name: "Markdown", parseMode: "MarkdownV2", suffix: "_b_"},
			{name: "HTML", parseMode: "HTML", suffix: "<b>b</b>"},
		} {
			tt := tt
			t.Run(fmt.Sprintf("%s_limit_%d", tt.name, limit), func(t *testing.T) {
				t.Parallel()
				prefix := strings.Repeat("a", limit)
				chunks := splitTelegramMessage(prefix+tt.suffix, limit, tt.parseMode)
				if len(chunks) != 2 {
					t.Fatalf("chunks = %d, want 2", len(chunks))
				}
				if chunks[0] != prefix || chunks[1] != tt.suffix {
					t.Fatalf("chunks = %#v, want an untouched boundary opener", chunks)
				}
			})
		}
	}
}

func TestSplitTelegramMessageKeepsTelegramEmojiEntitiesBalanced(t *testing.T) {
	t.Parallel()

	const (
		open  = `<tg-emoji emoji-id="5368324170671202286">`
		close = "</tg-emoji>"
	)
	for _, limit := range []int{telegramCaptionLimit, telegramTextLimit} {
		limit := limit
		t.Run(fmt.Sprintf("limit_%d", limit), func(t *testing.T) {
			t.Parallel()
			chunks := splitTelegramMessage(
				strings.Repeat("a", limit-1)+open+"👍👍"+close,
				limit,
				"HTML",
			)
			if len(chunks) != 2 {
				t.Fatalf("chunks = %d, want 2", len(chunks))
			}
			if !strings.HasSuffix(chunks[0], close) || !strings.HasPrefix(chunks[1], open) ||
				strings.Count(chunks[0], open) != 1 || strings.Count(chunks[0], close) != 1 ||
				strings.Count(chunks[1], open) != 1 || strings.Count(chunks[1], close) != 1 {
				t.Fatalf("tg-emoji entity was split unbalanced: %#v", chunks)
			}
		})
	}
}

func TestTelegramSenderRetriesOnlyFailedChunk(t *testing.T) {
	t.Parallel()

	requests := make(chan string, 4)
	requestCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Text string `json:"text"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode payload: %v", err)
		}
		requestCount++
		requests <- payload.Text
		if requestCount == 2 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"ok":false}`))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":1}}`))
	}))
	defer server.Close()

	sender, err := NewTelegramSender(slog.Default(), TelegramConfig{
		BotToken: "test-token", ChatID: "default-chat", APIUrl: server.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	sender.retryWait = func(context.Context, time.Duration) error { return nil }
	text := strings.Repeat("a", telegramTextLimit) +
		strings.Repeat("b", telegramTextLimit) + strings.Repeat("c", 100)
	if err := sender.Send(t.Context(), Message{Text: text}); err != nil {
		t.Fatal(err)
	}
	close(requests)
	got := make([]string, 0, 4)
	for request := range requests {
		got = append(got, request)
	}
	if len(got) != 4 {
		t.Fatalf("request count = %d, want 4", len(got))
	}
	if got[0] == got[1] {
		t.Fatalf("first successful chunk was retried: requests = %d", len(got))
	}
	if got[1] != got[2] {
		t.Fatal("failed chunk was not retried in place")
	}
	if got[2] == got[3] {
		t.Fatal("final chunk was not sent after the retry succeeded")
	}
}

func TestTelegramSenderRetriesWholeV2HTMLBatch(t *testing.T) {
	t.Parallel()

	requests := make(chan string, 5)
	requestCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Text string `json:"text"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode payload: %v", err)
		}
		requestCount++
		requests <- payload.Text
		if requestCount == 2 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"ok":false}`))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":1}}`))
	}))
	defer server.Close()

	sender, err := NewTelegramSender(slog.Default(), TelegramConfig{
		BotToken: "test-token", ChatID: "default-chat", APIUrl: server.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	sender.retryWait = func(context.Context, time.Duration) error { return nil }
	text := strings.Repeat("a", telegramTextLimit) +
		strings.Repeat("b", telegramTextLimit) + strings.Repeat("c", 100)
	if err := sender.Send(t.Context(), Message{Text: text, ParseMode: "HTML"}); err != nil {
		t.Fatal(err)
	}
	close(requests)
	got := make([]string, 0, 5)
	for request := range requests {
		got = append(got, request)
	}
	if len(got) != 5 {
		t.Fatalf("request count = %d, want 5", len(got))
	}
	if got[0] != got[2] || got[1] != got[3] || got[3] == got[4] {
		t.Fatal("HTML retry did not restart the whole V2 chunk batch")
	}
}

func TestTelegramSenderUsesV2ThreeImmediateAttempts(t *testing.T) {
	t.Parallel()

	attempts := make(chan struct{}, telegramSendAttempts)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts <- struct{}{}
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"ok":false}`))
	}))
	defer server.Close()

	sender, err := NewTelegramSender(slog.Default(), TelegramConfig{
		BotToken: "test-token", ChatID: "default-chat", APIUrl: server.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	waits := make([]time.Duration, 0, telegramSendAttempts-1)
	sender.retryWait = func(_ context.Context, delay time.Duration) error {
		waits = append(waits, delay)
		return nil
	}
	if err := sender.Send(t.Context(), Message{Text: "retry"}); err == nil {
		t.Fatal("Send() error = nil")
	}
	if got := len(attempts); got != telegramSendAttempts {
		t.Fatalf("attempt count = %d, want %d", got, telegramSendAttempts)
	}
	if len(waits) != 2 || waits[0] != 3*time.Second || waits[1] != 6*time.Second {
		t.Fatalf("retry waits = %v, want [3s 6s]", waits)
	}
}

func senderForParseMode(parseMode string) *TelegramSender {
	return &TelegramSender{parseMode: parseMode, logger: slog.Default()}
}

func TestTelegramSenderSendUsesV2TargetChat(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		userID     string
		wantChatID string
	}{
		{name: "broadcast uses configured chat", wantChatID: "default-chat"},
		{name: "direct message uses userid", userID: "target-user", wantChatID: "target-user"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			payloads := make(chan map[string]any, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var payload map[string]any
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Errorf("decode payload: %v", err)
				}
				payloads <- payload
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":1}}`))
			}))
			defer server.Close()
			sender, err := NewTelegramSender(slog.Default(), TelegramConfig{
				BotToken: "test-token", ChatID: "default-chat", APIUrl: server.URL,
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := sender.Send(t.Context(), Message{Title: "title", UserID: tt.userID}); err != nil {
				t.Fatal(err)
			}
			if got := (<-payloads)["chat_id"]; got != tt.wantChatID {
				t.Fatalf("chat_id = %#v, want %q", got, tt.wantChatID)
			}
		})
	}
}

func TestTelegramSenderPreservesV2WebPagePreviewTriState(t *testing.T) {
	t.Parallel()

	trueValue, falseValue := true, false
	tests := []struct {
		name    string
		value   *bool
		wantSet bool
		want    bool
	}{
		{name: "nil 不发送字段"},
		{name: "true 原样发送", value: &trueValue, wantSet: true, want: true},
		{name: "false 原样发送", value: &falseValue, wantSet: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			payloads := make(chan map[string]any, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var payload map[string]any
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Errorf("decode payload: %v", err)
				}
				payloads <- payload
				_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":1}}`))
			}))
			defer server.Close()

			sender, err := NewTelegramSender(slog.Default(), TelegramConfig{
				BotToken: "test-token", ChatID: "default-chat", APIUrl: server.URL,
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := sender.Send(t.Context(), Message{
				Text: "https://example.com", DisableWebPagePreview: tt.value,
			}); err != nil {
				t.Fatal(err)
			}
			got, exists := (<-payloads)["disable_web_page_preview"]
			if exists != tt.wantSet {
				t.Fatalf("disable_web_page_preview presence = %v, want %v", exists, tt.wantSet)
			}
			if tt.wantSet && got != tt.want {
				t.Fatalf("disable_web_page_preview = %#v, want %v", got, tt.want)
			}
		})
	}
}

func TestTelegramSender_Send_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/sendMessage") {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("expected application/json content type")
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true,"result":{"message_id":123}}`))
	}))
	defer server.Close()

	sender, err := NewTelegramSender(slog.Default(), TelegramConfig{
		BotToken: "test-token",
		ChatID:   "12345",
		APIUrl:   server.URL,
	})
	if err != nil {
		t.Fatalf("NewTelegramSender failed: %v", err)
	}

	msg := Message{
		Title: "Test Notification",
		Text:  "This is a test message",
		Link:  "https://example.com",
		Type:  TypeOrganize,
	}

	if err := sender.Send(context.Background(), msg); err != nil {
		t.Errorf("Send failed: %v", err)
	}
}

func TestTelegramSender_Send_APIError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		status int
	}{
		{name: "Bad Request", status: http.StatusBadRequest},
		{name: "Too Many Requests", status: http.StatusTooManyRequests},
		{name: "Internal Server Error", status: http.StatusInternalServerError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(`{"ok":false,"description":"sensitive upstream response"}`))
			}))
			defer server.Close()

			const fixtureCredential = "telegram-test-token"
			sender, err := NewTelegramSender(slog.Default(), TelegramConfig{
				BotToken: fixtureCredential,
				ChatID:   "12345",
				APIUrl:   server.URL,
			})
			if err != nil {
				t.Fatalf("NewTelegramSender failed: %v", err)
			}
			sender.retryWait = func(context.Context, time.Duration) error { return nil }

			err = sender.Send(t.Context(), Message{Title: "Test"})
			if err == nil {
				t.Fatalf("Send() error = nil, status %d", tt.status)
			}
			if !strings.Contains(err.Error(), strconv.Itoa(tt.status)) {
				t.Fatalf("Send() error = %v, want status %d", err, tt.status)
			}
			for _, secret := range []string{fixtureCredential, server.URL, "sensitive upstream response"} {
				if strings.Contains(err.Error(), secret) {
					t.Fatalf("Send() error leaked %q: %v", secret, err)
				}
			}
		})
	}
}

func TestTelegramSenderCancellationStopsRequest(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
	}))
	defer func() {
		close(release)
		server.Close()
	}()

	sender, err := NewTelegramSender(slog.Default(), TelegramConfig{
		BotToken: "test-token", ChatID: "12345", APIUrl: server.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- sender.Send(ctx, Message{Title: "Test"})
	}()
	select {
	case <-started:
		cancel()
	case <-time.After(time.Second):
		t.Fatal("Telegram request did not start")
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Send() error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Telegram request did not stop after cancellation")
	}
}

func TestTelegramSenderTimeoutAndResponseLossAreSanitized(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		handler   func(<-chan struct{}, chan<- struct{}) http.HandlerFunc
		configure func(*TelegramSender)
		accepted  bool
	}{
		{
			name: "client timeout",
			handler: func(release <-chan struct{}, _ chan<- struct{}) http.HandlerFunc {
				return func(_ http.ResponseWriter, _ *http.Request) {
					<-release
				}
			},
			configure: func(sender *TelegramSender) {
				sender.client.Timeout = 25 * time.Millisecond
			},
		},
		{
			name: "response lost after request",
			handler: func(_ <-chan struct{}, accepted chan<- struct{}) http.HandlerFunc {
				return func(w http.ResponseWriter, r *http.Request) {
					_, _ = io.Copy(io.Discard, r.Body)
					accepted <- struct{}{}
					connection, _, err := w.(http.Hijacker).Hijack()
					if err == nil {
						_ = connection.Close()
					}
				}
			},
			configure: func(*TelegramSender) {},
			accepted:  true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			release := make(chan struct{})
			accepted := make(chan struct{}, telegramSendAttempts)
			server := httptest.NewServer(tt.handler(release, accepted))
			defer func() {
				close(release)
				server.Close()
			}()

			const fixtureCredential = "telegram-transport-secret"
			sender, err := NewTelegramSender(slog.Default(), TelegramConfig{
				BotToken: fixtureCredential, ChatID: "12345", APIUrl: server.URL,
			})
			if err != nil {
				t.Fatal(err)
			}
			tt.configure(sender)
			sender.retryWait = func(context.Context, time.Duration) error { return nil }
			err = sender.Send(t.Context(), Message{Title: "Test"})
			if err == nil {
				t.Fatal("Send() error = nil")
			}
			if tt.accepted {
				select {
				case <-accepted:
				default:
					t.Fatal("Telegram fixture did not accept the complete request before losing the response")
				}
			}
			for _, secret := range []string{fixtureCredential, server.URL} {
				if strings.Contains(err.Error(), secret) {
					t.Fatalf("Send() error leaked %q: %v", secret, err)
				}
			}
		})
	}
}

func TestTelegramSenderRejectsInvalidTelegramResponses(t *testing.T) {
	t.Parallel()

	const sensitive = "telegram-sensitive-description"
	tests := []struct {
		name string
		body string
	}{
		{name: "API 明确拒绝", body: `{"ok":false,"error_code":400,"description":"` + sensitive + `"}`},
		{name: "空响应"},
		{name: "损坏 JSON", body: `{"ok":`},
		{name: "缺少 ok", body: `{"result":{"message_id":1}}`},
		{name: "尾随 JSON", body: `{"ok":true}{"ok":true}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tt.body))
			}))
			defer server.Close()

			const fixtureCredential = "telegram-test-token"
			sender, err := NewTelegramSender(slog.Default(), TelegramConfig{
				BotToken: fixtureCredential, ChatID: "12345", APIUrl: server.URL,
			})
			if err != nil {
				t.Fatal(err)
			}
			sender.retryWait = func(context.Context, time.Duration) error { return nil }
			err = sender.Send(t.Context(), Message{Title: "Test"})
			if err == nil {
				t.Fatal("Send() error = nil")
			}
			for _, secret := range []string{fixtureCredential, server.URL, sensitive} {
				if strings.Contains(err.Error(), secret) {
					t.Fatalf("Send() error 泄漏敏感信息 %q: %v", secret, err)
				}
			}
		})
	}
}

func TestTelegramSenderAPIURLUsesV2RootJoin(t *testing.T) {
	t.Parallel()

	paths := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths <- r.URL.EscapedPath()
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":1}}`))
	}))
	defer server.Close()

	sender, err := NewTelegramSender(slog.Default(), TelegramConfig{
		BotToken: "test-token", ChatID: "12345", APIUrl: server.URL + "/nested/base/",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := sender.Send(t.Context(), Message{Title: "Test"}); err != nil {
		t.Fatal(err)
	}
	if got := <-paths; got != "/bottest-token/sendMessage" {
		t.Fatalf("request path = %q, want V2 root-joined Bot API path", got)
	}
}

func TestTelegramSender_FormatMessage_MarkdownV2(t *testing.T) {
	sender := &TelegramSender{
		parseMode: "MarkdownV2",
		logger:    slog.Default(),
	}

	msg := Message{
		Title: "Test [Title]",
		Text:  "Body with _special_ chars",
		Link:  "https://example.com/path?query=1",
	}

	formatted := sender.formatMessage(msg)
	want := "*Test \\[Title\\]*\nBody with _special_ chars\n" +
		"[查看详情](https://example.com/path?query=1)"
	if formatted != want {
		t.Fatalf("formatMessage() = %q, want %q", formatted, want)
	}
}

func TestTelegramSender_FormatMessage_HTML(t *testing.T) {
	sender := &TelegramSender{
		parseMode: "HTML",
		logger:    slog.Default(),
	}

	msg := Message{
		Title: "Test <Title>",
		Text:  "Body with & chars",
		Link:  "https://example.com",
	}

	formatted := sender.formatMessage(msg)
	want := "<b>Test &lt;Title&gt;</b>\nBody with & chars\n" +
		"<a href=\"https://example.com\">查看详情</a>"
	if formatted != want {
		t.Fatalf("formatMessage() = %q, want %q", formatted, want)
	}
}

func TestNewTelegramSender_MissingConfig(t *testing.T) {
	tests := []struct {
		name   string
		config TelegramConfig
	}{
		{"no bot token", TelegramConfig{ChatID: "123"}},
		{"no chat id", TelegramConfig{BotToken: "token"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewTelegramSender(slog.Default(), tt.config)
			if err == nil {
				t.Errorf("expected error for %s", tt.name)
			}
		})
	}
}

func TestTelegramSender_Name(t *testing.T) {
	sender := &TelegramSender{}
	if sender.Name() != ChannelTelegram {
		t.Errorf("expected channel name %q, got %q", ChannelTelegram, sender.Name())
	}
}

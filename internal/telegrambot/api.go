// Package telegrambot implements the inbound half of the Telegram integration:
// long polling of getUpdates, slash commands, and inline-keyboard callbacks.
package telegrambot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	defaultAPIURL   = "https://api.telegram.org"
	defaultPollTime = 25 * time.Second
	maxReplyRunes   = 3800
)

var (
	// ErrEmptyToken reports a Telegram channel without a bot token.
	ErrEmptyToken = errors.New("Telegram 机器人 Token 为空")
	// ErrAPIRequest reports a transport failure or a non-ok Bot API envelope.
	ErrAPIRequest = errors.New("Telegram 接口请求失败")
)

// Chat is the Bot API chat object subset this package needs.
type Chat struct {
	ID       int64  `json:"id"`
	Type     string `json:"type"`
	Title    string `json:"title"`
	Username string `json:"username"`
}

// User is the Bot API user object subset this package needs.
type User struct {
	ID        int64  `json:"id"`
	IsBot     bool   `json:"is_bot"`
	FirstName string `json:"first_name"`
	Username  string `json:"username"`
}

// Message is the Bot API message object subset this package needs.
type Message struct {
	MessageID int    `json:"message_id"`
	Chat      Chat   `json:"chat"`
	From      *User  `json:"from"`
	Text      string `json:"text"`
}

// CallbackQuery is an inline-keyboard button press.
type CallbackQuery struct {
	ID      string   `json:"id"`
	From    User     `json:"from"`
	Message *Message `json:"message"`
	Data    string   `json:"data"`
}

// Update is one Bot API update.
type Update struct {
	UpdateID      int64          `json:"update_id"`
	Message       *Message       `json:"message"`
	CallbackQuery *CallbackQuery `json:"callback_query"`
}

// InlineButton is one inline-keyboard button.
type InlineButton struct {
	Text         string `json:"text"`
	CallbackData string `json:"callback_data,omitempty"`
	URL          string `json:"url,omitempty"`
}

// InlineKeyboard is an inline keyboard markup.
type InlineKeyboard struct {
	InlineKeyboard [][]InlineButton `json:"inline_keyboard"`
}

type apiEnvelope struct {
	OK          bool            `json:"ok"`
	Result      json.RawMessage `json:"result"`
	Description string          `json:"description"`
	ErrorCode   int             `json:"error_code"`
}

type apiClient struct {
	baseURL string
	token   string
	client  *http.Client
}

func newAPIClient(baseURL, token string, client *http.Client) *apiClient {
	base := strings.TrimSpace(baseURL)
	if base == "" {
		base = defaultAPIURL
	}
	base = strings.TrimSuffix(base, "/")
	if client == nil {
		client = &http.Client{Timeout: defaultPollTime + 30*time.Second}
	}
	return &apiClient{baseURL: base, token: strings.TrimSpace(token), client: client}
}

func (c *apiClient) endpoint(method string) string {
	return fmt.Sprintf("%s/bot%s/%s", c.baseURL, c.token, strings.TrimSpace(method))
}

// call performs one Bot API request and decodes its result into out.
func (c *apiClient) call(ctx context.Context, method string, payload any, out any) error {
	if c.token == "" {
		return ErrEmptyToken
	}
	body := []byte("{}")
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrAPIRequest, err)
		}
		body = encoded
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint(method), bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("%w: %v", ErrAPIRequest, err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrAPIRequest, err)
	}
	defer resp.Body.Close()
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if readErr != nil {
		return fmt.Errorf("%w: %v", ErrAPIRequest, readErr)
	}
	var envelope apiEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return fmt.Errorf("%w: HTTP %d", ErrAPIRequest, resp.StatusCode)
	}
	if !envelope.OK {
		description := strings.TrimSpace(envelope.Description)
		if description == "" {
			description = "HTTP " + strconv.Itoa(resp.StatusCode)
		}
		return fmt.Errorf("%w: %s", ErrAPIRequest, description)
	}
	if out == nil || len(envelope.Result) == 0 {
		return nil
	}
	if err := json.Unmarshal(envelope.Result, out); err != nil {
		return fmt.Errorf("%w: %v", ErrAPIRequest, err)
	}
	return nil
}

// getUpdates long-polls the Bot API and returns up to 100 pending updates.
func (c *apiClient) getUpdates(ctx context.Context, offset int64, poll time.Duration) ([]Update, error) {
	if poll <= 0 {
		poll = defaultPollTime
	}
	payload := map[string]any{
		"offset":          offset,
		"timeout":         int(poll / time.Second),
		"allowed_updates": []string{"message", "callback_query"},
	}
	var updates []Update
	if err := c.call(ctx, "getUpdates", payload, &updates); err != nil {
		return nil, err
	}
	return updates, nil
}

// sendMessage sends plain text with an optional inline keyboard.
func (c *apiClient) sendMessage(ctx context.Context, chatID int64, text string, keyboard *InlineKeyboard) (int, error) {
	payload := map[string]any{
		"chat_id":                  chatID,
		"text":                     truncateText(text, maxReplyRunes),
		"disable_web_page_preview": true,
	}
	if keyboard != nil && len(keyboard.InlineKeyboard) > 0 {
		payload["reply_markup"] = keyboard
	}
	var message Message
	if err := c.call(ctx, "sendMessage", payload, &message); err != nil {
		return 0, err
	}
	return message.MessageID, nil
}

// editMessageText rewrites a previously sent message in place.
func (c *apiClient) editMessageText(ctx context.Context, chatID int64, messageID int, text string, keyboard *InlineKeyboard) error {
	payload := map[string]any{
		"chat_id":                  chatID,
		"message_id":               messageID,
		"text":                     truncateText(text, maxReplyRunes),
		"disable_web_page_preview": true,
	}
	if keyboard != nil {
		payload["reply_markup"] = keyboard
	}
	return c.call(ctx, "editMessageText", payload, nil)
}

// answerCallback acknowledges a button press so the client stops spinning.
func (c *apiClient) answerCallback(ctx context.Context, id, text string) error {
	payload := map[string]any{"callback_query_id": id}
	if trimmed := strings.TrimSpace(text); trimmed != "" {
		payload["text"] = truncateText(trimmed, 200)
	}
	return c.call(ctx, "answerCallbackQuery", payload, nil)
}

// deleteMessage removes a message the bot sent.
func (c *apiClient) deleteMessage(ctx context.Context, chatID int64, messageID int) error {
	payload := map[string]any{"chat_id": chatID, "message_id": messageID}
	return c.call(ctx, "deleteMessage", payload, nil)
}

// getMe validates the token and returns the bot identity.
func (c *apiClient) getMe(ctx context.Context) (User, error) {
	var user User
	err := c.call(ctx, "getMe", nil, &user)
	return user, err
}

func truncateText(text string, limit int) string {
	runes := []rune(strings.TrimSpace(text))
	if limit <= 0 || len(runes) <= limit {
		return string(runes)
	}
	return string(runes[:limit]) + "…"
}

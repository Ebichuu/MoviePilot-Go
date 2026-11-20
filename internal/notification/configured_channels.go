package notification

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/moviepilot-go/moviepilot-go/internal/notificationconfig"
)

const configuredChannelTimeout = 20 * time.Second

type configuredChannelSender struct {
	logger  *slog.Logger
	channel Channel
	name    string
	config  map[string]any
	client  *http.Client

	mu           sync.Mutex
	token        string
	tokenExpires time.Time
}

func newConfiguredChannelSender(logger *slog.Logger, channel Channel, config notificationconfig.Config) (ChannelSender, bool) {
	if logger == nil {
		logger = slog.Default()
	}
	sender := &configuredChannelSender{
		logger:  logger,
		channel: channel,
		name:    config.Name,
		config:  config.Config,
		client:  &http.Client{Timeout: configuredChannelTimeout},
	}
	valid := false
	switch channel {
	case ChannelDiscord:
		valid = sender.value("DISCORD_BOT_TOKEN") != ""
	case ChannelFeishu:
		valid = sender.value("FEISHU_APP_ID") != "" && sender.value("FEISHU_APP_SECRET") != "" &&
			(sender.value("FEISHU_OPEN_ID") != "" || sender.value("FEISHU_CHAT_ID") != "")
	case ChannelSlack:
		valid = sender.value("SLACK_OAUTH_TOKEN") != ""
	case ChannelSynologyChat:
		valid = sender.value("SYNOLOGYCHAT_WEBHOOK") != "" && sender.value("SYNOLOGYCHAT_TOKEN") != ""
	case ChannelVoceChat:
		valid = sender.value("VOCECHAT_HOST") != "" && sender.value("VOCECHAT_API_KEY") != "" && sender.value("VOCECHAT_CHANNEL_ID") != ""
	case ChannelQQ:
		valid = sender.value("QQ_APP_ID") != "" && sender.value("QQ_APP_SECRET") != ""
	case ChannelWeChat:
		// The enterprise app mode is an HTTP API. WeChat Bot mode uses a
		// separate websocket protocol and is not projected as an HTTP sender.
		valid = sender.value("WECHAT_MODE") != "bot" && sender.value("WECHAT_CORPID") != "" &&
			sender.value("WECHAT_APP_SECRET") != "" && sender.value("WECHAT_APP_ID") != ""
	}
	if !valid {
		return nil, false
	}
	return sender, true
}

func (s *configuredChannelSender) Name() Channel { return s.channel }

func (s *configuredChannelSender) Send(ctx context.Context, msg Message) error {
	if ctx == nil || s == nil || s.client == nil {
		return errors.New("notification channel unavailable")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	switch s.channel {
	case ChannelDiscord:
		return s.sendDiscord(ctx, msg)
	case ChannelFeishu:
		return s.sendFeishu(ctx, msg)
	case ChannelSlack:
		return s.sendSlack(ctx, msg)
	case ChannelSynologyChat:
		return s.sendSynologyChat(ctx, msg)
	case ChannelVoceChat:
		return s.sendVoceChat(ctx, msg)
	case ChannelQQ:
		return s.sendQQ(ctx, msg)
	case ChannelWeChat:
		return s.sendWeChat(ctx, msg)
	default:
		return fmt.Errorf("unsupported configured notification channel")
	}
}

func (s *configuredChannelSender) value(key string) string {
	value, ok := s.config[key]
	if !ok || value == nil {
		return ""
	}
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed)
	case json.Number:
		return typed.String()
	default:
		return strings.TrimSpace(fmt.Sprint(value))
	}
}

func (s *configuredChannelSender) postJSON(ctx context.Context, endpoint string, payload any, headers map[string]string) (map[string]any, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, errors.New("encode notification request")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, errors.New("build notification request")
	}
	request.Header.Set("Content-Type", "application/json")
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	response, err := s.client.Do(request)
	if err != nil {
		return nil, errors.New("notification request failed")
	}
	defer response.Body.Close()
	responseBody, readErr := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if readErr != nil {
		return nil, errors.New("read notification response")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("notification service returned HTTP %d", response.StatusCode)
	}
	var result map[string]any
	if len(bytes.TrimSpace(responseBody)) != 0 {
		if err := json.Unmarshal(responseBody, &result); err != nil {
			return nil, errors.New("invalid notification response")
		}
	}
	if result != nil {
		if ok, exists := result["ok"].(bool); exists && !ok {
			return nil, errors.New("notification service rejected the message")
		}
		if code, exists := result["code"]; exists && fmt.Sprint(code) != "0" {
			return nil, errors.New("notification service rejected the message")
		}
		if errCode, exists := result["errcode"]; exists && fmt.Sprint(errCode) != "0" {
			return nil, errors.New("notification service rejected the message")
		}
	}
	return result, nil
}

func (s *configuredChannelSender) getJSON(ctx context.Context, endpoint string, headers map[string]string) (map[string]any, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, errors.New("build notification request")
	}
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	response, err := s.client.Do(request)
	if err != nil {
		return nil, errors.New("notification request failed")
	}
	defer response.Body.Close()
	responseBody, readErr := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if readErr != nil {
		return nil, errors.New("read notification response")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("notification service returned HTTP %d", response.StatusCode)
	}
	var result map[string]any
	if err := json.Unmarshal(responseBody, &result); err != nil {
		return nil, errors.New("invalid notification response")
	}
	if ok, exists := result["ok"].(bool); exists && !ok {
		return nil, errors.New("notification service rejected the request")
	}
	if code, exists := result["code"]; exists && fmt.Sprint(code) != "0" {
		return nil, errors.New("notification service rejected the request")
	}
	if errCode, exists := result["errcode"]; exists && fmt.Sprint(errCode) != "0" {
		return nil, errors.New("notification service rejected the request")
	}
	return result, nil
}

func (s *configuredChannelSender) getJSONArray(ctx context.Context, endpoint string, headers map[string]string) ([]any, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, errors.New("build notification request")
	}
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	response, err := s.client.Do(request)
	if err != nil {
		return nil, errors.New("notification request failed")
	}
	defer response.Body.Close()
	responseBody, readErr := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if readErr != nil {
		return nil, errors.New("read notification response")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("notification service returned HTTP %d", response.StatusCode)
	}
	var result []any
	if err := json.Unmarshal(responseBody, &result); err != nil {
		return nil, errors.New("invalid notification response")
	}
	return result, nil
}

func (s *configuredChannelSender) sendDiscord(ctx context.Context, msg Message) error {
	token := s.value("DISCORD_BOT_TOKEN")
	channelID := s.value("DISCORD_CHANNEL_ID")
	if msg.UserID != "" {
		if msg.UserID == s.value("DISCORD_CHANNEL_ID") {
			channelID = msg.UserID
		} else {
			result, err := s.postJSON(ctx, "https://discord.com/api/v10/users/@me/channels", map[string]string{"recipient_id": msg.UserID}, map[string]string{
				"Authorization": "Bot " + token,
			})
			if err != nil {
				return err
			}
			channelID, _ = result["id"].(string)
		}
	}
	if channelID == "" {
		if guildID := s.value("DISCORD_GUILD_ID"); guildID != "" {
			channels, err := s.getJSONArray(ctx, "https://discord.com/api/v10/guilds/"+url.PathEscape(guildID)+"/channels", map[string]string{
				"Authorization": "Bot " + token,
			})
			if err == nil {
				for _, item := range channels {
					channel, _ := item.(map[string]any)
					if fmt.Sprint(channel["type"]) == "0" {
						channelID, _ = channel["id"].(string)
						break
					}
				}
			}
		}
	}
	if channelID == "" {
		return errors.New("discord destination is not configured")
	}
	payload := map[string]any{
		"allowed_mentions": map[string]any{"parse": []string{}},
	}
	if msg.Image != "" {
		embed := map[string]any{"title": msg.Title, "description": msg.Text, "image": map[string]string{"url": msg.Image}}
		if msg.Link != "" {
			embed["url"] = msg.Link
		}
		payload["embeds"] = []any{embed}
	} else {
		content := messageText(msg)
		if len(content) > 2000 {
			content = content[:2000]
		}
		payload["content"] = content
	}
	_, err := s.postJSON(ctx, "https://discord.com/api/v10/channels/"+url.PathEscape(channelID)+"/messages", payload,
		map[string]string{"Authorization": "Bot " + token})
	return err
}

func (s *configuredChannelSender) sendFeishu(ctx context.Context, msg Message) error {
	token, err := s.feishuToken(ctx)
	if err != nil {
		return err
	}
	receiveID, receiveType := msg.UserID, "open_id"
	if receiveID == "" {
		if chatID := s.value("FEISHU_CHAT_ID"); chatID != "" {
			receiveID, receiveType = chatID, "chat_id"
		} else {
			receiveID = s.value("FEISHU_OPEN_ID")
		}
	}
	if receiveID == "" {
		return errors.New("feishu destination is not configured")
	}
	contentText := messageText(msg)
	if msg.Image != "" {
		contentText += "\n" + msg.Image
	}
	content, _ := json.Marshal(map[string]string{"text": contentText})
	_, err = s.postJSON(ctx, "https://open.feishu.cn/open-apis/im/v1/messages?receive_id_type="+url.QueryEscape(receiveType), map[string]any{
		"receive_id": receiveID,
		"msg_type":   "text",
		"content":    string(content),
	}, map[string]string{"Authorization": "Bearer " + token})
	return err
}

func (s *configuredChannelSender) feishuToken(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.token != "" && time.Now().Add(30*time.Second).Before(s.tokenExpires) {
		return s.token, nil
	}
	result, err := s.postJSON(ctx, "https://open.feishu.cn/open-apis/auth/v3/tenant_access_token/internal", map[string]string{
		"app_id": s.value("FEISHU_APP_ID"), "app_secret": s.value("FEISHU_APP_SECRET"),
	}, nil)
	if err != nil {
		return "", err
	}
	token, _ := result["tenant_access_token"].(string)
	if token == "" {
		return "", errors.New("feishu access token was not returned")
	}
	expires, _ := strconv.Atoi(fmt.Sprint(result["expire"]))
	if expires < 60 {
		expires = 60
	}
	s.token, s.tokenExpires = token, time.Now().Add(time.Duration(expires)*time.Second)
	return token, nil
}

func (s *configuredChannelSender) sendSlack(ctx context.Context, msg Message) error {
	token := s.value("SLACK_OAUTH_TOKEN")
	channel := msg.UserID
	if channel == "" {
		channel = s.value("SLACK_CHANNEL")
	}
	if channel == "" || !isSlackChannelID(channel) {
		name := channel
		if name == "" {
			name = "全体"
		}
		channel, _ = s.findSlackChannel(ctx, token, name)
	}
	if channel == "" {
		return errors.New("slack destination is not configured")
	}
	text := messageText(msg)
	payload := map[string]any{
		"channel": channel,
		"text":    text,
		"mrkdwn":  true,
	}
	if msg.Image != "" {
		section := map[string]any{
			"type":      "section",
			"text":      map[string]string{"type": "mrkdwn", "text": "*" + msg.Title + "*\n" + msg.Text},
			"accessory": map[string]any{"type": "image", "image_url": msg.Image, "alt_text": msg.Title},
		}
		blocks := []any{section}
		if msg.Link != "" {
			blocks = append(blocks, map[string]any{
				"type": "actions",
				"elements": []any{map[string]any{
					"type":      "button",
					"text":      map[string]any{"type": "plain_text", "text": "查看详情", "emoji": true},
					"url":       msg.Link,
					"action_id": "actionId-url",
				}},
			})
		}
		payload["blocks"] = blocks
	}
	_, err := s.postJSON(ctx, "https://slack.com/api/chat.postMessage", payload,
		map[string]string{"Authorization": "Bearer " + token})
	return err
}

func isSlackChannelID(value string) bool {
	return strings.HasPrefix(value, "C") || strings.HasPrefix(value, "G") || strings.HasPrefix(value, "D")
}

func (s *configuredChannelSender) findSlackChannel(ctx context.Context, token, name string) (string, error) {
	cursor := ""
	for page := 0; page < 10; page++ {
		endpoint := "https://slack.com/api/conversations.list?types=public_channel,private_channel&limit=200"
		if cursor != "" {
			endpoint += "&cursor=" + url.QueryEscape(cursor)
		}
		result, err := s.getJSON(ctx, endpoint, map[string]string{"Authorization": "Bearer " + token})
		if err != nil {
			return "", err
		}
		if channels, ok := result["channels"].([]any); ok {
			for _, item := range channels {
				channel, _ := item.(map[string]any)
				if channel["name"] == name {
					return fmt.Sprint(channel["id"]), nil
				}
			}
		}
		meta, _ := result["response_metadata"].(map[string]any)
		cursor, _ = meta["next_cursor"].(string)
		if cursor == "" {
			break
		}
	}
	return "", nil
}

func (s *configuredChannelSender) sendSynologyChat(ctx context.Context, msg Message) error {
	webhook := s.value("SYNOLOGYCHAT_WEBHOOK")
	token := s.value("SYNOLOGYCHAT_TOKEN")
	var targets []int64
	if msg.UserID != "" {
		id, err := strconv.ParseInt(msg.UserID, 10, 64)
		if err != nil || id <= 0 {
			return errors.New("synology chat recipient is invalid")
		}
		targets = append(targets, id)
	}
	if msg.UserID == "" {
		webhookURL, err := url.Parse(webhook)
		if err != nil || webhookURL.Host == "" || (webhookURL.Scheme != "http" && webhookURL.Scheme != "https") {
			return errors.New("synology chat webhook is invalid")
		}
		base := &url.URL{Scheme: webhookURL.Scheme, Host: webhookURL.Host, Path: "/webapi/entry.cgi"}
		query := base.Query()
		query.Set("api", "SYNO.Chat.External")
		query.Set("method", "user_list")
		query.Set("version", "2")
		query.Set("token", token)
		base.RawQuery = query.Encode()
		result, err := s.getJSON(ctx, base.String(), nil)
		if err != nil {
			return err
		}
		data, _ := result["data"].(map[string]any)
		users, _ := data["users"].([]any)
		targets = targets[:0]
		for _, item := range users {
			user, _ := item.(map[string]any)
			deleted, _ := user["deleted"].(bool)
			if !deleted && user["user_id"] != nil {
				id, _ := strconv.ParseInt(fmt.Sprint(user["user_id"]), 10, 64)
				if id > 0 {
					targets = append(targets, id)
				}
			}
		}
	}
	if len(targets) == 0 {
		return errors.New("synology chat has no recipient")
	}
	text := strings.ReplaceAll(messageText(msg), "\n\n", "\n")
	if msg.Link != "" {
		text += "\n[查看详情](" + msg.Link + ")"
	}
	payload := map[string]any{"text": text, "user_ids": targets}
	if msg.Image != "" {
		payload["file_url"] = msg.Image
	}
	encodedPayload, _ := json.Marshal(payload)
	form := url.Values{"payload": {string(encodedPayload)}}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, webhook, strings.NewReader(form.Encode()))
	if err != nil {
		return errors.New("build notification request")
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := s.client.Do(request)
	if err != nil {
		return errors.New("notification request failed")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("notification service returned HTTP %d", response.StatusCode)
	}
	var result map[string]any
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&result); err != nil {
		return errors.New("invalid notification response")
	}
	if serviceError, ok := result["error"].(map[string]any); ok {
		if code := serviceError["code"]; code != nil && fmt.Sprint(code) != "0" {
			return errors.New("synology chat rejected the message")
		}
	}
	return nil
}

func (s *configuredChannelSender) sendVoceChat(ctx context.Context, msg Message) error {
	host := strings.TrimRight(strings.TrimSpace(s.value("VOCECHAT_HOST")), "/")
	if !strings.Contains(host, "://") {
		host = "http://" + host
	}
	parsed, err := url.Parse(host)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return errors.New("vocechat host is invalid")
	}
	target, action := msg.UserID, "send_to_user"
	if target == "" {
		target, action = "GID#"+s.value("VOCECHAT_CHANNEL_ID"), "send_to_group"
	}
	if strings.HasPrefix(target, "GID#") {
		target, action = strings.TrimPrefix(target, "GID#"), "send_to_group"
	} else if strings.HasPrefix(target, "UID#") {
		target = strings.TrimPrefix(target, "UID#")
	}
	content := "**" + msg.Title + "**"
	if msg.Text != "" {
		content += "\n" + msg.Text
	}
	if msg.Image != "" {
		content += "\n![](" + msg.Image + ")"
	}
	if msg.Link != "" {
		content += "\n[查看详情](" + msg.Link + ")"
	}
	endpoint := strings.TrimRight(host, "/") + "/api/bot/" + action + "/" + url.PathEscape(target)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(content))
	if err != nil {
		return errors.New("build notification request")
	}
	request.Header.Set("Content-Type", "text/markdown")
	request.Header.Set("Accept", "application/json; charset=utf-8")
	request.Header.Set("x-api-key", s.value("VOCECHAT_API_KEY"))
	response, err := s.client.Do(request)
	if err != nil {
		return errors.New("notification request failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("notification service returned HTTP %d", response.StatusCode)
	}
	return nil
}

func (s *configuredChannelSender) sendQQ(ctx context.Context, msg Message) error {
	token, err := s.qqToken(ctx)
	if err != nil {
		return err
	}
	target, group := msg.UserID, false
	if strings.HasPrefix(target, "group:") {
		target, group = strings.TrimPrefix(target, "group:"), true
	} else if strings.HasPrefix(target, "GROUP#") {
		target, group = strings.TrimPrefix(target, "GROUP#"), true
	} else if strings.HasPrefix(target, "USER#") {
		target = strings.TrimPrefix(target, "USER#")
	}
	if target == "" {
		if target = s.value("QQ_GROUP_OPENID"); target != "" {
			group = true
		} else {
			target = s.value("QQ_OPENID")
		}
	}
	if target == "" {
		return errors.New("qq destination is not configured")
	}
	content := messageText(msg)
	if msg.Image != "" {
		content += "\n" + msg.Image
	}
	body := map[string]any{"content": content, "msg_type": 0}
	path := "/v2/users/"
	if group {
		path = "/v2/groups/"
	}
	_, err = s.postJSON(ctx, "https://api.sgroup.qq.com"+path+url.PathEscape(target)+"/messages", body,
		map[string]string{"Authorization": "QQBot " + token})
	return err
}

func (s *configuredChannelSender) qqToken(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.token != "" && time.Now().Add(time.Minute).Before(s.tokenExpires) {
		return s.token, nil
	}
	result, err := s.postJSON(ctx, "https://bots.qq.com/app/getAppAccessToken", map[string]string{
		"appId": s.value("QQ_APP_ID"), "clientSecret": s.value("QQ_APP_SECRET"),
	}, nil)
	if err != nil {
		return "", err
	}
	token, _ := result["access_token"].(string)
	if token == "" {
		return "", errors.New("qq access token was not returned")
	}
	expires, _ := strconv.Atoi(fmt.Sprint(result["expires_in"]))
	if expires < 60 {
		expires = 7200
	}
	s.token, s.tokenExpires = token, time.Now().Add(time.Duration(expires)*time.Second)
	return token, nil
}

func (s *configuredChannelSender) sendWeChat(ctx context.Context, msg Message) error {
	token, err := s.wechatToken(ctx)
	if err != nil {
		return err
	}
	appID := s.value("WECHAT_APP_ID")
	agentID, err := strconv.Atoi(appID)
	if err != nil {
		return errors.New("wechat app id is invalid")
	}
	proxy := strings.TrimRight(s.value("WECHAT_PROXY"), "/")
	if proxy == "" {
		proxy = "https://qyapi.weixin.qq.com"
	}
	target := msg.UserID
	if target == "" {
		target = "@all"
	}
	var payload map[string]any
	if msg.Image != "" {
		payload = map[string]any{
			"touser": target, "msgtype": "news", "agentid": agentID,
			"news": map[string]any{"articles": []any{map[string]string{
				"title": msg.Title, "description": msg.Text, "picurl": msg.Image, "url": msg.Link,
			}}},
		}
	} else {
		content := strings.ReplaceAll(msg.Text, "\n\n", "\n")
		if msg.Title != "" {
			content = msg.Title + "\n" + content
		}
		if msg.Link != "" {
			content += "\n点击查看：" + msg.Link
		}
		for _, chunk := range splitWechatContent(content, 2048) {
			payload = map[string]any{
				"touser": target, "msgtype": "text", "agentid": agentID,
				"text": map[string]string{"content": chunk},
				"safe": 0, "enable_id_trans": 0, "enable_duplicate_check": 0,
			}
			endpoint := proxy + "/cgi-bin/message/send?access_token=" + url.QueryEscape(token)
			if _, err := s.postJSON(ctx, endpoint, payload, nil); err != nil {
				return err
			}
		}
		return nil
	}
	endpoint := proxy + "/cgi-bin/message/send?access_token=" + url.QueryEscape(token)
	_, err = s.postJSON(ctx, endpoint, payload, nil)
	return err
}

func splitWechatContent(content string, maxBytes int) []string {
	if content == "" {
		return []string{""}
	}
	chunks := make([]string, 0, 1)
	for len(content) > maxBytes {
		cut := maxBytes
		for cut > 0 && (content[cut]&0xc0) == 0x80 {
			cut--
		}
		if cut == 0 {
			_, width := utf8.DecodeRuneInString(content)
			cut = width
		}
		chunks = append(chunks, content[:cut])
		content = content[cut:]
	}
	if content != "" {
		chunks = append(chunks, content)
	}
	return chunks
}

func (s *configuredChannelSender) wechatToken(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.token != "" && time.Now().Add(time.Minute).Before(s.tokenExpires) {
		return s.token, nil
	}
	proxy := strings.TrimRight(s.value("WECHAT_PROXY"), "/")
	if proxy == "" {
		proxy = "https://qyapi.weixin.qq.com"
	}
	endpoint := proxy + "/cgi-bin/gettoken?corpid=" + url.QueryEscape(s.value("WECHAT_CORPID")) +
		"&corpsecret=" + url.QueryEscape(s.value("WECHAT_APP_SECRET"))
	result, err := s.getJSON(ctx, endpoint, nil)
	if err != nil {
		return "", err
	}
	token, _ := result["access_token"].(string)
	if token == "" {
		return "", errors.New("wechat access token was not returned")
	}
	expires, _ := strconv.Atoi(fmt.Sprint(result["expires_in"]))
	if expires < 60 {
		expires = 7200
	}
	s.token, s.tokenExpires = token, time.Now().Add(time.Duration(expires)*time.Second)
	return token, nil
}

func messageText(msg Message) string {
	parts := make([]string, 0, 3)
	if msg.Title != "" {
		parts = append(parts, msg.Title)
	}
	if msg.Text != "" {
		parts = append(parts, msg.Text)
	}
	if msg.Link != "" {
		parts = append(parts, msg.Link)
	}
	return strings.Join(parts, "\n")
}

var _ ChannelSender = (*configuredChannelSender)(nil)

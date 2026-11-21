package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const maxWebhookBodyBytes int64 = 2 << 20

// WebhookEvent 是从 Emby / Jellyfin / Plex Webhook 报文解析出的统一事件视图。
type WebhookEvent struct {
	Source     string `json:"source"`
	Event      string `json:"event"`
	Title      string `json:"title"`
	Text       string `json:"text"`
	ItemName   string `json:"item_name,omitempty"`
	ItemType   string `json:"item_type,omitempty"`
	Username   string `json:"username,omitempty"`
	Client     string `json:"client,omitempty"`
	Device     string `json:"device,omitempty"`
	ServerName string `json:"server_name,omitempty"`
}

// WebhookNotifier 将解析成功的 Webhook 事件发送到已配置的通知渠道。
type WebhookNotifier interface {
	NotifyWebhook(ctx context.Context, event WebhookEvent) error
}

// handleWebhook 对应 V2 `POST /api/v1/webhook/`（兼容 Emby / Jellyfin JSON 及 Plex multipart payload）。
func (d Dependencies) handleWebhook(w http.ResponseWriter, r *http.Request) {
	sourceHint := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("source")))
	payload, err := readWebhookPayload(r, maxWebhookBodyBytes)
	if err != nil {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "读取 Webhook 报文失败"})
		return
	}
	if len(payload) == 0 {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "Webhook 报文内容为空"})
		return
	}

	event, ok := parseWebhookPayload(payload, sourceHint)
	if !ok {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "无法识别的 Webhook 报文格式"})
		return
	}

	if d.Logger != nil {
		d.Logger.Info("收到媒体服务器 Webhook 事件",
			"source", event.Source,
			"event", event.Event,
			"item", event.ItemName,
			"user", event.Username,
		)
	}

	notified := false
	if d.WebhookNotifier != nil && event.Title != "" {
		if err := d.WebhookNotifier.NotifyWebhook(r.Context(), event); err != nil {
			if d.Logger != nil {
				d.Logger.Warn("Webhook 通知发送失败", "source", event.Source, "event", event.Event, "error", err)
			}
		} else {
			notified = true
		}
	}

	writeJSON(w, http.StatusOK, Response{
		Success: true,
		Message: "Webhook 已处理",
		Data: map[string]any{
			"source":   event.Source,
			"event":    event.Event,
			"title":    event.Title,
			"notified": notified,
		},
	})
}

func readWebhookPayload(r *http.Request, limit int64) ([]byte, error) {
	if r == nil || r.Body == nil {
		return nil, nil
	}
	ct := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Type")))
	if strings.HasPrefix(ct, "multipart/form-data") {
		if err := r.ParseMultipartForm(limit); err != nil {
			return nil, err
		}
		if raw := strings.TrimSpace(r.FormValue("payload")); raw != "" {
			return []byte(raw), nil
		}
		return nil, nil
	}
	if strings.HasPrefix(ct, "application/x-www-form-urlencoded") {
		if err := r.ParseForm(); err != nil {
			return nil, err
		}
		if raw := strings.TrimSpace(r.FormValue("payload")); raw != "" {
			return []byte(raw), nil
		}
		return nil, nil
	}
	return io.ReadAll(io.LimitReader(r.Body, limit))
}

func parseWebhookPayload(raw []byte, sourceHint string) (WebhookEvent, bool) {
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil || body == nil {
		return WebhookEvent{}, false
	}

	// 1. Jellyfin Webhook Plugin（带 NotificationType 字段）
	if nt := strings.TrimSpace(stringField(body, "NotificationType")); nt != "" {
		return parseJellyfinWebhook(body, nt), true
	}

	// 2. Plex Webhook（带小写 event 且包含 Account / Metadata / Server）
	if plexEvent := strings.TrimSpace(stringField(body, "event")); plexEvent != "" {
		return parsePlexWebhook(body, plexEvent), true
	}

	// 3. Emby Webhook（带大写 Event）
	if embyEvent := strings.TrimSpace(stringField(body, "Event")); embyEvent != "" {
		return parseEmbyWebhook(body, embyEvent), true
	}

	// 4. 兜底通用事件（如果调用方带了 title/message 或指定 source）
	title := strings.TrimSpace(stringField(body, "Title"))
	if title == "" {
		title = strings.TrimSpace(stringField(body, "title"))
	}
	text := strings.TrimSpace(stringField(body, "Description"))
	if text == "" {
		text = strings.TrimSpace(stringField(body, "message"))
	}
	if title != "" || text != "" {
		if sourceHint == "" {
			sourceHint = "webhook"
		}
		if title == "" {
			title = "Webhook 通知"
		}
		return WebhookEvent{Source: sourceHint, Event: "custom", Title: title, Text: text}, true
	}
	return WebhookEvent{}, false
}

func parseEmbyWebhook(body map[string]any, eventName string) WebhookEvent {
	itemMap := mapField(body, "Item")
	userMap := mapField(body, "User")
	sessionMap := mapField(body, "Session")
	serverMap := mapField(body, "Server")

	itemName := formatMediaWebhookItemName(
		stringField(itemMap, "SeriesName"),
		stringField(itemMap, "Name"),
		intField(itemMap, "ParentIndexNumber"),
		intField(itemMap, "IndexNumber"),
	)
	if itemName == "" {
		itemName = stringField(body, "Title")
	}
	username := stringField(userMap, "Name")
	client := stringField(sessionMap, "Client")
	device := stringField(sessionMap, "DeviceName")
	serverName := stringField(serverMap, "Name")
	if serverName == "" {
		serverName = "Emby"
	}

	action := describeMediaEvent(eventName)
	title := buildWebhookTitle(action, itemName, serverName)
	text := buildWebhookText(serverName, username, client, device, stringField(body, "Description"))
	return WebhookEvent{
		Source:     "emby",
		Event:      eventName,
		Title:      title,
		Text:       text,
		ItemName:   itemName,
		ItemType:   stringField(itemMap, "Type"),
		Username:   username,
		Client:     client,
		Device:     device,
		ServerName: serverName,
	}
}

func parseJellyfinWebhook(body map[string]any, eventName string) WebhookEvent {
	itemName := formatMediaWebhookItemName(
		stringField(body, "SeriesName"),
		stringField(body, "Name"),
		intField(body, "SeasonNumber"),
		intField(body, "EpisodeNumber"),
	)
	username := stringField(body, "NotificationUsername")
	if username == "" {
		username = stringField(body, "Username")
	}
	client := stringField(body, "ClientName")
	device := stringField(body, "DeviceName")
	serverName := stringField(body, "ServerName")
	if serverName == "" {
		serverName = "Jellyfin"
	}

	action := describeMediaEvent(eventName)
	title := buildWebhookTitle(action, itemName, serverName)
	text := buildWebhookText(serverName, username, client, device, stringField(body, "Overview"))
	return WebhookEvent{
		Source:     "jellyfin",
		Event:      eventName,
		Title:      title,
		Text:       text,
		ItemName:   itemName,
		ItemType:   stringField(body, "ItemType"),
		Username:   username,
		Client:     client,
		Device:     device,
		ServerName: serverName,
	}
}

func parsePlexWebhook(body map[string]any, eventName string) WebhookEvent {
	meta := mapField(body, "Metadata")
	account := mapField(body, "Account")
	player := mapField(body, "Player")
	server := mapField(body, "Server")

	itemName := formatMediaWebhookItemName(
		stringField(meta, "grandparentTitle"),
		stringField(meta, "title"),
		intField(meta, "parentIndex"),
		intField(meta, "index"),
	)
	username := stringField(account, "title")
	client := stringField(player, "title")
	device := stringField(player, "publicAddress")
	serverName := stringField(server, "title")
	if serverName == "" {
		serverName = "Plex"
	}

	action := describeMediaEvent(eventName)
	title := buildWebhookTitle(action, itemName, serverName)
	text := buildWebhookText(serverName, username, client, device, stringField(meta, "summary"))
	return WebhookEvent{
		Source:     "plex",
		Event:      eventName,
		Title:      title,
		Text:       text,
		ItemName:   itemName,
		ItemType:   stringField(meta, "type"),
		Username:   username,
		Client:     client,
		Device:     device,
		ServerName: serverName,
	}
}

func describeMediaEvent(event string) string {
	switch strings.ToLower(strings.TrimSpace(event)) {
	case "playback.start", "playbackstart", "media.play":
		return "开始播放"
	case "playback.pause", "media.pause":
		return "暂停播放"
	case "playback.unpause", "media.resume":
		return "恢复播放"
	case "playback.stop", "playbackstop", "media.stop":
		return "停止播放"
	case "media.scrobble", "item.markplayed":
		return "已看完"
	case "library.new", "itemadded":
		return "新入库"
	case "user.authenticated", "authenticationsuccess":
		return "用户登录成功"
	case "user.authenticationfailed", "authenticationfailure":
		return "用户登录失败"
	case "system.webhooktest", "test":
		return "Webhook 测试"
	default:
		return "媒体事件(" + strings.TrimSpace(event) + ")"
	}
}

func formatMediaWebhookItemName(seriesName, itemName string, season, episode int) string {
	seriesName = strings.TrimSpace(seriesName)
	itemName = strings.TrimSpace(itemName)
	if seriesName != "" && season > 0 && episode > 0 {
		label := fmt.Sprintf("%s S%02dE%02d", seriesName, season, episode)
		if itemName != "" && !strings.EqualFold(itemName, seriesName) {
			return label + " " + itemName
		}
		return label
	}
	if seriesName != "" && itemName != "" && !strings.EqualFold(itemName, seriesName) {
		return seriesName + " - " + itemName
	}
	if itemName != "" {
		return itemName
	}
	return seriesName
}

func buildWebhookTitle(action, itemName, serverName string) string {
	if itemName != "" {
		return fmt.Sprintf("%s %s", action, itemName)
	}
	return fmt.Sprintf("%s · %s", serverName, action)
}

func buildWebhookText(serverName, username, client, device, extra string) string {
	lines := make([]string, 0, 5)
	if serverName != "" {
		lines = append(lines, "服务器："+serverName)
	}
	if username != "" {
		lines = append(lines, "用户："+username)
	}
	if client != "" || device != "" {
		dev := strings.TrimSpace(strings.Trim(client+" "+device, " "))
		lines = append(lines, "设备："+dev)
	}
	if extra = strings.TrimSpace(extra); extra != "" {
		if len(extra) > 200 {
			extra = extra[:200] + "..."
		}
		lines = append(lines, extra)
	}
	return strings.Join(lines, "\n")
}

func mapField(m map[string]any, key string) map[string]any {
	if m == nil {
		return nil
	}
	sub, _ := m[key].(map[string]any)
	return sub
}

func stringField(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	v, ok := m[key]
	if !ok || v == nil {
		return ""
	}
	s, ok := v.(string)
	if ok {
		return strings.TrimSpace(s)
	}
	return strings.TrimSpace(fmt.Sprint(v))
}

func intField(m map[string]any, key string) int {
	if m == nil {
		return 0
	}
	switch v := m[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case json.Number:
		n, _ := v.Int64()
		return int(n)
	case string:
		var n int
		_, _ = fmt.Sscanf(strings.TrimSpace(v), "%d", &n)
		return n
	default:
		return 0
	}
}

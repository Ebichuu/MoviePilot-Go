package telegrambot

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	searchPageSize      = 5
	searchSessionTTL    = 30 * time.Minute
	searchSessionLimit  = 64
	searchResultCeiling = 30
)

// SearchResult is one normalized search hit handed to the bot.
type SearchResult struct {
	Title     string
	Site      string
	Size      int64
	Seeders   int
	DetailURL string
}

// Services is the small slice of the application the bot is allowed to touch.
// It is deliberately read-only: mutations stay in the Web UI.
type Services interface {
	// SearchTitles runs one indexer search and returns normalized hits.
	SearchTitles(ctx context.Context, keyword string, limit int) ([]SearchResult, error)
	// Subscriptions lists the operator's subscriptions as ready-to-print lines.
	Subscriptions(ctx context.Context) ([]string, error)
	// Sites lists configured indexer sites as ready-to-print lines.
	Sites(ctx context.Context) ([]string, error)
}

type searchSession struct {
	keyword string
	results []SearchResult
	touched time.Time
}

// Router turns Telegram updates into replies. It keeps only short-lived
// per-chat search sessions so inline buttons can address earlier results.
type Router struct {
	logger   *slog.Logger
	services Services
	now      func() time.Time

	mu       sync.Mutex
	sessions map[int64]*searchSession
}

// NewRouter builds a router. A nil Services disables every command except help.
func NewRouter(logger *slog.Logger, services Services) *Router {
	if logger == nil {
		logger = slog.Default()
	}
	return &Router{
		logger:   logger,
		services: services,
		now:      time.Now,
		sessions: make(map[int64]*searchSession),
	}
}

// HandleUpdate dispatches one update. Callback queries are always acknowledged.
func (r *Router) HandleUpdate(ctx context.Context, api *apiClient, update Update) {
	if r == nil || api == nil {
		return
	}
	switch {
	case update.CallbackQuery != nil:
		r.handleCallback(ctx, api, update.CallbackQuery)
	case update.Message != nil:
		r.handleMessage(ctx, api, update.Message)
	}
}

func (r *Router) handleMessage(ctx context.Context, api *apiClient, message *Message) {
	chatID := message.Chat.ID
	text := strings.TrimSpace(message.Text)
	if chatID == 0 {
		return
	}
	if !strings.HasPrefix(text, "/") {
		r.reply(ctx, api, chatID, "发送 /help 查看可用命令。", nil)
		return
	}
	command, argument := splitCommand(text)
	switch command {
	case "/start", "/help":
		r.reply(ctx, api, chatID, helpText, nil)
	case "/id":
		r.reply(ctx, api, chatID, fmt.Sprintf("chat_id: %d\nuser_id: %d", chatID, message.From.ID), nil)
	case "/search":
		r.handleSearch(ctx, api, chatID, argument, 1)
	case "/subs", "/subscriptions":
		r.handleList(ctx, api, chatID, "订阅", r.subscriptions)
	case "/sites":
		r.handleList(ctx, api, chatID, "站点", r.sites)
	case "/version":
		r.reply(ctx, api, chatID, "MoviePilot-Go · Telegram 入站已启用", nil)
	default:
		r.reply(ctx, api, chatID, "未知命令 "+command+"。发送 /help 查看可用命令。", nil)
	}
}

const helpText = `可用命令
/search <关键词>  搜索资源（结果带按钮）
/subs             我的订阅
/sites            站点列表
/id               查看当前 chat id
/help             显示本帮助

说明：下载与订阅的新增仍在 Web 端完成，机器人只做查询与跳转。`

func (r *Router) handleSearch(ctx context.Context, api *apiClient, chatID int64, keyword string, page int) {
	keyword = strings.TrimSpace(keyword)
	if keyword == "" {
		r.reply(ctx, api, chatID, "用法：/search 沙丘", nil)
		return
	}
	if r.services == nil {
		r.reply(ctx, api, chatID, "搜索服务未就绪，暂时无法搜索。", nil)
		return
	}
	results, err := r.services.SearchTitles(ctx, keyword, searchResultCeiling)
	if err != nil {
		r.logger.Warn("telegram 搜索失败", "error", err)
		r.reply(ctx, api, chatID, "搜索失败，请稍后再试。", nil)
		return
	}
	if len(results) == 0 {
		r.reply(ctx, api, chatID, "没有搜索到资源："+keyword, nil)
		return
	}
	r.storeSession(chatID, keyword, results)
	r.renderSearchPage(ctx, api, chatID, 0, 0)
}

func (r *Router) renderSearchPage(ctx context.Context, api *apiClient, chatID int64, messageID, page int) {
	session := r.loadSession(chatID)
	if session == nil {
		r.reply(ctx, api, chatID, "搜索上下文已过期，请重新 /search。", nil)
		return
	}
	total := len(session.results)
	pages := (total + searchPageSize - 1) / searchPageSize
	if page < 0 {
		page = 0
	}
	if page >= pages {
		page = pages - 1
	}
	start := page * searchPageSize
	end := start + searchPageSize
	if end > total {
		end = total
	}
	var builder strings.Builder
	fmt.Fprintf(&builder, "“%s” 共 %d 条（第 %d/%d 页）\n", session.keyword, total, page+1, pages)
	keyboard := &InlineKeyboard{}
	for index, result := range session.results[start:end] {
		absolute := start + index
		fmt.Fprintf(&builder, "\n%d. %s\n   %s · %s\n", absolute+1, result.Title, result.Site, formatSize(result.Size))
		row := []InlineButton{{Text: fmt.Sprintf("详情 %d", absolute+1), CallbackData: fmt.Sprintf("d:%d", absolute)}}
		if result.DetailURL != "" {
			row = append(row, InlineButton{Text: "打开", URL: result.DetailURL})
		}
		keyboard.InlineKeyboard = append(keyboard.InlineKeyboard, row)
	}
	var nav []InlineButton
	if page > 0 {
		nav = append(nav, InlineButton{Text: "上一页", CallbackData: fmt.Sprintf("p:%d", page-1)})
	}
	if page+1 < pages {
		nav = append(nav, InlineButton{Text: "下一页", CallbackData: fmt.Sprintf("p:%d", page+1)})
	}
	if len(nav) > 0 {
		keyboard.InlineKeyboard = append(keyboard.InlineKeyboard, nav)
	}
	keyboard.InlineKeyboard = append(keyboard.InlineKeyboard, []InlineButton{{Text: "收起", CallbackData: "x"}})

	if messageID > 0 {
		if err := api.editMessageText(ctx, chatID, messageID, builder.String(), keyboard); err == nil {
			return
		}
	}
	r.reply(ctx, api, chatID, builder.String(), keyboard)
}

func (r *Router) handleList(ctx context.Context, api *apiClient, chatID int64, label string, load func(context.Context) ([]string, error)) {
	if r.services == nil {
		r.reply(ctx, api, chatID, label+"查询未就绪。", nil)
		return
	}
	lines, err := load(ctx)
	if err != nil {
		r.logger.Warn("telegram 列表查询失败", "label", label, "error", err)
		r.reply(ctx, api, chatID, label+"查询失败，请稍后再试。", nil)
		return
	}
	if len(lines) == 0 {
		r.reply(ctx, api, chatID, "暂无"+label+"。", nil)
		return
	}
	if len(lines) > 30 {
		lines = append(lines[:30], fmt.Sprintf("…其余 %d 条请在 Web 端查看", len(lines)-30))
	}
	r.reply(ctx, api, chatID, label+"（"+strconv.Itoa(len(lines))+"）\n\n"+strings.Join(lines, "\n"), nil)
}

func (r *Router) subscriptions(ctx context.Context) ([]string, error) {
	return r.services.Subscriptions(ctx)
}

func (r *Router) sites(ctx context.Context) ([]string, error) {
	return r.services.Sites(ctx)
}

func (r *Router) handleCallback(ctx context.Context, api *apiClient, query *CallbackQuery) {
	if query == nil {
		return
	}
	// 先应答，避免客户端一直转圈。
	if err := api.answerCallback(ctx, query.ID, ""); err != nil {
		r.logger.Debug("telegram answerCallbackQuery 失败", "error", err)
	}
	var chatID int64
	var messageID int
	if query.Message != nil {
		chatID = query.Message.Chat.ID
		messageID = query.Message.MessageID
	}
	data := strings.TrimSpace(query.Data)
	switch {
	case data == "x":
		if chatID != 0 && messageID != 0 {
			if err := api.deleteMessage(ctx, chatID, messageID); err != nil {
				r.logger.Debug("telegram deleteMessage 失败", "error", err)
			}
		}
	case strings.HasPrefix(data, "p:"):
		page, err := strconv.Atoi(strings.TrimPrefix(data, "p:"))
		if err != nil || chatID == 0 {
			return
		}
		r.renderSearchPage(ctx, api, chatID, messageID, page)
	case strings.HasPrefix(data, "d:"):
		index, err := strconv.Atoi(strings.TrimPrefix(data, "d:"))
		if err != nil || chatID == 0 {
			return
		}
		r.renderDetail(ctx, api, chatID, messageID, index)
	case strings.HasPrefix(data, "b:"):
		page, err := strconv.Atoi(strings.TrimPrefix(data, "b:"))
		if err != nil || chatID == 0 {
			return
		}
		r.renderSearchPage(ctx, api, chatID, 0, page)
	default:
		if chatID != 0 {
			r.reply(ctx, api, chatID, "按钮已失效，请重新 /search。", nil)
		}
	}
}

func (r *Router) renderDetail(ctx context.Context, api *apiClient, chatID int64, messageID, index int) {
	session := r.loadSession(chatID)
	if session == nil || index < 0 || index >= len(session.results) {
		r.reply(ctx, api, chatID, "该结果已过期，请重新 /search。", nil)
		return
	}
	result := session.results[index]
	var builder strings.Builder
	fmt.Fprintf(&builder, "%s\n\n站点：%s\n大小：%s\n", result.Title, result.Site, formatSize(result.Size))
	if result.Seeders > 0 {
		fmt.Fprintf(&builder, "做种：%d\n", result.Seeders)
	}
	if result.DetailURL != "" {
		fmt.Fprintf(&builder, "详情页：%s\n", result.DetailURL)
	}
	builder.WriteString("\n下载请回到 Web 端的搜索结果页操作。")
	keyboard := &InlineKeyboard{InlineKeyboard: [][]InlineButton{{
		{Text: "返回列表", CallbackData: fmt.Sprintf("b:%d", index/searchPageSize)},
		{Text: "收起", CallbackData: "x"},
	}}}
	if messageID > 0 {
		if err := api.editMessageText(ctx, chatID, messageID, builder.String(), keyboard); err == nil {
			return
		}
	}
	r.reply(ctx, api, chatID, builder.String(), keyboard)
}

func (r *Router) reply(ctx context.Context, api *apiClient, chatID int64, text string, keyboard *InlineKeyboard) {
	if _, err := api.sendMessage(ctx, chatID, text, keyboard); err != nil {
		r.logger.Warn("telegram 回复失败", "error", err, "chat_id", chatID)
	}
}

func (r *Router) storeSession(chatID int64, keyword string, results []SearchResult) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	for id, session := range r.sessions {
		if now.Sub(session.touched) > searchSessionTTL {
			delete(r.sessions, id)
		}
	}
	if len(r.sessions) >= searchSessionLimit {
		for id := range r.sessions {
			delete(r.sessions, id)
			break
		}
	}
	r.sessions[chatID] = &searchSession{keyword: keyword, results: results, touched: now}
}

func (r *Router) loadSession(chatID int64) *searchSession {
	r.mu.Lock()
	defer r.mu.Unlock()
	session, ok := r.sessions[chatID]
	if !ok {
		return nil
	}
	if r.now().Sub(session.touched) > searchSessionTTL {
		delete(r.sessions, chatID)
		return nil
	}
	session.touched = r.now()
	return session
}

func splitCommand(text string) (string, string) {
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return "", ""
	}
	command := fields[0]
	if index := strings.IndexByte(command, '@'); index >= 0 {
		command = command[:index]
	}
	return strings.ToLower(command), strings.TrimSpace(strings.Join(fields[1:], " "))
}

func formatSize(size int64) string {
	if size <= 0 {
		return "未知大小"
	}
	const unit = 1024
	value := float64(size)
	for _, suffix := range []string{"B", "KB", "MB", "GB", "TB"} {
		if value < unit {
			return fmt.Sprintf("%.1f %s", value, suffix)
		}
		value /= unit
	}
	return fmt.Sprintf("%.1f PB", value)
}

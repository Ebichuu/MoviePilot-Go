package notification

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	telegramAPIResponseLimit = 64 << 10
	telegramTextLimit        = 4096
	telegramCaptionLimit     = 1024
	telegramImageLimit       = 10 << 20
	telegramSendAttempts     = 3
	telegramRetryDelay       = 3 * time.Second
)

// TelegramSender delivers notifications via Telegram Bot API.
type TelegramSender struct {
	logger    *slog.Logger
	botToken  string
	chatID    string
	apiURL    string
	client    *http.Client
	parseMode string
	retryWait func(context.Context, time.Duration) error
}

// TelegramConfig holds Telegram Bot configuration.
type TelegramConfig struct {
	BotToken  string
	ChatID    string
	APIUrl    string // Optional custom API URL, defaults to https://api.telegram.org
	ParseMode string // "MarkdownV2" or "HTML", defaults to MarkdownV2
}

// NewTelegramSender creates a Telegram notification channel.
func NewTelegramSender(logger *slog.Logger, cfg TelegramConfig) (*TelegramSender, error) {
	if cfg.BotToken == "" {
		return nil, fmt.Errorf("telegram bot token is required")
	}
	if cfg.ChatID == "" {
		return nil, fmt.Errorf("telegram chat ID is required")
	}

	apiURL := cfg.APIUrl
	if apiURL == "" {
		apiURL = "https://api.telegram.org"
	}

	parseMode := normalizeTelegramParseMode(cfg.ParseMode, "MarkdownV2")

	return &TelegramSender{
		logger:    nonNilLogger(logger),
		botToken:  cfg.BotToken,
		chatID:    cfg.ChatID,
		apiURL:    strings.TrimSuffix(apiURL, "/"),
		client:    &http.Client{Timeout: 15 * time.Second},
		parseMode: parseMode,
		retryWait: waitTelegramRetry,
	}, nil
}

func (t *TelegramSender) Name() Channel {
	return ChannelTelegram
}

func (t *TelegramSender) Send(ctx context.Context, msg Message) error {
	if ctx == nil || t == nil || t.client == nil {
		return fmt.Errorf("telegram sender unavailable")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	parseMode := normalizeTelegramParseMode(msg.ParseMode, t.parseMode)
	text := t.formatMessageWithMode(msg, parseMode)
	if text == "" {
		return fmt.Errorf("empty telegram message")
	}
	chatID := t.chatID
	if msg.UserID != "" {
		chatID = msg.UserID
	}

	image := t.downloadImage(ctx, msg.Image)
	limit := telegramTextLimit
	if len(image) > 0 {
		limit = telegramCaptionLimit
	}
	chunks := splitTelegramMessage(text, limit, parseMode)
	if len(chunks) == 0 {
		return fmt.Errorf("empty telegram message")
	}
	sent := make([]bool, len(chunks))
	err := t.retry(ctx, func() error {
		for index, chunk := range chunks {
			if parseMode == "MarkdownV2" && sent[index] {
				continue
			}
			var sendErr error
			if index == 0 && len(image) > 0 {
				sendErr = t.sendPhoto(ctx, chatID, chunk, parseMode, image)
			} else {
				sendErr = t.sendText(ctx, chatID, chunk, parseMode, msg.DisableWebPagePreview)
			}
			if sendErr != nil {
				return sendErr
			}
			if parseMode == "MarkdownV2" {
				sent[index] = true
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	t.logger.InfoContext(ctx, "telegram notification sent",
		slog.String("type", string(msg.Type)),
		slog.String("title", msg.Title))

	return nil
}

func (t *TelegramSender) downloadImage(ctx context.Context, imageURL string) []byte {
	if strings.TrimSpace(imageURL) == "" {
		return nil
	}
	parsed, err := url.Parse(imageURL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return nil
	}
	resp, err := t.client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices ||
		resp.ContentLength > telegramImageLimit {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return nil
	}
	image, err := io.ReadAll(io.LimitReader(resp.Body, telegramImageLimit+1))
	if err != nil || len(image) == 0 || len(image) > telegramImageLimit {
		return nil
	}
	return image
}

func (t *TelegramSender) sendText(
	ctx context.Context,
	chatID, text, parseMode string,
	disablePreview *bool,
) error {
	payload := map[string]any{
		"chat_id":    chatID,
		"text":       text,
		"parse_mode": parseMode,
	}
	if disablePreview != nil {
		payload["disable_web_page_preview"] = *disablePreview
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal telegram payload failed")
	}
	return t.sendAPIRequest(ctx, "sendMessage", "application/json", bytes.NewReader(body))
}

func (t *TelegramSender) retry(ctx context.Context, send func() error) error {
	delay := telegramRetryDelay
	wait := t.retryWait
	if wait == nil {
		wait = waitTelegramRetry
	}
	var sendErr error
	for attempt := 0; attempt < telegramSendAttempts; attempt++ {
		if sendErr = send(); sendErr == nil {
			return nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if attempt+1 < telegramSendAttempts {
			if err := wait(ctx, delay); err != nil {
				return err
			}
			delay *= 2
		}
	}
	return sendErr
}

func waitTelegramRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (t *TelegramSender) sendPhoto(
	ctx context.Context,
	chatID, caption, parseMode string,
	image []byte,
) error {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	fields := [][2]string{{"chat_id", chatID}, {"caption", caption}, {"parse_mode", parseMode}}
	for _, field := range fields {
		if err := writer.WriteField(field[0], field[1]); err != nil {
			return fmt.Errorf("build telegram photo request failed")
		}
	}
	part, err := writer.CreateFormFile("photo", "image")
	if err != nil {
		return fmt.Errorf("build telegram photo request failed")
	}
	if _, err := part.Write(image); err != nil {
		return fmt.Errorf("build telegram photo request failed")
	}
	if err := writer.Close(); err != nil {
		return fmt.Errorf("build telegram photo request failed")
	}
	return t.sendAPIRequest(ctx, "sendPhoto", writer.FormDataContentType(), bytes.NewReader(body.Bytes()))
}

func (t *TelegramSender) sendAPIRequest(
	ctx context.Context,
	method, contentType string,
	body io.Reader,
) error {
	endpoint, err := telegramAPIEndpoint(t.apiURL, t.botToken, method)
	if err != nil {
		return fmt.Errorf("telegram api endpoint invalid")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, body)
	if err != nil {
		return fmt.Errorf("create telegram request failed")
	}
	req.Header.Set("Content-Type", contentType)
	resp, err := t.client.Do(req)
	if err != nil {
		// The request URL contains the bot token by Telegram API design.
		// Never return the raw client error because it can leak that URL.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return fmt.Errorf("telegram api unavailable")
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("telegram api error: status %d", resp.StatusCode)
	}
	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, telegramAPIResponseLimit+1))
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return fmt.Errorf("telegram api response invalid")
	}
	if len(responseBody) > telegramAPIResponseLimit {
		return fmt.Errorf("telegram api response invalid")
	}
	var result struct {
		OK *bool `json:"ok"`
	}
	if err := json.Unmarshal(responseBody, &result); err != nil || result.OK == nil {
		return fmt.Errorf("telegram api response invalid")
	}
	if !*result.OK {
		return fmt.Errorf("telegram api rejected request")
	}
	return nil
}

func telegramAPIEndpoint(apiURL, token, method string) (string, error) {
	base, err := url.Parse(apiURL)
	if err != nil || base.Scheme == "" || base.Host == "" {
		return "", fmt.Errorf("invalid telegram api URL")
	}
	return base.ResolveReference(&url.URL{Path: "/bot" + token + "/" + method}).String(), nil
}

func normalizeTelegramParseMode(value, fallback string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		value = strings.ToLower(strings.TrimSpace(fallback))
	}
	switch value {
	case "html":
		return "HTML"
	case "markdownv2", "mdv2":
		return "MarkdownV2"
	default:
		return "MarkdownV2"
	}
}

type telegramEntity struct {
	key   string
	open  string
	close string
}

type telegramSplitToken struct {
	raw     string
	width   int
	entity  telegramEntity
	action  int
	newline bool
}

func splitTelegramMessage(text string, limit int, parseMode string) []string {
	if text == "" {
		return nil
	}
	if limit <= 0 {
		return []string{text}
	}
	var tokens []telegramSplitToken
	if parseMode == "HTML" {
		tokens = tokenizeTelegramHTML(text)
	} else {
		tokens = tokenizeTelegramMarkdown(text)
	}
	return splitTelegramTokens(text, tokens, limit)
}

func splitTelegramTokens(text string, tokens []telegramSplitToken, limit int) []string {
	totalWidth := 0
	for _, token := range tokens {
		totalWidth += token.width
	}
	if totalWidth <= limit {
		return []string{text}
	}

	chunks := make([]string, 0, (totalWidth+limit-1)/limit)
	active := make([]telegramEntity, 0, 4)
	for start := 0; start < len(tokens); {
		width, end, lastNewline := 0, start, -1
		for end < len(tokens) {
			token := tokens[end]
			if width >= limit && token.width == 0 && token.action == 1 {
				break
			}
			if width > 0 && token.width > 0 && width+token.width > limit {
				break
			}
			width += token.width
			if token.newline {
				lastNewline = end
			}
			end++
			if width > limit {
				break
			}
		}
		if end == start {
			end++
		}
		chosen := end
		if end < len(tokens) && lastNewline > start {
			chosen = lastNewline
		}

		endState := appendTelegramTokenState(append([]telegramEntity(nil), active...), tokens[start:chosen])
		var chunk strings.Builder
		for _, entity := range active {
			chunk.WriteString(entity.open)
		}
		for _, token := range tokens[start:chosen] {
			chunk.WriteString(token.raw)
		}
		for index := len(endState) - 1; index >= 0; index-- {
			chunk.WriteString(endState[index].close)
		}
		if chunk.Len() > 0 {
			chunks = append(chunks, chunk.String())
		}
		active = endState
		start = chosen
		for start < len(tokens) && tokens[start].newline {
			start++
		}
	}
	return chunks
}

func appendTelegramTokenState(active []telegramEntity, tokens []telegramSplitToken) []telegramEntity {
	for _, token := range tokens {
		switch token.action {
		case 1:
			active = append(active, token.entity)
		case -1:
			if len(active) > 0 && active[len(active)-1].key == token.entity.key {
				active = active[:len(active)-1]
			}
		}
	}
	return active
}

func tokenizeTelegramMarkdown(text string) []telegramSplitToken {
	tokens := make([]telegramSplitToken, 0, utf8.RuneCountInString(text))
	active := make([]telegramEntity, 0, 4)
	for index := 0; index < len(text); {
		if text[index] == '\\' && index+1 < len(text) {
			_, size := utf8.DecodeRuneInString(text[index+1:])
			tokens = append(tokens, telegramSplitToken{raw: text[index : index+1+size], width: 1})
			index += 1 + size
			continue
		}
		if linkTokens, end, ok := telegramMarkdownLinkAt(text, index); ok {
			tokens = append(tokens, linkTokens...)
			index = end
			continue
		}
		if marker := telegramMarkdownMarkerAt(text, index, active); marker != "" {
			entity := telegramEntity{key: "md:" + marker, open: marker, close: marker}
			action := 1
			if len(active) > 0 && active[len(active)-1].key == entity.key {
				action = -1
			}
			token := telegramSplitToken{raw: marker, entity: entity, action: action}
			tokens = append(tokens, token)
			active = appendTelegramTokenState(active, []telegramSplitToken{token})
			index += len(marker)
			continue
		}
		r, size := utf8.DecodeRuneInString(text[index:])
		tokens = append(tokens, telegramSplitToken{raw: text[index : index+size], width: 1, newline: r == '\n'})
		index += size
	}
	return tokens
}

func telegramMarkdownMarkerAt(text string, index int, active []telegramEntity) string {
	if len(active) > 0 && strings.HasPrefix(active[len(active)-1].key, "md:`") {
		marker := strings.TrimPrefix(active[len(active)-1].key, "md:")
		if strings.HasPrefix(text[index:], marker) {
			return marker
		}
		return ""
	}
	for _, marker := range []string{"```", "||", "__", "*", "_", "~", "`"} {
		if strings.HasPrefix(text[index:], marker) {
			return marker
		}
	}
	return ""
}

func telegramMarkdownLinkAt(text string, index int) ([]telegramSplitToken, int, bool) {
	if index >= len(text) || text[index] != '[' {
		return nil, index, false
	}
	labelEnd := findUnescaped(text, "](", index+1)
	if labelEnd < 0 {
		return nil, index, false
	}
	linkEnd := findUnescaped(text, ")", labelEnd+2)
	if linkEnd < 0 {
		return nil, index, false
	}
	closeMarker := text[labelEnd : linkEnd+1]
	entity := telegramEntity{
		key:   "md:link:" + closeMarker,
		open:  "[",
		close: closeMarker,
	}
	tokens := []telegramSplitToken{{raw: "[", entity: entity, action: 1}}
	tokens = append(tokens, tokenizeTelegramMarkdown(text[index+1:labelEnd])...)
	tokens = append(tokens, telegramSplitToken{raw: closeMarker, entity: entity, action: -1})
	return tokens, linkEnd + 1, true
}

func tokenizeTelegramHTML(text string) []telegramSplitToken {
	tokens := make([]telegramSplitToken, 0, utf8.RuneCountInString(text))
	for index := 0; index < len(text); {
		if text[index] == '<' {
			if offset := strings.IndexByte(text[index:], '>'); offset >= 0 {
				raw := text[index : index+offset+1]
				token := telegramSplitToken{raw: raw}
				if entity, action, ok := telegramHTMLEntity(raw); ok {
					token.entity, token.action = entity, action
				}
				tokens = append(tokens, token)
				index += len(raw)
				continue
			}
		}
		if text[index] == '&' {
			if offset := strings.IndexByte(text[index:], ';'); offset > 0 {
				raw := text[index : index+offset+1]
				tokens = append(tokens, telegramSplitToken{raw: raw, width: 1})
				index += len(raw)
				continue
			}
		}
		r, size := utf8.DecodeRuneInString(text[index:])
		tokens = append(tokens, telegramSplitToken{raw: text[index : index+size], width: 1, newline: r == '\n'})
		index += size
	}
	return tokens
}

func telegramHTMLEntity(raw string) (telegramEntity, int, bool) {
	value := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(raw, "<"), ">"))
	if value == "" || strings.HasPrefix(value, "!") || strings.HasPrefix(value, "?") || strings.HasSuffix(value, "/") {
		return telegramEntity{}, 0, false
	}
	action := 1
	if strings.HasPrefix(value, "/") {
		action = -1
		value = strings.TrimSpace(strings.TrimPrefix(value, "/"))
	}
	if field := strings.Fields(value); len(field) > 0 {
		value = field[0]
	}
	name := strings.ToLower(strings.TrimSpace(value))
	if name == "" || name == "br" {
		return telegramEntity{}, 0, false
	}
	entity := telegramEntity{key: "html:" + name, open: raw, close: "</" + name + ">"}
	return entity, action, true
}

func findUnescaped(text, needle string, start int) int {
	for index := start; index+len(needle) <= len(text); index++ {
		if strings.HasPrefix(text[index:], needle) && !isEscapedAt(text, index) {
			return index
		}
	}
	return -1
}

func isEscapedAt(text string, index int) bool {
	backslashes := 0
	for index--; index >= 0 && text[index] == '\\'; index-- {
		backslashes++
	}
	return backslashes%2 != 0
}

func nonNilLogger(logger *slog.Logger) *slog.Logger {
	if logger == nil {
		return slog.Default()
	}
	return logger
}

func (t *TelegramSender) formatMessage(msg Message) string {
	return t.formatMessageWithMode(msg, normalizeTelegramParseMode("", t.parseMode))
}

func (t *TelegramSender) formatMessageWithMode(msg Message, parseMode string) string {
	parts := make([]string, 0, 3)
	if msg.Title != "" {
		title := strings.TrimSuffix(msg.Title, "\n")
		if parseMode == "MarkdownV2" {
			parts = append(parts, "*"+t.escapeMarkdownV2(title)+"*")
		} else {
			parts = append(parts, "<b>"+t.escapeHTML(title)+"</b>")
		}
	}
	if msg.Text != "" {
		if parseMode == "MarkdownV2" {
			parts = append(parts, standardizeTelegramMarkdown(msg.Text))
		} else {
			parts = append(parts, msg.Text)
		}
	}
	if msg.Link != "" {
		if parseMode == "MarkdownV2" {
			parts = append(parts, "[查看详情]("+t.escapeMarkdownV2Link(msg.Link)+")")
		} else {
			parts = append(parts, "<a href=\""+t.escapeHTML(msg.Link)+"\">查看详情</a>")
		}
	}
	return strings.Join(parts, "\n")
}

func (t *TelegramSender) escapeMarkdownV2(text string) string {
	var escaped strings.Builder
	for _, r := range text {
		if strings.ContainsRune("\\_*[]()~`>#+-=|{}.!", r) {
			escaped.WriteByte('\\')
		}
		escaped.WriteRune(r)
	}
	return escaped.String()
}

func (t *TelegramSender) escapeMarkdownV2Link(link string) string {
	// For URLs in MarkdownV2, only escape ) and \.
	replacer := strings.NewReplacer(
		"\\", "\\\\",
		")", "\\)",
	)
	return replacer.Replace(link)
}

func (t *TelegramSender) escapeHTML(text string) string {
	replacer := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		"\"", "&quot;",
		"'", "&#x27;",
	)
	return replacer.Replace(text)
}

func standardizeTelegramMarkdown(text string) string {
	var result strings.Builder
	for index := 0; index < len(text); {
		if text[index] == '\\' {
			if index+1 >= len(text) {
				result.WriteString("\\\\")
				break
			}
			next, size := utf8.DecodeRuneInString(text[index+1:])
			if strings.ContainsRune(`!"#$%&'()*+,-./:;<=>?@[\]^_`+"`"+`{|}~`, next) {
				result.WriteString((&TelegramSender{}).escapeMarkdownV2(string(next)))
				index += 1 + size
				continue
			}
			result.WriteString("\\\\")
			index++
			continue
		}
		if raw, end, ok := standardizeMarkdownLinkAt(text, index); ok {
			result.WriteString(raw)
			index = end
			continue
		}
		matched := false
		for _, entity := range []struct {
			source string
			open   string
			close  string
		}{
			{source: "```", open: "```", close: "```"},
			{source: "**", open: "*", close: "*"},
			{source: "||", open: "||", close: "||"},
			{source: "~~", open: "~", close: "~"},
			{source: "*", open: "_", close: "_"},
			{source: "_", open: "_", close: "_"},
			{source: "`", open: "`", close: "`"},
		} {
			if !strings.HasPrefix(text[index:], entity.source) {
				continue
			}
			end := findUnescaped(text, entity.source, index+len(entity.source))
			if end <= index+len(entity.source) {
				continue
			}
			inner := text[index+len(entity.source) : end]
			result.WriteString(entity.open)
			if strings.HasPrefix(entity.source, "`") {
				result.WriteString(escapeTelegramCode(inner))
			} else {
				result.WriteString(standardizeTelegramMarkdown(inner))
			}
			result.WriteString(entity.close)
			index = end + len(entity.source)
			matched = true
			break
		}
		if matched {
			continue
		}
		if text[index] == '>' && (index == 0 || text[index-1] == '\n') {
			result.WriteByte('>')
			index++
			continue
		}
		r, size := utf8.DecodeRuneInString(text[index:])
		result.WriteString((&TelegramSender{}).escapeMarkdownV2(string(r)))
		index += size
	}
	return result.String()
}

func standardizeMarkdownLinkAt(text string, index int) (string, int, bool) {
	if index >= len(text) || text[index] != '[' {
		return "", index, false
	}
	labelEnd := findUnescaped(text, "](", index+1)
	if labelEnd < 0 {
		return "", index, false
	}
	depth := 1
	for end := labelEnd + 2; end < len(text); end++ {
		if isEscapedAt(text, end) {
			continue
		}
		switch text[end] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				label := standardizeTelegramMarkdown(text[index+1 : labelEnd])
				link := (&TelegramSender{}).escapeMarkdownV2Link(text[labelEnd+2 : end])
				return "[" + label + "](" + link + ")", end + 1, true
			}
		}
	}
	return "", index, false
}

func escapeTelegramCode(text string) string {
	return strings.NewReplacer("\\", "\\\\", "`", "\\`").Replace(text)
}

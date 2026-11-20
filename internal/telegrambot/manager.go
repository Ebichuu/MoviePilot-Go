package telegrambot

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	pollErrorBackoff = 5 * time.Second
	minPollInterval  = time.Second
	reconcileEvery   = 60 * time.Second
)

// Config is one enabled Telegram channel as far as the bot cares.
type Config struct {
	Name    string
	Token   string
	ChatID  string
	APIURL  string
	Enabled bool
}

// Poller long-polls one bot token and dispatches updates through the router.
type Poller struct {
	api      *apiClient
	router   *Router
	logger   *slog.Logger
	allowed  map[int64]struct{}
	allowAny bool
	pollTime time.Duration
}

func newPoller(cfg Config, router *Router, logger *slog.Logger) *Poller {
	allowed, allowAny := parseAllowedChats(cfg.ChatID)
	return &Poller{
		api:      newAPIClient(cfg.APIURL, cfg.Token, &http.Client{Timeout: defaultPollTime + 30*time.Second}),
		router:   router,
		logger:   logger,
		allowed:  allowed,
		allowAny: allowAny,
		pollTime: defaultPollTime,
	}
}

// parseAllowedChats accepts the same chat_id spellings the sender accepts:
// a single id, or several ids separated by commas/spaces.
func parseAllowedChats(raw string) (map[int64]struct{}, bool) {
	allowed := map[int64]struct{}{}
	for _, field := range strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ';' || r == ' ' || r == '\t' || r == '\n'
	}) {
		id, err := strconv.ParseInt(strings.TrimSpace(field), 10, 64)
		if err != nil || id == 0 {
			continue
		}
		allowed[id] = struct{}{}
	}
	return allowed, len(allowed) == 0
}

func (p *Poller) chatAllowed(chatID int64) bool {
	if p.allowAny {
		return true
	}
	_, ok := p.allowed[chatID]
	return ok
}

// Run polls until the context is cancelled.
func (p *Poller) Run(ctx context.Context) {
	if p == nil || p.api == nil || p.router == nil || ctx == nil {
		return
	}
	var offset int64
	for {
		if err := ctx.Err(); err != nil {
			return
		}
		started := time.Now()
		updates, err := p.api.getUpdates(ctx, offset, p.pollTime)
		if err != nil {
			if errors.Is(ctx.Err(), context.Canceled) {
				return
			}
			p.logger.Warn("telegram getUpdates 失败", "error", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(pollErrorBackoff):
			}
			continue
		}
		for _, update := range updates {
			if update.UpdateID >= offset {
				offset = update.UpdateID + 1
			}
			p.dispatch(ctx, update)
		}
		// 正常情况下 Bot API 会按 timeout 挂起连接；若对端立刻返回空结果
		// （代理或测试桩），这里补一个最小间隔，避免空转打满 CPU 和请求配额。
		if len(updates) == 0 && time.Since(started) < minPollInterval {
			select {
			case <-ctx.Done():
				return
			case <-time.After(minPollInterval):
			}
		}
	}
}

func (p *Poller) dispatch(ctx context.Context, update Update) {
	chatID := updateChatID(update)
	if chatID == 0 {
		return
	}
	if !p.chatAllowed(chatID) {
		p.logger.Info("telegram 忽略未授权会话", "chat_id", chatID)
		return
	}
	p.router.HandleUpdate(ctx, p.api, update)
}

func updateChatID(update Update) int64 {
	if update.Message != nil {
		return update.Message.Chat.ID
	}
	if update.CallbackQuery != nil && update.CallbackQuery.Message != nil {
		return update.CallbackQuery.Message.Chat.ID
	}
	return 0
}

type runningPoller struct {
	config Config
	cancel context.CancelFunc
	done   chan struct{}
}

// Manager keeps exactly one poller per enabled bot token. Configuration is
// re-read on an interval, so adding or rotating a bot needs no restart.
type Manager struct {
	logger  *slog.Logger
	router  *Router
	client  *http.Client
	now     func() time.Time
	running map[string]*runningPoller

	mu      sync.Mutex
	stopped bool
}

// NewManager builds a bot manager. A nil router disables polling.
func NewManager(logger *slog.Logger, router *Router) *Manager {
	if logger == nil {
		logger = slog.Default()
	}
	return &Manager{
		logger:  logger,
		router:  router,
		now:     time.Now,
		running: map[string]*runningPoller{},
	}
}

// Run reconciles pollers with source() until the context is cancelled.
func (m *Manager) Run(ctx context.Context, source func(context.Context) []Config) {
	if m == nil || ctx == nil {
		return
	}
	ticker := time.NewTicker(reconcileEvery)
	defer ticker.Stop()
	for {
		m.sync(ctx, source)
		select {
		case <-ctx.Done():
			m.Stop()
			return
		case <-ticker.C:
		}
	}
}

func (m *Manager) sync(ctx context.Context, source func(context.Context) []Config) {
	if m == nil || m.router == nil {
		return
	}
	var configs []Config
	if source != nil {
		configs = source(ctx)
	}
	m.mu.Lock()
	if m.stopped {
		m.mu.Unlock()
		return
	}
	desired := make(map[string]Config, len(configs))
	for _, cfg := range configs {
		token := strings.TrimSpace(cfg.Token)
		if token == "" || !cfg.Enabled {
			continue
		}
		key := pollerKey(cfg)
		desired[key] = cfg
	}
	for key, handle := range m.running {
		if _, ok := desired[key]; ok {
			continue
		}
		handle.cancel()
		delete(m.running, key)
		m.logger.Info("telegram 轮询停止", "channel", handle.config.Name)
	}
	for key, cfg := range desired {
		if _, ok := m.running[key]; ok {
			continue
		}
		pollCtx, cancel := context.WithCancel(ctx)
		poller := newPoller(cfg, m.router, m.logger)
		handle := &runningPoller{config: cfg, cancel: cancel, done: make(chan struct{})}
		m.running[key] = handle
		go func(key string, poller *Poller, handle *runningPoller) {
			defer close(handle.done)
			defer func() {
				if recovered := recover(); recovered != nil {
					m.logger.Error("telegram 轮询协程异常退出", "panic", recovered)
				}
			}()
			poller.Run(pollCtx)
		}(key, poller, handle)
		m.logger.Info("telegram 轮询启动", "channel", cfg.Name)
	}
	m.mu.Unlock()
}

// Stop cancels every poller and waits briefly for them to finish.
func (m *Manager) Stop() {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.stopped = true
	handles := make([]*runningPoller, 0, len(m.running))
	for key, handle := range m.running {
		handle.cancel()
		handles = append(handles, handle)
		delete(m.running, key)
	}
	m.mu.Unlock()
	for _, handle := range handles {
		select {
		case <-handle.done:
		case <-time.After(2 * time.Second):
		}
	}
}

func pollerKey(cfg Config) string {
	parts := []string{strings.TrimSpace(cfg.Token), strings.TrimSpace(cfg.APIURL), strings.TrimSpace(cfg.ChatID)}
	return strings.Join(parts, "\x00")
}

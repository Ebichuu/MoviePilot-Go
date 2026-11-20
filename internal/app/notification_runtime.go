package app

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"strings"
	"sync"

	"github.com/moviepilot-go/moviepilot-go/internal/config"
	"github.com/moviepilot-go/moviepilot-go/internal/notification"
	"github.com/moviepilot-go/moviepilot-go/internal/notificationconfig"
)

// notificationRuntimeStore is deliberately smaller than database.Store so the
// reload path can be exercised without a PostgreSQL connection.
type notificationRuntimeStore interface {
	LoadNotificationSettings(context.Context) (json.RawMessage, bool, error)
}

// notificationRuntime owns the process-long Telegram registry. The registry
// itself publishes immutable states atomically; reloadMu additionally
// serializes database reads and sender construction so concurrent HTTP saves
// cannot finish out of order.
type notificationRuntime struct {
	reloadMu sync.Mutex
	store    notificationRuntimeStore
	registry *notification.Registry
	logger   *slog.Logger
	// snapshot 是最近一次成功发布的配置副本，供入站机器人读取 Token。
	snapshot notificationconfig.Snapshot
}

// Telegrams 返回最近一次成功发布的 Telegram 通道配置。
// 入站轮询按此动态启停，配置变更无需重启进程。
func (r *notificationRuntime) Telegrams() []notificationconfig.Telegram {
	if r == nil {
		return nil
	}
	r.reloadMu.Lock()
	defer r.reloadMu.Unlock()
	return append([]notificationconfig.Telegram(nil), r.snapshot.Telegrams...)
}

// Registry returns the process-long atomic sender registry shared by planners,
// workers, and HTTP-triggered reloads.
func (r *notificationRuntime) Registry() *notification.Registry {
	if r == nil {
		return nil
	}
	return r.registry
}

// loadNotificationRuntime loads the initial snapshot and always returns a
// non-nil Registry when successful. Existing database settings take precedence
// over the legacy environment-variable fallback, including null and empty
// lists that intentionally disable Telegram delivery.
func loadNotificationRuntime(
	ctx context.Context,
	store notificationRuntimeStore,
	logger *slog.Logger,
) (*notificationRuntime, error) {
	if ctx == nil {
		return nil, errors.New("通知运行时上下文不可用")
	}
	if store == nil {
		return nil, errors.New("通知设置存储不可用")
	}
	if logger == nil {
		logger = slog.Default()
	}
	runtime := &notificationRuntime{
		store:    store,
		registry: notification.NewRegistry(logger),
		logger:   logger,
	}
	if err := runtime.Reload(ctx); err != nil {
		return nil, err
	}
	return runtime, nil
}

// Reload rebuilds a complete sender set before publishing it. Any load,
// decode, or sender-construction failure leaves the previously published
// registry state untouched (Registry.Replace provides that atomic guarantee).
func (r *notificationRuntime) Reload(ctx context.Context) error {
	if r == nil || r.store == nil || r.registry == nil {
		return errors.New("通知运行时不可用")
	}
	if ctx == nil {
		return errors.New("通知运行时上下文不可用")
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	r.reloadMu.Lock()
	defer r.reloadMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}

	snapshot, err := r.loadSnapshot(ctx)
	if err != nil {
		return err
	}
	if err := r.registry.Replace(snapshot); err != nil {
		// Do not return the underlying factory error: it may contain a token or
		// endpoint supplied by a user. The old state remains published.
		return errors.New("重建通知运行态失败")
	}
	r.snapshot = snapshot
	return nil
}

func (r *notificationRuntime) loadSnapshot(ctx context.Context) (notificationconfig.Snapshot, error) {
	raw, found, err := r.store.LoadNotificationSettings(ctx)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return notificationconfig.Snapshot{}, ctxErr
		}
		return notificationconfig.Snapshot{}, errors.New("加载通知设置失败")
	}
	if !found {
		return legacyNotificationSnapshot(), nil
	}

	snapshot, err := notificationconfig.Decode(raw)
	if err != nil {
		// A present setting is authoritative. In particular, never fall back to
		// environment variables when the value is null, empty, or malformed.
		return notificationconfig.Snapshot{}, errors.New("通知设置格式无效")
	}
	return snapshot, nil
}

// legacyNotificationSnapshot is the compatibility-only environment fallback.
// It is used only when PostgreSQL has no Notifications row. All historical
// notification categories are enabled because the old env configuration had
// no switch filter.
func legacyNotificationSnapshot() notificationconfig.Snapshot {
	token := strings.TrimSpace(os.Getenv("MOVIEPILOT_TELEGRAM_BOT_TOKEN"))
	chatID := strings.TrimSpace(os.Getenv("MOVIEPILOT_TELEGRAM_CHAT_ID"))
	if token == "" || chatID == "" {
		return notificationconfig.Snapshot{}
	}
	apiURL := strings.TrimSpace(os.Getenv("MOVIEPILOT_TELEGRAM_API_URL"))
	switches := []string{
		string(notification.TypeDownload),
		string(notification.TypeOrganize),
		string(notification.TypeSubscribe),
		string(notification.TypeSiteMessage),
		string(notification.TypeMediaServer),
		string(notification.TypeManual),
		string(notification.TypePlugin),
	}
	return notificationconfig.Snapshot{
		Telegrams: []notificationconfig.Telegram{{
			Name: "Telegram", BotToken: token, ChatID: chatID,
			APIURL: apiURL, Switchs: switches,
		}},
	}
}

// buildNotificationSender remains as a compatibility helper for callers that
// still expect the old environment-only constructor. The application uses
// loadNotificationRuntime instead, but keeping this helper avoids changing
// unrelated tests and integrations in the same package.
func buildNotificationSender(_ config.Config, logger *slog.Logger) (notification.Sender, []notification.Channel) {
	registry := notification.NewRegistry(logger)
	snapshot := legacyNotificationSnapshot()
	if len(snapshot.Telegrams) == 0 {
		return registry, nil
	}
	if err := registry.Replace(snapshot); err != nil {
		if logger == nil {
			logger = slog.Default()
		}
		logger.Warn("Telegram 通知初始化失败")
		return registry, nil
	}
	return registry, []notification.Channel{notification.ChannelTelegram}
}

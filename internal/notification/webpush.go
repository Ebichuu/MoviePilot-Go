package notification

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

const (
	webPushWNSDefaultTTL   = 24 * time.Hour
	webPushRequestTimeout  = 15 * time.Second
	webPushDefaultURL      = "/?shotcut=message"
	webPushPayloadMaxBytes = 4096
)

var (
	errWebPushUnavailable = errors.New("webpush sender unavailable")
	errWebPushConfig      = errors.New("webpush vapid configuration invalid")
)

// WebPushSubscription 是一个浏览器推送订阅，对应 pywebpush 的 subscription_info。
type WebPushSubscription struct {
	Endpoint string
	// Username 是该订阅归属的 MoviePilot 用户；V2 用配置项 WEBPUSH_USERNAME 过滤。
	Username string
	// P256dh 与 Auth 是订阅密钥的 base64url 字符串。
	P256dh string
	Auth   string
}

// WebPushSubscriptionSource 提供当前有效订阅，并支持移除失效订阅，等价 V2
// `global_vars.get_subscriptions()` / `remove_subscription()`。
type WebPushSubscriptionSource interface {
	List(ctx context.Context) ([]WebPushSubscription, error)
	Remove(ctx context.Context, endpoint string) error
}

// WebPushConfig 是 VAPID 与发送策略配置，等价 V2 `settings.VAPID` 与
// `webpush_options_for_endpoint`。
type WebPushConfig struct {
	// VAPIDPrivateKey 是 P-256 私钥标量的 base64/base64url 编码。
	VAPIDPrivateKey string
	// VAPIDPublicKey 是 P-256 公钥（非压缩点）的 base64/base64url 编码；留空时
	// 从私钥推导。
	VAPIDPublicKey string
	// VAPIDSubject 是 VAPID 声明的 `sub`（mailto: 或站点 URL）。
	VAPIDSubject string
	// Usernames 等价 WEBPUSH_USERNAME；非空时只接收这些用户的消息。
	Usernames []string
	// TTL 控制 TTL 头，0 使用默认值。
	TTL time.Duration
}

// WebPushSender 通过 RFC 8030 + RFC 8291（aes128gcm）向浏览器订阅投递通知。
type WebPushSender struct {
	logger     *slog.Logger
	config     WebPushConfig
	source     WebPushSubscriptionSource
	httpClient *http.Client
	now        func() time.Time
}

// NewWebPushSender 创建 WebPush 通知渠道。
func NewWebPushSender(
	logger *slog.Logger,
	config WebPushConfig,
	source WebPushSubscriptionSource,
) *WebPushSender {
	if logger == nil {
		logger = slog.Default()
	}
	return &WebPushSender{
		logger:     logger,
		config:     config,
		source:     source,
		httpClient: &http.Client{Timeout: webPushRequestTimeout},
		now:        time.Now,
	}
}

func (w *WebPushSender) Name() Channel {
	return ChannelWebPush
}

// Send 复刻 V2 `WebPushModule.post_message`：按用户过滤、逐订阅投递、404/410
// 时移除失效订阅；单个订阅失败不影响其余订阅。
func (w *WebPushSender) Send(ctx context.Context, msg Message) error {
	if w == nil || w.httpClient == nil || w.now == nil {
		return errWebPushUnavailable
	}
	if ctx == nil {
		return errors.New("notification context is nil")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if w.source == nil {
		return errWebPushUnavailable
	}
	if strings.TrimSpace(msg.Title) == "" && strings.TrimSpace(msg.Text) == "" {
		w.logger.WarnContext(ctx, "webpush title and body are both empty")
		return nil
	}
	if !w.usernameAllowed(msg.Username) {
		return nil
	}
	payload, err := WebPushPayload(msg)
	if err != nil {
		return fmt.Errorf("build webpush payload: %w", err)
	}
	subscriptions, err := w.source.List(ctx)
	if err != nil {
		return errWebPushUnavailable
	}
	for _, subscription := range subscriptions {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := w.deliver(ctx, subscription, payload); err != nil {
			if contextErr := ctx.Err(); contextErr != nil {
				return contextErr
			}
			w.logger.ErrorContext(ctx, "webpush delivery failed",
				slog.String("type", string(msg.Type)),
				slog.String("endpoint_host", webPushEndpointHost(subscription.Endpoint)),
				slog.String("error", err.Error()))
		}
	}
	return nil
}

func (w *WebPushSender) usernameAllowed(username string) bool {
	if len(w.config.Usernames) == 0 {
		return true
	}
	username = strings.TrimSpace(username)
	if username == "" {
		return false
	}
	for _, allowed := range w.config.Usernames {
		if strings.TrimSpace(allowed) == username {
			return true
		}
	}
	return false
}

var _ ChannelSender = (*WebPushSender)(nil)

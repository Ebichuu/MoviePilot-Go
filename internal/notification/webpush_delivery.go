package notification

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// deliver 加密并向单个订阅端点投递；404/410 视为订阅失效并移除。
func (w *WebPushSender) deliver(
	ctx context.Context,
	subscription WebPushSubscription,
	payload []byte,
) error {
	body, err := encryptWebPushPayload(payload, subscription.P256dh, subscription.Auth)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, subscription.Endpoint, strings.NewReader(string(body)))
	if err != nil {
		return errWebPushUnavailable
	}
	request.Header.Set("Content-Encoding", "aes128gcm")
	request.Header.Set("Content-Type", "application/octet-stream")
	request.Header.Set("TTL", strconv.Itoa(webPushTTLSeconds(w.ttlFor(subscription.Endpoint))))
	if isWNSEndpoint(subscription.Endpoint) {
		// WNS 要求 TTL 与 X-WNS-Cache-Policy 一致，否则返回 400。
		request.Header.Set("X-WNS-Cache-Policy", "cache")
	}
	authorization, err := vapidAuthorization(
		subscription.Endpoint,
		w.config.VAPIDSubject,
		w.config.VAPIDPrivateKey,
		w.config.VAPIDPublicKey,
		w.now(),
	)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", authorization)

	response, err := w.httpClient.Do(request)
	if err != nil {
		return errWebPushUnavailable
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		return nil
	}
	if response.StatusCode == http.StatusNotFound || response.StatusCode == http.StatusGone {
		if removeErr := w.source.Remove(ctx, subscription.Endpoint); removeErr != nil {
			w.logger.ErrorContext(ctx, "remove gone webpush subscription failed",
				"endpoint_host", webPushEndpointHost(subscription.Endpoint))
		}
	}
	return fmt.Errorf("webpush endpoint returned status %d", response.StatusCode)
}

// ttlFor 复刻 V2：WNS 端点固定 86400 秒（并要求 X-WNS-Cache-Policy: cache），
// 其余端点沿用 pywebpush 的默认 TTL=0（立即投递、不做缓存）。
func (w *WebPushSender) ttlFor(endpoint string) time.Duration {
	if isWNSEndpoint(endpoint) {
		return webPushWNSDefaultTTL
	}
	if w.config.TTL > 0 {
		return w.config.TTL
	}
	return 0
}

func webPushTTLSeconds(ttl time.Duration) int {
	if ttl <= 0 {
		return 0
	}
	return int(ttl / time.Second)
}

// WebPushPayload 构造 V2 的 `{title, body, url}` JSON 载荷。
func WebPushPayload(msg Message) ([]byte, error) {
	title := strings.TrimSpace(msg.Title)
	body := msg.Text
	if title == "" {
		title = body
		body = ""
	}
	url := strings.TrimSpace(msg.Link)
	if url == "" {
		url = webPushDefaultURL
	}
	payload, err := json.Marshal(map[string]any{
		"title": title,
		"body":  body,
		"url":   url,
	})
	if err != nil || len(payload) > webPushPayloadMaxBytes {
		return nil, errWebPushConfig
	}
	return payload, nil
}

// isWNSEndpoint 复刻 V2 `is_wns_endpoint`（app/helper/webpush.py:18–22）。
func isWNSEndpoint(endpoint string) bool {
	return strings.Contains(endpoint, "notify.windows.com")
}

func webPushEndpointHost(endpoint string) string {
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Host == "" {
		return ""
	}
	return parsed.Host
}

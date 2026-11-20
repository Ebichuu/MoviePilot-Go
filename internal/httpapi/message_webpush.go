package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/moviepilot-go/moviepilot-go/internal/database"
)

type webPushSubscriptionRequest struct {
	Endpoint string `json:"endpoint"`
	Keys     struct {
		P256dh string `json:"p256dh"`
		Auth   string `json:"auth"`
	} `json:"keys"`
	UserAgent string `json:"user_agent"`
}

type webPushSubscriptionWriter interface {
	UpsertWebPushSubscription(context.Context, database.WebPushSubscription) error
}

// WebPushDispatcher 发送一条 WebPush 通知（V2 `/message/webpush/send`）。
type WebPushDispatcher interface {
	SendWebPush(ctx context.Context, title, body, link string) error
}

// subscribeWebPush 复刻 V2 `/message/webpush/subscribe`（app/api/endpoints/message.py:285–299）。
func (d Dependencies) subscribeWebPush(w http.ResponseWriter, r *http.Request) {
	user, ok := d.authorizedUser(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "未认证")
		return
	}
	writer, ok := any(d.Store).(webPushSubscriptionWriter)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "WebPush 订阅存储未初始化")
		return
	}
	var input webPushSubscriptionRequest
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		writeError(w, http.StatusBadRequest, "WebPush 订阅请求无效")
		return
	}
	endpoint := strings.TrimSpace(input.Endpoint)
	if endpoint == "" || strings.TrimSpace(input.Keys.P256dh) == "" || strings.TrimSpace(input.Keys.Auth) == "" {
		writeError(w, http.StatusUnprocessableEntity, "WebPush 订阅参数不完整")
		return
	}
	if err := writer.UpsertWebPushSubscription(r.Context(), database.WebPushSubscription{
		Endpoint: endpoint, Username: user.Username,
		P256dh: input.Keys.P256dh, Auth: input.Keys.Auth, UserAgent: input.UserAgent,
	}); err != nil {
		writeError(w, http.StatusUnprocessableEntity, "WebPush 订阅保存失败")
		return
	}
	writeJSON(w, http.StatusOK, Response{Success: true})
}

type webPushSendRequest struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	URL   string `json:"url"`
}

// sendWebPush 复刻 V2 `/message/webpush/send`（app/api/endpoints/message.py:302–326）：
// 单条失败不改变整体成功响应，失效订阅在发送端移除。
func (d Dependencies) sendWebPush(w http.ResponseWriter, r *http.Request) {
	if _, ok := d.authorizedUser(r); !ok {
		writeError(w, http.StatusUnauthorized, "未认证")
		return
	}
	if d.WebPush == nil {
		writeJSON(w, http.StatusOK, Response{Success: true})
		return
	}
	var input webPushSendRequest
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		writeError(w, http.StatusBadRequest, "WebPush 请求无效")
		return
	}
	if err := d.WebPush.SendWebPush(r.Context(), strings.TrimSpace(input.Title), input.Body, strings.TrimSpace(input.URL)); err != nil {
		if contextErr := r.Context().Err(); contextErr != nil {
			writeError(w, http.StatusServiceUnavailable, "WebPush 发送已取消")
			return
		}
	}
	writeJSON(w, http.StatusOK, Response{Success: true})
}

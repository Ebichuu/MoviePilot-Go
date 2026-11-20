package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/moviepilot-go/moviepilot-go/internal/notificationconfig"
)

const (
	notificationSettingsKey = "Notifications"
	notificationReloadLimit = 5 * time.Second
)

// NotificationSettingStore is the smallest persistence contract for V2's
// Notifications setting. The raw JSON is kept so unknown V2 fields round-trip.
type NotificationSettingStore interface {
	LoadNotificationSettings(context.Context) (json.RawMessage, bool, error)
	SaveNotificationSettings(context.Context, json.RawMessage) error
}

func (d Dependencies) notificationSettingStore() (NotificationSettingStore, error) {
	if d.NotificationSettings != nil {
		return d.NotificationSettings, nil
	}
	if d.Store != nil {
		return d.Store, nil
	}
	return nil, errors.New("通知设置存储不可用")
}

func (d Dependencies) getNotificationSettings(w http.ResponseWriter, r *http.Request) {
	store, err := d.notificationSettingStore()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "读取消息通知设置失败")
		return
	}
	raw, found, err := store.LoadNotificationSettings(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "读取消息通知设置失败")
		return
	}
	if !found {
		writeSuccess(w, map[string]any{"value": nil})
		return
	}
	value, err := decodeSettingValue(raw)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "读取消息通知设置失败")
		return
	}
	writeSuccess(w, map[string]any{"value": value})
}

func (d Dependencies) setNotificationSettings(w http.ResponseWriter, r *http.Request) {
	raw, err := readNotificationSettingValue(r, d.Config.MaxRequestBody)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "消息通知设置必须是有效 JSON")
		return
	}
	store, err := d.notificationSettingStore()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "保存消息通知设置失败")
		return
	}
	if err := store.SaveNotificationSettings(r.Context(), raw); err != nil {
		writeError(w, http.StatusInternalServerError, "保存消息通知设置失败")
		return
	}
	if d.ReloadNotifications != nil {
		reloadCtx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), notificationReloadLimit)
		reloadErr := d.ReloadNotifications(reloadCtx)
		cancel()
		if reloadErr != nil {
			logger := d.Logger
			if logger == nil {
				logger = slog.Default()
			}
			// V2 returns success after persistence. Keep the old runtime snapshot
			// on reload failure and never log the underlying error or its secrets.
			logger.Warn("消息通知运行态重载失败，继续使用旧快照")
		}
	}
	writeSuccess(w, nil)
}

func readNotificationSettingValue(r *http.Request, maxBytes int64) (json.RawMessage, error) {
	if r == nil || r.Body == nil {
		return json.RawMessage("null"), nil
	}
	if maxBytes <= 0 {
		maxBytes = 1 << 20
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("读取设置: %w", err)
	}
	if int64(len(body)) > maxBytes {
		return nil, errors.New("设置请求体过大")
	}
	if len(bytes.TrimSpace(body)) == 0 {
		body = []byte("null")
	}
	snapshot, err := notificationconfig.Decode(body)
	if err != nil {
		return nil, errors.New("设置值不是有效 JSON")
	}
	return snapshot.Raw, nil
}

func decodeSettingValue(raw json.RawMessage) (any, error) {
	if len(bytes.TrimSpace(raw)) == 0 || !json.Valid(raw) {
		return nil, errors.New("设置值不是有效 JSON")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, errors.New("解析设置值失败")
	}
	return value, nil
}

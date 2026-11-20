package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/moviepilot-go/moviepilot-go/internal/notificationhistory"
)

const (
	notificationHistoryDefaultPage  = 1
	notificationHistoryDefaultCount = 20
)

// NotificationHistoryStore is the HTTP boundary required by the V2 notification center.
type NotificationHistoryStore interface {
	ListNotificationHistory(context.Context, int, int) ([]notificationhistory.Event, error)
	UpdateNotificationClearBefore(
		context.Context,
		notificationhistory.Scope,
		int64,
	) (notificationhistory.ClearBefore, error)
}

func (d Dependencies) listNotificationHistory(w http.ResponseWriter, r *http.Request) {
	page, count, err := notificationHistoryPage(r)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "通知历史分页参数无效")
		return
	}
	store := d.notificationHistoryStore()
	if store == nil {
		writeError(w, http.StatusServiceUnavailable, "通知历史存储不可用")
		return
	}

	events, err := store.ListNotificationHistory(r.Context(), page, count)
	if errors.Is(err, notificationhistory.ErrInvalidPage) {
		writeError(w, http.StatusUnprocessableEntity, "通知历史分页参数无效")
		return
	}
	if err != nil {
		if d.Logger != nil {
			d.Logger.Error("查询通知历史失败", "request_id", requestID(r.Context()))
		}
		writeError(w, http.StatusInternalServerError, "查询通知历史失败")
		return
	}

	location := d.Config.Location
	if location == nil {
		location = time.Local
	}
	items := make([]notificationhistory.V2Item, 0, len(events))
	for _, event := range events {
		item, projectionErr := event.V2(location)
		if projectionErr != nil {
			if d.Logger != nil {
				d.Logger.Error("转换通知历史失败", "request_id", requestID(r.Context()), "notification_id", event.ID)
			}
			writeError(w, http.StatusInternalServerError, "查询通知历史失败")
			return
		}
		items = append(items, item)
	}
	writeJSON(w, http.StatusOK, items)
}

func (d Dependencies) clearNotificationHistory(w http.ResponseWriter, r *http.Request) {
	scope, err := notificationHistoryScope(r)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "通知历史清理范围无效")
		return
	}
	store := d.notificationHistoryStore()
	if store == nil {
		writeError(w, http.StatusServiceUnavailable, "通知历史存储不可用")
		return
	}

	clearBefore, err := store.UpdateNotificationClearBefore(r.Context(), scope, time.Now().UnixMilli())
	if errors.Is(err, notificationhistory.ErrInvalidScope) || errors.Is(err, notificationhistory.ErrInvalidClearBefore) {
		writeError(w, http.StatusUnprocessableEntity, "通知历史清理范围无效")
		return
	}
	if err != nil {
		if d.Logger != nil {
			d.Logger.Error("清理通知历史失败", "request_id", requestID(r.Context()), "scope", scope)
		}
		writeError(w, http.StatusInternalServerError, "清理通知历史失败")
		return
	}
	writeSuccess(w, map[string]any{"clear_before": clearBefore})
}

func (d Dependencies) notificationHistoryStore() NotificationHistoryStore {
	if d.NotificationHistory != nil {
		return d.NotificationHistory
	}
	if d.Store == nil {
		return nil
	}
	return d.Store
}

func notificationHistoryPage(r *http.Request) (int, int, error) {
	page, err := notificationHistoryQueryInt(r, "page", notificationHistoryDefaultPage)
	if err != nil {
		return 0, 0, notificationhistory.ErrInvalidPage
	}
	count, err := notificationHistoryQueryInt(r, "count", notificationHistoryDefaultCount)
	if err != nil {
		return 0, 0, notificationhistory.ErrInvalidPage
	}
	if err := notificationhistory.ValidatePage(page, count); err != nil {
		return 0, 0, err
	}
	return page, count, nil
}

func notificationHistoryQueryInt(r *http.Request, key string, fallback int) (int, error) {
	values, exists := r.URL.Query()[key]
	if !exists {
		return fallback, nil
	}
	if len(values) == 0 {
		return 0, notificationhistory.ErrInvalidPage
	}
	value, err := strconv.Atoi(strings.TrimSpace(values[len(values)-1]))
	if err != nil {
		return 0, notificationhistory.ErrInvalidPage
	}
	return value, nil
}

func notificationHistoryScope(r *http.Request) (notificationhistory.Scope, error) {
	values, exists := r.URL.Query()["scope"]
	if !exists {
		return notificationhistory.ScopeAll, nil
	}
	if len(values) == 0 {
		return "", notificationhistory.ErrInvalidScope
	}
	scope := notificationhistory.Scope(values[len(values)-1])
	if !scope.Valid() {
		return "", notificationhistory.ErrInvalidScope
	}
	return scope, nil
}

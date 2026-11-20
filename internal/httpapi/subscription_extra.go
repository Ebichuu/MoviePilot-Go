package httpapi

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/moviepilot-go/moviepilot-go/internal/database"
)

// getSubscription 对应 V2 `GET /subscribe/{subscribe_id}`：按 ID 查询订阅详情。
// 未命中时与 `GET /subscribe/media/{mediaid}` 一致，返回空订阅对象而不是 404。
func (d Dependencies) getSubscription(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFromContext(r.Context())
	id, err := strconv.ParseInt(strings.TrimSpace(chi.URLParam(r, "subscribe_id")), 10, 64)
	if err != nil || id <= 0 {
		writeJSON(w, http.StatusOK, emptySubscription())
		return
	}
	items, err := d.Store.ListSubscriptions(r.Context(), claims.Username, claims.SuperUser)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "查询订阅失败")
		return
	}
	for _, item := range items {
		if item.ID != id {
			continue
		}
		writeJSON(w, http.StatusOK, item.V2())
		return
	}
	writeJSON(w, http.StatusOK, emptySubscription())
}

// listAllSubscriptions 对应 V2 `GET /subscribe/list?token=...`：API_TOKEN 版本的全量
// 订阅列表（不限用户），供外部脚本同步。
func (d Dependencies) listAllSubscriptions(w http.ResponseWriter, r *http.Request) {
	if d.Store == nil {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	items, err := d.Store.ListSubscriptions(r.Context(), "", true)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "查询订阅失败")
		return
	}
	result := make([]map[string]any, 0, len(items))
	for _, item := range items {
		result = append(result, item.V2())
	}
	writeJSON(w, http.StatusOK, result)
}

// deleteSubscriptionByMedia 对应 V2 `DELETE /subscribe/media/{mediaid}`：按媒体键删除
// 当前用户可见的订阅，`season` 查询参数用于区分同一剧集的不同季。
func (d Dependencies) deleteSubscriptionByMedia(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFromContext(r.Context())
	source, mediaID, ok := splitMediaKey(mediaKeyParam(r, "mediaid"))
	if !ok {
		writeSuccess(w, map[string]any{})
		return
	}
	season := 0
	if value := strings.TrimSpace(r.URL.Query().Get("season")); value != "" {
		season, _ = strconv.Atoi(value)
	}
	items, err := d.Store.ListSubscriptions(r.Context(), claims.Username, claims.SuperUser)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "查询订阅失败")
		return
	}
	for _, item := range items {
		if !strings.EqualFold(item.MediaSource, source) || item.MediaID != mediaID {
			continue
		}
		if season > 0 && item.Season != nil && *item.Season != season {
			continue
		}
		deleted, err := d.Store.DeleteSubscription(r.Context(), item.ID, claims.Username, claims.SuperUser)
		if err != nil && !errors.Is(err, database.ErrSubscriptionNotFound) {
			writeError(w, http.StatusInternalServerError, "删除订阅失败")
			return
		}
		writeSuccess(w, nil)
		_ = deleted
		return
	}
	writeSuccess(w, map[string]any{})
}

// subscribeSearch 对应 V2 `GET /subscribe/search?keyword=&page=&count=`：
// 按订阅名做关键字过滤，返回 V2 `Subscribe[]`。
func (d Dependencies) subscribeSearch(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFromContext(r.Context())
	items, err := d.Store.ListSubscriptions(r.Context(), claims.Username, claims.SuperUser)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "查询订阅失败")
		return
	}
	keyword := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("keyword")))
	page := parseHistoryInt(r.URL.Query().Get("page"), 1, 1, 10000)
	count := parseHistoryInt(r.URL.Query().Get("count"), 30, 1, 100)
	result := make([]map[string]any, 0, len(items))
	for _, item := range items {
		if keyword != "" && !strings.Contains(strings.ToLower(item.Name), keyword) {
			continue
		}
		result = append(result, item.V2())
	}
	start := (page - 1) * count
	if start >= len(result) {
		writeJSON(w, http.StatusOK, []map[string]any{})
		return
	}
	end := start + count
	if end > len(result) {
		end = len(result)
	}
	writeJSON(w, http.StatusOK, result[start:end])
}

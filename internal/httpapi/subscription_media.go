package httpapi

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
)

// mediaKeyParam 读取媒体键路径参数并做一次百分号解码。chi 在匹配路由时不做解码，
// 而前端（axios）会把 `themoviedb:278` 编码成 `themoviedb%3A278`，不还原就永远匹配不到。
func mediaKeyParam(r *http.Request, name string) string {
	raw := chi.URLParam(r, name)
	if decoded, err := url.PathUnescape(raw); err == nil {
		return decoded
	}
	return raw
}

// subscriptionByMedia 对应 V2 `GET /subscribe/media/{mediaid}`：按媒体键
// （themoviedb:550、douban:1292052、bangumi:253、anilist:21）查询当前用户可见的订阅。
// 推荐/发现页的每张卡片都会调用它判断“是否已订阅”，因此未命中时必须返回空订阅对象
// 而不是 404。
func (d Dependencies) subscriptionByMedia(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFromContext(r.Context())
	source, mediaID, ok := splitMediaKey(mediaKeyParam(r, "mediaid"))
	if !ok {
		writeJSON(w, http.StatusOK, emptySubscription())
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
		writeJSON(w, http.StatusOK, item.V2())
		return
	}
	writeJSON(w, http.StatusOK, emptySubscription())
}

// listUserSubscriptions 对应 V2 `GET /subscribe/user/{username}`：管理员可查任意
// 用户，普通用户只能查自己，越权查询返回空数组。
func (d Dependencies) listUserSubscriptions(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFromContext(r.Context())
	username := strings.TrimSpace(chi.URLParam(r, "username"))
	if !claims.SuperUser && username != claims.Username {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	items, err := d.Store.ListSubscriptions(r.Context(), username, false)
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

// splitMediaKey 解析 V2 的媒体键，并把 tmdb 归一到 themoviedb。
func splitMediaKey(value string) (string, string, bool) {
	parts := strings.SplitN(strings.TrimSpace(value), ":", 2)
	if len(parts) != 2 || parts[1] == "" {
		return "", "", false
	}
	source := strings.ToLower(strings.TrimSpace(parts[0]))
	if source == "tmdb" {
		source = "themoviedb"
	}
	if source == "" {
		return "", "", false
	}
	return source, parts[1], true
}

// emptySubscription 返回 V2 空订阅对象：字段齐全但全部为空，前端据此判断“未订阅”。
func emptySubscription() map[string]any {
	return map[string]any{
		"id": nil, "username": nil, "name": nil, "type": nil, "year": nil,
		"season": nil, "media_source": nil, "media_id": nil, "state": nil,
		"date": nil, "last_update": nil, "total_episode": 0, "lack_episode": 0,
		"completed_episode": nil, "sites": []int{}, "filter_groups": []string{},
		"note": []any{},
	}
}

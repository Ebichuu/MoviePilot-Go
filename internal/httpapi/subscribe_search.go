package httpapi

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
)

// subscribeSearchByID 对应 V2 `GET /subscribe/search/{subscribe_id}`：
// 用该订阅的标题跑一次索引站点搜索，返回候选资源列表。
func (d Dependencies) subscribeSearchByID(w http.ResponseWriter, r *http.Request) {
	if d.Store == nil {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	claims, _ := claimsFromContext(r.Context())
	id, err := strconv.ParseInt(strings.TrimSpace(chi.URLParam(r, "subscribe_id")), 10, 64)
	if err != nil || id <= 0 {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	items, listErr := d.Store.ListSubscriptions(r.Context(), claims.Username, claims.SuperUser)
	if listErr != nil {
		writeError(w, http.StatusInternalServerError, "查询订阅失败")
		return
	}
	for _, item := range items {
		if item.ID != id {
			continue
		}
		if strings.TrimSpace(item.Name) == "" {
			writeJSON(w, http.StatusOK, []any{})
			return
		}
		d.searchJSON(w, r, item.Name)
		return
	}
	writeJSON(w, http.StatusOK, []any{})
}

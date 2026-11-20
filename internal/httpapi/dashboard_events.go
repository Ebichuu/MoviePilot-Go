package httpapi

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	dashboardEventDefaultDays = 7
	dashboardEventMaxDays     = 90
)

// dashboardEventStatistics 是新增端点 GET /dashboard/events?days=7：
// 返回按天、按事件类型聚合的计数（download.completed / organization.completed 等），
// 供仪表盘与外部集成观察业务节奏。未配置存储时返回空数组而不是报错。
func (d Dependencies) dashboardEventStatistics(w http.ResponseWriter, r *http.Request) {
	days := dashboardEventDefaultDays
	if raw := strings.TrimSpace(r.URL.Query().Get("days")); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 && parsed <= dashboardEventMaxDays {
			days = parsed
		}
	}
	if d.Store == nil {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	since := time.Now().UTC().AddDate(0, 0, -(days - 1))
	items, err := d.Store.ListEventStatistics(r.Context(), since)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "查询事件统计失败")
		return
	}
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		out = append(out, map[string]any{
			"date":       item.Day.Format("2006-01-02"),
			"event_type": item.EventType,
			"count":      item.Count,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

package httpapi

import (
	"net/http"

	"github.com/go-chi/chi/v5"
)

// 本文件补齐官方前端会直接调用的入口。这些入口在 Go 侧还没有接入真实数据，但按计划的
// V2 兼容原则必须返回 V2 的空状态/零值，而不是 404：一旦落到 404，整页或整块卡片会报错，
// 用户看到的是「功能坏了」而不是「还没有数据」。真实数据由后续切片替换这里的实现。

// publicSystemConfigKeys 复刻 V2 `_PUBLIC_SYSTEM_CONFIG_KEYS`
// （app/api/endpoints/system.py）：普通用户可读取的非敏感系统配置。
var publicSystemConfigKeys = map[string]struct{}{
	"Directories":                 {},
	"Storages":                    {},
	"IndexerSites":                {},
	"EpisodeFormatRuleTable":      {},
	"DefaultMovieSubscribeConfig": {},
	"DefaultTvSubscribeConfig":    {},
	"FollowSubscribers":           {},
}

// dashboardSchedule 对应 V2 `GET /dashboard/schedule`（List[ScheduleInfo]）。
// River 的周期任务还没有投影成 V2 形状，先返回空列表。
func dashboardSchedule(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, []any{})
}

// transferQueue 对应 V2 `GET /transfer/queue`（List[TransferJob]）。
func transferQueue(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, []any{})
}

// pluginSidebarNav 对应 V2 `GET /plugin/sidebar_nav`（List[PluginSidebarNavItem]）。
// 首版不装载 Python 插件，因此没有侧栏入口。
func pluginSidebarNav(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, []any{})
}

// agentMCPServers 对应 V2 `GET /message/agent/mcp/servers`（Response）。
func agentMCPServers(w http.ResponseWriter, _ *http.Request) {
	writeSuccess(w, map[string]any{
		"servers": []any{}, "enabled_count": 0, "total_count": 0,
	})
}

// publicSetting 对应 V2 `GET /system/setting/public/{key}`：普通用户可读取白名单内的
// 非敏感配置；白名单外的键按 V2 返回 404。
func (d Dependencies) publicSetting(w http.ResponseWriter, r *http.Request) {
	key := chi.URLParam(r, "key")
	if _, ok := publicSystemConfigKeys[key]; !ok {
		writeError(w, http.StatusNotFound, "配置项不存在")
		return
	}
	var value any
	if d.Store != nil {
		found, err := d.Store.GetSetting(r.Context(), key, &value)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "读取配置失败")
			return
		}
		if !found {
			value = nil
		}
	}
	writeSuccess(w, map[string]any{"value": value})
}

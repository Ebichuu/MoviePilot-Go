package httpapi

import (
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
)

// mediaServerPlay 对应 V2 `GET /mediaserver/play/{itemid}`：返回媒体服务器播放页面地址。
// V2 的路径参数是 `{itemid:path}`，这里用通配段保持一致的兼容性。
func (d Dependencies) mediaServerPlay(w http.ResponseWriter, r *http.Request) {
	itemID := strings.Trim(strings.TrimSpace(chi.URLParam(r, "*")), "/")
	if itemID == "" {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "参数错误"})
		return
	}
	if d.MediaServerPlayback == nil {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "未配置媒体服务器"})
		return
	}
	playback, found, err := d.MediaServerPlayback.PlayMediaServerItem(r.Context(), itemID)
	if err != nil || !found || strings.TrimSpace(playback.URL) == "" {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "未找到播放地址"})
		return
	}
	writeSuccess(w, playback)
}

// systemRunScheduler 对应 V2 `GET /system/runscheduler?jobid=...`：立即触发一次后台服务。
func (d Dependencies) systemRunScheduler(w http.ResponseWriter, r *http.Request) {
	jobID := strings.TrimSpace(r.URL.Query().Get("jobid"))
	if jobID == "" {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "命令不能为空！"})
		return
	}
	if d.Scheduler == nil {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "后台服务不可用"})
		return
	}
	triggered, err := d.Scheduler.RunScheduler(r.Context(), jobID)
	if err != nil || !triggered {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "后台服务不存在"})
		return
	}
	writeJSON(w, http.StatusOK, Response{Success: true})
}

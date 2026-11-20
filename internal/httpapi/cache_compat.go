package httpapi

import "net/http"

// refusal 统一返回「明确不支持」的响应体，避免为不存在的子系统伪造成功。
func refusal(message string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: message})
	}
}

// successOnly 用于「没有副作用可做」的幂等接口（清空/刷新空缓存等）。
func successOnly(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, Response{Success: true})
}

// torrentCacheList 对应 V2 `GET /torrent/cache`：种子搜索缓存视图。
// 本项目不做搜索结果缓存，因此缓存恒为空。
func torrentCacheList(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, Response{Success: true, Data: map[string]any{
		"count": 0, "sites": 0, "data": []any{},
	}})
}

// torrentCacheReidentify 对应 V2 `POST /torrent/cache/reidentify/{domain}/{hash}`：
// 没有缓存条目可重新识别。
func torrentCacheReidentify(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, Response{Success: false, Message: "未找到缓存条目"})
}

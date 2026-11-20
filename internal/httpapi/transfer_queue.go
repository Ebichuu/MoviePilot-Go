package httpapi

import "net/http"

// transferQueue 对应 V2 `GET /transfer/queue`：本项目整理流程同步执行、没有持久化
// 整理队列，因此返回空数组（与 V2 队列空闲时一致）。
func transferQueueList(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, []any{})
}

// transferNow 对应 V2 `GET /transfer/now`：同上。
func transferNowList(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, []any{})
}

// removeTransferQueue 对应 V2 `DELETE /transfer/queue`：没有队列项可删，按 V2 语义成功。
func removeTransferQueueItem(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, Response{Success: true})
}

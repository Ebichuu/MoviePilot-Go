package httpapi

import "net/http"

// systemLogging 对应 V2 `GET /system/logging`：列出日志文件。
// 本项目日志写入进程标准输出（容器日志），不在磁盘保留日志文件，因此返回空列表。
func systemLogging(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, []any{})
}

// systemLoggingDownload 对应 V2 `GET /system/logging/download/{name}`。同上，没有可下载文件。
func systemLoggingDownload(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, Response{Success: false, Message: "日志文件不存在"})
}

// transferAIRedo 对应 V2 `POST /history/transfer/ai-redo`：需要用 LLM 重新命名。
// 本项目未接入 LLM 路由，明确拒绝而不是伪造结果。
func transferAIRedo(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, Response{Success: false, Message: "未配置 AI 模型，无法执行 AI 重命名"})
}

// transferAIHistoryRedo 对应 V2 `POST /history/transfer/{history_id}/ai-redo`。同上。
func transferAIHistoryRedo(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, Response{Success: false, Message: "未配置 AI 模型，无法执行 AI 重命名"})
}

package httpapi

import (
	"fmt"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

const (
	defaultLoggingTailLines = 50
	maxLoggingTailLines     = 2000
	loggingHeartbeatPeriod  = 15 * time.Second
)

// systemLogging 保留无依赖降级入口（供最小路由测试直接调用）。
func systemLogging(w http.ResponseWriter, r *http.Request) {
	(Dependencies{}).systemLogging(w, r)
}

// systemLoggingDownload 保留无依赖降级入口。
func systemLoggingDownload(w http.ResponseWriter, r *http.Request) {
	(Dependencies{}).systemLoggingDownload(w, r)
}

// systemLogging 对应 V2 `GET /system/logging`：
// - 未装配内存日志缓冲时返回空数组（兼容最小部署）；
// - `length=-1` 返回 `text/plain` 全量缓冲日志；
// - 其他情况以 V2 兼容的 SSE (`text/event-stream`) 先推最近 `length` 行，再实时推送新日志。
func (d Dependencies) systemLogging(w http.ResponseWriter, r *http.Request) {
	if d.LogBuffer == nil {
		writeJSON(w, http.StatusOK, []any{})
		return
	}

	length := defaultLoggingTailLines
	if raw := strings.TrimSpace(r.URL.Query().Get("length")); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil {
			length = parsed
		}
	}

	if length == -1 {
		lines := d.LogBuffer.All()
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		if len(lines) > 0 {
			_, _ = w.Write([]byte(strings.Join(lines, "\n") + "\n"))
		}
		return
	}

	if length <= 0 {
		length = defaultLoggingTailLines
	}
	if length > maxLoggingTailLines {
		length = maxLoggingTailLines
	}

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	flusher, canFlush := w.(http.Flusher)

	// 先订阅再读历史，避免在切换瞬间漏掉新日志。
	liveCh, unsubscribe := d.LogBuffer.Subscribe(128)
	defer unsubscribe()

	history := d.LogBuffer.Tail(length)
	for _, line := range history {
		if _, err := fmt.Fprintf(w, "data: %s\n\n", sanitizeSSELine(line)); err != nil {
			return
		}
	}
	if canFlush {
		flusher.Flush()
	}

	ticker := time.NewTicker(loggingHeartbeatPeriod)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case line, ok := <-liveCh:
			if !ok {
				return
			}
			if _, err := fmt.Fprintf(w, "data: %s\n\n", sanitizeSSELine(line)); err != nil {
				return
			}
			if canFlush {
				flusher.Flush()
			}
		case <-ticker.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			if canFlush {
				flusher.Flush()
			}
		}
	}
}

// systemLoggingDownload 对应 V2 `GET /system/logging/download/{name}`：导出当前内存日志缓冲。
func (d Dependencies) systemLoggingDownload(w http.ResponseWriter, r *http.Request) {
	if d.LogBuffer == nil {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "日志文件不存在"})
		return
	}
	name := path.Base(strings.TrimSpace(chi.URLParam(r, "name")))
	if name == "" || name == "." || name == "/" || !strings.HasSuffix(strings.ToLower(name), ".log") {
		name = "moviepilot.log"
	}
	lines := d.LogBuffer.All()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name))
	w.WriteHeader(http.StatusOK)
	if len(lines) > 0 {
		_, _ = w.Write([]byte(strings.Join(lines, "\n") + "\n"))
	}
}

func sanitizeSSELine(line string) string {
	return strings.NewReplacer("\r", " ", "\n", " ").Replace(line)
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

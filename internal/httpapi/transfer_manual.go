package httpapi

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/moviepilot-go/moviepilot-go/internal/storage"
)

// manualTransferHistory 对应 V2 `POST /transfer/manual/history`，读整理链里真实落库的记录。
func (d Dependencies) manualTransferHistory(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if raw := strings.TrimSpace(r.URL.Query().Get("count")); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
			limit = parsed
		}
	}
	if d.Store == nil {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	records, err := d.Store.ListManualOrganizationHistory(r.Context(), limit)
	if err != nil {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "读取整理历史失败：" + err.Error()})
		return
	}
	items := make([]map[string]any, 0, len(records))
	for _, record := range records {
		items = append(items, map[string]any{
			"source_path": record.SourcePath,
			"target_path": record.TargetPath,
			"state":       record.State,
			"success":     record.State == "completed",
			"updated_at":  record.UpdatedAt.Format("2006-01-02 15:04:05"),
		})
	}
	writeJSON(w, http.StatusOK, items)
}

// manualTransfer 对应 V2 `POST /transfer/manual`：把指定的一个文件按目标路径整理（重命名/移动）。
func (d Dependencies) manualTransfer(w http.ResponseWriter, r *http.Request) {
	var body struct {
		SourcePath string `json:"source_path"`
		TargetPath string `json:"target_path"`
		Storage    string `json:"storage"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "请求体不是合法 JSON"})
		return
	}
	source := strings.TrimSpace(body.SourcePath)
	target := strings.TrimSpace(body.TargetPath)
	if source == "" || target == "" {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "source_path 与 target_path 不能为空"})
		return
	}
	provider := d.storageProvider()
	if provider == nil {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "未配置存储，无法手动整理"})
		return
	}
	storageName := strings.TrimSpace(body.Storage)
	if storageName == "" {
		storageName = "local"
	}
	done, err := provider.Rename(r.Context(), storageName, source, target, false)
	if err != nil {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "整理失败：" + err.Error()})
		return
	}
	if !done {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "整理失败：存储未执行该操作"})
		return
	}
	if d.Store != nil {
		if recordErr := d.Store.RecordManualOrganization(r.Context(), source, target); recordErr != nil && d.Logger != nil {
			d.Logger.Warn("手动整理记录写入失败", "error", recordErr)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"message": "整理完成",
		"data":    map[string]any{"source_path": source, "target_path": target},
	})
}

// manualTransferTargetPath 对应 V2 `POST /transfer/manual/target-path`。
// 目标路径依赖命名模板，而 Go 版尚未实现命名模板配置，因此明确拒绝而不是编造路径。
func (d Dependencies) manualTransferTargetPath(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, Response{
		Success: false,
		Message: "未配置命名模板，无法计算目标路径（整理链使用内置规则自动命名）",
	})
}

var _ = storage.ErrUnsupportedStorage

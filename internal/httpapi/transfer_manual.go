package httpapi

import (
	"encoding/json"
	"net/http"
	"path"
	"strconv"
	"strings"

	"github.com/moviepilot-go/moviepilot-go/internal/organization"
	"github.com/moviepilot-go/moviepilot-go/internal/storage"
)

// manualTransferHistory 对应 V2 `POST /transfer/manual/history`，读整理链里真实落库的记录（含失败原因）。
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
		item := map[string]any{
			"source_path": record.SourcePath,
			"target_path": record.TargetPath,
			"mode":        record.Mode,
			"state":       record.State,
			"success":     record.State == "completed",
			"updated_at":  record.UpdatedAt.Format("2006-01-02 15:04:05"),
		}
		if record.LastError != "" {
			item["errmsg"] = record.LastError
		}
		items = append(items, item)
	}
	writeJSON(w, http.StatusOK, items)
}

// manualTransfer 对应 V2 `POST /transfer/manual`：把指定的一个文件按目标路径整理（重命名/移动），
// 并在成功或失败时都写入整理历史，方便 Web 端排查失败原因。
func (d Dependencies) manualTransfer(w http.ResponseWriter, r *http.Request) {
	var body struct {
		SourcePath   string `json:"source_path"`
		TargetPath   string `json:"target_path"`
		Storage      string `json:"storage"`
		TransferType string `json:"transfer_type"`
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
	mode := strings.TrimSpace(body.TransferType)
	if mode == "" {
		mode = "move"
	}
	done, err := provider.Rename(r.Context(), storageName, source, target, false)
	if err != nil {
		if d.Store != nil {
			if recErr := d.Store.RecordFailedOrganization(r.Context(), source, target, mode, err.Error()); recErr != nil && d.Logger != nil {
				d.Logger.Warn("手动整理失败记录写入失败", "error", recErr)
			}
		}
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "整理失败：" + err.Error()})
		return
	}
	if !done {
		if d.Store != nil {
			if recErr := d.Store.RecordFailedOrganization(r.Context(), source, target, mode, "存储未执行该操作"); recErr != nil && d.Logger != nil {
				d.Logger.Warn("手动整理失败记录写入失败", "error", recErr)
			}
		}
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "整理失败：存储未执行该操作"})
		return
	}
	if d.Store != nil {
		if recordErr := d.Store.RecordManualOrganizationWithMode(r.Context(), source, target, mode); recordErr != nil && d.Logger != nil {
			d.Logger.Warn("手动整理记录写入失败", "error", recordErr)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"message": "整理完成",
		"data":    map[string]any{"source_path": source, "target_path": target},
	})
}

// manualTransferTargetPath 对应 V2 `POST /transfer/manual/target-path`：
// 使用配置的 `MOVIE_RENAME_FORMAT` / `TV_RENAME_FORMAT`（或请求指定的模板 / V2 默认模板）
// 渲染目标相对路径，并在提供 target_root 时拼出完整目标路径。
func (d Dependencies) manualTransferTargetPath(w http.ResponseWriter, r *http.Request) {
	if r.Body == nil {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "请提供源文件路径（source_path 或 filename）"})
		return
	}
	var body struct {
		SourcePath string `json:"source_path"`
		Filename   string `json:"filename"`
		TargetRoot string `json:"target_path"`
		MediaType  string `json:"media_type"`
		TypeName   string `json:"type_name"`
		Title      string `json:"title"`
		Year       string `json:"year"`
		Category   string `json:"category"`
		Season     int    `json:"season"`
		Episode    int    `json:"episode"`
		TMDBID     string `json:"tmdbid"`
		Template   string `json:"template"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "请提供有效的目标路径计算参数"})
		return
	}
	source := strings.TrimSpace(body.SourcePath)
	if source == "" {
		source = strings.TrimSpace(body.Filename)
	}
	if source == "" {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "source_path 与 filename 不能同时为空"})
		return
	}

	mediaType := strings.TrimSpace(body.MediaType)
	if mediaType == "" {
		mediaType = strings.TrimSpace(body.TypeName)
	}
	var episodes []int
	if body.Episode > 0 {
		episodes = []int{body.Episode}
	}

	templates := d.namingTemplates()
	relative, err := organization.RenderRelativeTarget(organization.NamingInput{
		SourcePath:       source,
		MediaType:        mediaType,
		Title:            body.Title,
		Year:             body.Year,
		Category:         body.Category,
		Season:           body.Season,
		Episodes:         episodes,
		TMDBID:           body.TMDBID,
		ExplicitTemplate: body.Template,
	}, templates)
	if err != nil {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "计算目标路径失败：" + err.Error()})
		return
	}

	fullTarget := relative
	if root := strings.TrimSpace(body.TargetRoot); root != "" {
		cleanedRoot := strings.TrimRight(strings.ReplaceAll(root, `\`, "/"), "/")
		fullTarget = path.Join(cleanedRoot, relative)
	}

	writeJSON(w, http.StatusOK, Response{
		Success: true,
		Data: map[string]any{
			"target_path":   fullTarget,
			"relative_path": relative,
		},
	})
}

func (d Dependencies) namingTemplates() organization.NamingTemplates {
	templates := organization.NamingTemplates{
		Movie: d.Config.MovieRenameFormat,
		TV:    d.Config.TVRenameFormat,
	}
	if provider, ok := d.EnvironmentSettings.(interface{ RenameFormats() (string, string) }); ok && provider != nil {
		movieFmt, tvFmt := provider.RenameFormats()
		if movieFmt != "" {
			templates.Movie = movieFmt
		}
		if tvFmt != "" {
			templates.TV = tvFmt
		}
	}
	return templates
}

var _ = storage.ErrUnsupportedStorage

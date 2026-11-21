package httpapi

import (
	"encoding/json"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/moviepilot-go/moviepilot-go/internal/organization"
	"github.com/moviepilot-go/moviepilot-go/internal/organizer"
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

// manualTransfer 对应 V2 `POST /transfer/manual`：把指定的一个文件按目标路径整理，
// 本地存储支持 copy/move/hardlink/symlink（不可变计划 + SHA-256 校验）、可选刮削与刷新媒体库；
// 远程存储沿用 storage.Provider.Rename，成功与失败均写入整理历史。
func (d Dependencies) manualTransfer(w http.ResponseWriter, r *http.Request) {
	var body struct {
		SourcePath         string `json:"source_path"`
		TargetPath         string `json:"target_path"`
		Storage            string `json:"storage"`
		TransferType       string `json:"transfer_type"`
		Scrape             bool   `json:"scrape"`
		RefreshMediaServer bool   `json:"refresh_mediaserver"`
		TMDBID             int    `json:"tmdbid"`
		MediaType          string `json:"media_type"`
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
	storageName := strings.TrimSpace(body.Storage)
	if storageName == "" {
		storageName = "local"
	}
	modeStr := strings.ToLower(strings.TrimSpace(body.TransferType))
	if modeStr == "" {
		modeStr = "move"
	}

	// 本地存储且显式指定 copy/hardlink/symlink（或未配 provider 但本地文件存在）时走不可变 organizer。
	useLocalOrganizer := false
	var orgMode organizer.Mode
	if strings.EqualFold(storageName, "local") {
		if parsedMode, ok := parseManualTransferMode(modeStr); ok {
			orgMode = parsedMode
			if info, statErr := os.Stat(source); statErr == nil && info.Mode().IsRegular() {
				if modeStr != "move" || d.storageProvider() == nil {
					useLocalOrganizer = true
				}
			}
		}
	}

	if useLocalOrganizer {
		size, sum, fpErr := organizer.FingerprintFile(r.Context(), source)
		if fpErr != nil {
			d.recordManualTransferFailure(r, source, target, string(orgMode), fpErr.Error())
			writeJSON(w, http.StatusOK, Response{Success: false, Message: "整理失败：" + fpErr.Error()})
			return
		}
		plan, planErr := organizer.BuildPlan(filepath.Dir(target), []organizer.Operation{{
			Source:         source,
			Target:         target,
			Mode:           orgMode,
			ExpectedSize:   size,
			ExpectedSHA256: sum,
		}})
		if planErr != nil {
			d.recordManualTransferFailure(r, source, target, string(orgMode), planErr.Error())
			writeJSON(w, http.StatusOK, Response{Success: false, Message: "整理失败：" + planErr.Error()})
			return
		}
		if execErr := organizer.ExecuteWithinSourceRoot(r.Context(), filepath.Dir(source), plan); execErr != nil {
			d.recordManualTransferFailure(r, source, target, string(orgMode), execErr.Error())
			writeJSON(w, http.StatusOK, Response{Success: false, Message: "整理失败：" + execErr.Error()})
			return
		}
		modeStr = string(orgMode)
	} else {
		provider := d.storageProvider()
		if provider == nil {
			writeJSON(w, http.StatusOK, Response{Success: false, Message: "未配置存储，无法手动整理"})
			return
		}
		done, err := provider.Rename(r.Context(), storageName, source, target, false)
		if err != nil {
			d.recordManualTransferFailure(r, source, target, modeStr, err.Error())
			writeJSON(w, http.StatusOK, Response{Success: false, Message: "整理失败：" + err.Error()})
			return
		}
		if !done {
			d.recordManualTransferFailure(r, source, target, modeStr, "存储未执行该操作")
			writeJSON(w, http.StatusOK, Response{Success: false, Message: "整理失败：存储未执行该操作"})
			return
		}
	}

	if d.Store != nil {
		if recordErr := d.Store.RecordManualOrganizationWithMode(r.Context(), source, target, modeStr); recordErr != nil && d.Logger != nil {
			d.Logger.Warn("手动整理记录写入失败", "error", recordErr)
		}
	}

	scraped := false
	if body.Scrape && d.Scraper != nil && strings.EqualFold(storageName, "local") {
		if _, _, err := d.scrapeLocalPath(r.Context(), target, false, body.MediaType, body.TMDBID); err != nil {
			if d.Logger != nil {
				d.Logger.Warn("手动整理后刮削失败", "target", target, "error", err)
			}
		} else {
			scraped = true
		}
	}

	refreshed := false
	if body.RefreshMediaServer && d.MediaServerRefresher != nil {
		if err := d.MediaServerRefresher.RefreshAllMediaServers(r.Context()); err != nil {
			if d.Logger != nil {
				d.Logger.Warn("手动整理后刷新媒体库失败", "error", err)
			}
		} else {
			refreshed = true
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"message": "整理完成",
		"data": map[string]any{
			"source_path":         source,
			"target_path":         target,
			"mode":                modeStr,
			"scraped":             scraped,
			"refreshed":           refreshed,
			"refresh_mediaserver": refreshed,
		},
	})
}

func (d Dependencies) recordManualTransferFailure(r *http.Request, source, target, mode, errMsg string) {
	if d.Store == nil {
		return
	}
	if recErr := d.Store.RecordFailedOrganization(r.Context(), source, target, mode, errMsg); recErr != nil && d.Logger != nil {
		d.Logger.Warn("手动整理失败记录写入失败", "error", recErr)
	}
}

func parseManualTransferMode(raw string) (organizer.Mode, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "copy":
		return organizer.Copy, true
	case "move":
		return organizer.Move, true
	case "link", "hardlink":
		return organizer.Hardlink, true
	case "softlink", "symlink":
		return organizer.Symlink, true
	default:
		return "", false
	}
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

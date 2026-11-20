package httpapi

import (
	"fmt"
	"net/http"
	"path"
	"strings"

	"github.com/moviepilot-go/moviepilot-go/internal/media"
	"github.com/moviepilot-go/moviepilot-go/internal/templating"
)

// transferName 对应 V2 `GET /transfer/name?filename=&template=`：
// 用内置模板引擎渲染整理后的文件名，供前端「重命名预览」使用。
func (d Dependencies) transferName(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	filename := strings.TrimSpace(query.Get("filename"))
	if filename == "" {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "filename不能为空"})
		return
	}
	template := strings.TrimSpace(query.Get("template"))
	if template == "" {
		template = defaultTransferNameTemplate
	}
	base := path.Base(strings.ReplaceAll(filename, "\\", "/"))
	named, err := renderTransferName(base, template)
	if err != nil {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "命名模板渲染失败：" + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, Response{
		Success: true,
		Data:    map[string]any{"name": named, "template": template, "file": base},
	})
}

const defaultTransferNameTemplate = "{{ title }}{% if year %} ({{ year }}){% endif %}"

// renderTransferName 用内置模板引擎渲染文件名；扩展名缺失时补回原扩展名。
func renderTransferName(base, template string) (string, error) {
	parsed := media.Parse(base, media.Options{})
	seasonRaw := strings.TrimSpace(parsed.Season())
	episodeRaw := strings.TrimSpace(parsed.Episode())
	ext := strings.TrimPrefix(path.Ext(base), ".")
	context := map[string]any{
		"file":  base,
		"ext":   ext,
		"title": strings.TrimSpace(parsed.Name()),
		"year":  strings.TrimSpace(parsed.Year),
		"type":  strings.TrimSpace(string(parsed.Type)),
	}
	if seasonRaw != "" {
		context["season"] = strings.TrimLeft(seasonRaw, "Ss")
	}
	if episodeRaw != "" {
		context["episode"] = strings.TrimLeft(episodeRaw, "Ee")
	}
	if seasonRaw != "" && episodeRaw != "" {
		context["season_episode"] = seasonRaw + episodeRaw
	}
	rendered, err := templating.Render(template, context)
	if err != nil {
		return "", err
	}
	named := strings.TrimSpace(rendered)
	if named == "" {
		return "", fmt.Errorf("模板渲染结果为空")
	}
	if ext != "" && !strings.HasSuffix(named, "."+ext) {
		named = fmt.Sprintf("%s.%s", named, ext)
	}
	return named, nil
}

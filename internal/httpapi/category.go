package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/moviepilot-go/moviepilot-go/internal/metadata"
)

// CategoryConfigService is the HTTP boundary for category.yaml persistence.
type CategoryConfigService interface {
	LoadCategoryConfig() metadata.CategoryConfig
	SaveCategoryConfig(metadata.CategoryConfig) error
	MediaCategories() map[string][]string
}

func (d Dependencies) getCategoryConfig(w http.ResponseWriter, _ *http.Request) {
	if d.Categories == nil {
		writeError(w, http.StatusServiceUnavailable, "分类配置服务不可用")
		return
	}
	writeSuccess(w, d.Categories.LoadCategoryConfig())
}

func (d Dependencies) saveCategoryConfig(w http.ResponseWriter, r *http.Request) {
	if d.Categories == nil {
		writeError(w, http.StatusOK, "保存失败")
		return
	}
	var config metadata.CategoryConfig
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(&config); err != nil {
		writeError(w, http.StatusUnprocessableEntity, "分类配置格式无效")
		return
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusUnprocessableEntity, "分类配置格式无效")
		return
	}
	if err := d.Categories.SaveCategoryConfig(config); err != nil {
		writeError(w, http.StatusOK, "保存失败")
		return
	}
	writeJSON(w, http.StatusOK, Response{Success: true, Message: "保存成功", Data: map[string]any{}})
}

func (d Dependencies) listMediaCategories(w http.ResponseWriter, _ *http.Request) {
	if d.Categories == nil {
		writeError(w, http.StatusServiceUnavailable, "分类配置服务不可用")
		return
	}
	writeJSON(w, http.StatusOK, d.Categories.MediaCategories())
}

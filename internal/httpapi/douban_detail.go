package httpapi

import (
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/moviepilot-go/moviepilot-go/internal/media"
)

// doubanSubject 对应 V2 `GET /douban/{doubanid}`：按豆瓣 ID 取详情。
// doubanid 允许两种写法：纯数字（按 type_name 推断电影/电视剧）或 "movie:123"/"tv:123"。
func (d Dependencies) doubanSubject(w http.ResponseWriter, r *http.Request) {
	rawID := strings.TrimSpace(chi.URLParam(r, "doubanid"))
	if rawID == "" {
		writeJSON(w, http.StatusOK, emptyMediaInfo())
		return
	}
	mediaType := media.TypeMovie
	if strings.TrimSpace(r.URL.Query().Get("type_name")) == "电视剧" {
		mediaType = media.TypeTV
	}
	if strings.HasPrefix(rawID, "tv:") {
		mediaType = media.TypeTV
	}
	if d.Metadata == nil {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "未配置元数据源"})
		return
	}
	item, err := d.Metadata.Detail(r.Context(), "douban", typedMetadataID("douban", rawID, mediaType))
	if err != nil {
		if d.Logger != nil {
			d.Logger.Warn("豆瓣详情查询失败", "doubanid", rawID, "error", err)
		}
		writeJSON(w, http.StatusOK, emptyMediaInfo())
		return
	}
	writeJSON(w, http.StatusOK, v2MediaInfo(item, 0))
}

package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/moviepilot-go/moviepilot-go/internal/subtitle"
)

const subtitleUnconfiguredMessage = "未配置字幕源（请配置 MOVIEPILOT_OPENSUBTITLES_API_KEY 或 MOVIEPILOT_ASSRT_TOKEN）"

// searchSubtitleTitle 对应 V2 `GET /api/v1/search/subtitle/title`。
func (d Dependencies) searchSubtitleTitle(w http.ResponseWriter, r *http.Request) {
	query, err := parseSubtitleTitleQuery(r)
	if err != nil {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: err.Error()})
		return
	}
	d.executeSubtitleSearchJSON(w, r, query)
}

// searchSubtitleTitleStream 对应 V2 `GET /api/v1/search/subtitle/title/stream`。
func (d Dependencies) searchSubtitleTitleStream(w http.ResponseWriter, r *http.Request) {
	query, err := parseSubtitleTitleQuery(r)
	if err != nil {
		writeSubtitleSSEError(w, err.Error())
		return
	}
	d.executeSubtitleSearchStream(w, r, query)
}

// searchSubtitleMedia 对应 V2 `GET /api/v1/search/subtitle/media/{mediaid}`。
func (d Dependencies) searchSubtitleMedia(w http.ResponseWriter, r *http.Request) {
	query, err := d.parseSubtitleMediaQuery(r)
	if err != nil {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: err.Error()})
		return
	}
	d.executeSubtitleSearchJSON(w, r, query)
}

// searchSubtitleMediaStream 对应 V2 `GET /api/v1/search/subtitle/media/{mediaid}/stream`。
func (d Dependencies) searchSubtitleMediaStream(w http.ResponseWriter, r *http.Request) {
	query, err := d.parseSubtitleMediaQuery(r)
	if err != nil {
		writeSubtitleSSEError(w, err.Error())
		return
	}
	d.executeSubtitleSearchStream(w, r, query)
}

func (d Dependencies) executeSubtitleSearchJSON(w http.ResponseWriter, r *http.Request, query subtitle.Query) {
	if d.Subtitle == nil || !d.Subtitle.Enabled() {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: subtitleUnconfiguredMessage})
		return
	}
	results, err := d.Subtitle.Search(r.Context(), query, nil)
	if err != nil {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: err.Error()})
		return
	}
	if len(results) == 0 {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "未搜索到任何字幕"})
		return
	}
	writeSuccess(w, results)
}

func (d Dependencies) executeSubtitleSearchStream(w http.ResponseWriter, r *http.Request, query subtitle.Query) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "当前响应不支持流式搜索")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	if d.Subtitle == nil || !d.Subtitle.Enabled() {
		_ = writeSSEEvent(w, map[string]any{
			"type":    "error",
			"success": false,
			"message": subtitleUnconfiguredMessage,
		})
		flusher.Flush()
		return
	}

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	totalSites := d.Subtitle.ProviderCount()
	writeEvent := func(event map[string]any) bool {
		if err := writeSSEEvent(w, event); err != nil {
			cancel()
			return false
		}
		flusher.Flush()
		return true
	}

	if !writeEvent(map[string]any{
		"type":     "progress",
		"stage":    "searching",
		"value":    0,
		"text":     fmt.Sprintf("开始搜索字幕，共 %d 个字幕源 ...", totalSites),
		"items":    []any{},
		"finished": 0,
		"total":    totalSites,
	}) {
		return
	}

	finished, totalItems := 0, 0
	results, err := d.Subtitle.Search(ctx, query, func(evt subtitle.Event) {
		if ctx.Err() != nil {
			return
		}
		finished++
		totalItems += len(evt.Subtitles)
		payload := map[string]any{
			"type":        "append",
			"site":        evt.Site,
			"items":       evt.Subtitles,
			"finished":    finished,
			"total":       totalSites,
			"total_items": totalItems,
		}
		if evt.Subtitles == nil {
			payload["items"] = []any{}
		}
		if evt.Error != "" {
			payload["success"] = false
			payload["message"] = evt.Error
		}
		_ = writeEvent(payload)
	})
	if err != nil && !errors.Is(err, context.Canceled) {
		_ = writeEvent(map[string]any{"type": "error", "success": false, "message": err.Error()})
		return
	}
	if ctx.Err() != nil {
		return
	}
	_ = writeEvent(map[string]any{
		"type":        "done",
		"success":     true,
		"total_items": len(results),
	})
}

func writeSubtitleSSEError(w http.ResponseWriter, message string) {
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("X-Accel-Buffering", "no")
	_ = writeSSEEvent(w, map[string]any{"type": "error", "success": false, "message": message})
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}

type downloadSubtitleRequest struct {
	SubtitleIn *subtitle.Subtitle `json:"subtitle_in"`
	SavePath   string             `json:"save_path"`
	TMDBID     *int               `json:"tmdbid"`
	MediaID    string             `json:"media_id"`
	// 兼容直接扁平传入 Subtitle 字段
	Site       int    `json:"site"`
	SiteName   string `json:"site_name"`
	Title      string `json:"title"`
	Enclosure  string `json:"enclosure"`
	SubtitleID string `json:"subtitle_id"`
	FileName   string `json:"file_name"`
}

// downloadSubtitle 对应 V2 `POST /api/v1/download/subtitle`。
func (d Dependencies) downloadSubtitle(w http.ResponseWriter, r *http.Request) {
	var req downloadSubtitleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "字幕下载请求无效")
		return
	}
	var item subtitle.Subtitle
	if req.SubtitleIn != nil {
		item = *req.SubtitleIn
	} else {
		item = subtitle.Subtitle{
			Site:       req.Site,
			SiteName:   req.SiteName,
			Title:      req.Title,
			Enclosure:  req.Enclosure,
			SubtitleID: req.SubtitleID,
			FileName:   req.FileName,
		}
	}
	if strings.TrimSpace(item.Enclosure) == "" && strings.TrimSpace(item.SubtitleID) == "" {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "缺少字幕下载链接或字幕ID"})
		return
	}

	coord := d.Subtitle
	if coord == nil {
		coord = subtitle.NewCoordinator(nil, d.Config.SubtitleLanguages)
	}

	saveDir, err := d.resolveSubtitleSaveDir(req.SavePath)
	if err != nil {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: err.Error()})
		return
	}

	savedFiles, err := coord.Download(r.Context(), item, saveDir)
	if err != nil {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, Response{
		Success: true,
		Message: "字幕下载成功",
		Data:    map[string]any{"files": savedFiles},
	})
}

func (d Dependencies) resolveSubtitleSaveDir(requested string) (string, error) {
	savePath := strings.TrimSpace(requested)
	root := strings.TrimSpace(d.Config.StorageLocalPath)
	if savePath == "" {
		if root != "" {
			return filepath.Join(filepath.Clean(root), "subtitles"), nil
		}
		return filepath.Join(os.TempDir(), "moviepilot-subtitles"), nil
	}
	cleaned := filepath.Clean(savePath)
	if root != "" {
		cleanRoot := filepath.Clean(root)
		// 若 save_path 是相对路径，落到 StorageLocalPath 之下。
		if !filepath.IsAbs(cleaned) {
			cleaned = filepath.Join(cleanRoot, cleaned)
		}
		rel, err := filepath.Rel(cleanRoot, cleaned)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
			return "", errors.New("保存目录超出允许的本地存储根目录")
		}
	}
	// 如果调用方传入的是视频文件路径而非目录，取其父目录。
	if info, err := os.Stat(cleaned); err == nil && !info.IsDir() {
		return filepath.Dir(cleaned), nil
	}
	return cleaned, nil
}

func parseSubtitleTitleQuery(r *http.Request) (subtitle.Query, error) {
	q := r.URL.Query()
	keyword := strings.TrimSpace(q.Get("keyword"))
	if keyword == "" {
		return subtitle.Query{}, errors.New("搜索关键词不能为空")
	}
	siteIDs, err := parseCommaIntList(q.Get("sites"))
	if err != nil {
		return subtitle.Query{}, err
	}
	page := parseSearchPage(q.Get("page"))
	return subtitle.Query{
		Keyword: keyword,
		Page:    page,
		SiteIDs: siteIDs,
	}, nil
}

func (d Dependencies) parseSubtitleMediaQuery(r *http.Request) (subtitle.Query, error) {
	mediaID := strings.TrimSpace(chi.URLParam(r, "mediaid"))
	q := r.URL.Query()
	siteIDs, err := parseCommaIntList(q.Get("sites"))
	if err != nil {
		return subtitle.Query{}, err
	}
	query := subtitle.Query{
		MediaID:   mediaID,
		Keyword:   strings.TrimSpace(q.Get("title")),
		MediaType: strings.TrimSpace(q.Get("mtype")),
		Year:      strings.TrimSpace(q.Get("year")),
		SiteIDs:   siteIDs,
	}
	if rawSeason := strings.TrimSpace(q.Get("season")); rawSeason != "" {
		v, convErr := strconv.Atoi(rawSeason)
		if convErr != nil || v < 0 {
			return subtitle.Query{}, errors.New("season 参数无效")
		}
		query.Season = &v
	}
	if rawEpisode := strings.TrimSpace(q.Get("episode")); rawEpisode != "" {
		v, convErr := strconv.Atoi(rawEpisode)
		if convErr != nil || v < 0 {
			return subtitle.Query{}, errors.New("episode 参数无效")
		}
		query.Episode = &v
	}

	// 解析带来源前缀的 mediaid（tmdb:123 / themoviedb:123 / imdb:tt123 / tt123）
	lower := strings.ToLower(mediaID)
	switch {
	case strings.HasPrefix(lower, "tmdb:"):
		if id, err := strconv.Atoi(strings.TrimSpace(mediaID[5:])); err == nil && id > 0 {
			query.TMDBID = id
		}
	case strings.HasPrefix(lower, "themoviedb:"):
		if id, err := strconv.Atoi(strings.TrimSpace(mediaID[11:])); err == nil && id > 0 {
			query.TMDBID = id
		}
	case strings.HasPrefix(lower, "imdb:"):
		query.IMDBID = strings.TrimSpace(mediaID[5:])
	case strings.HasPrefix(lower, "tt"):
		query.IMDBID = mediaID
	default:
		if id, err := strconv.Atoi(mediaID); err == nil && id > 0 {
			query.TMDBID = id
		}
	}

	// 若未提供 title 且没有标准 TMDB/IMDB ID，退回把 mediaid 作为关键词搜索。
	if query.Keyword == "" && query.TMDBID == 0 && query.IMDBID == "" && mediaID != "" {
		query.Keyword = mediaID
	}
	return query, nil
}

func parseCommaIntList(raw string) ([]int, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, nil
	}
	parts := strings.Split(trimmed, ",")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		v, err := strconv.Atoi(p)
		if err != nil || v <= 0 {
			return nil, errors.New("sites 必须是逗号分隔的正整数")
		}
		out = append(out, v)
	}
	return out, nil
}

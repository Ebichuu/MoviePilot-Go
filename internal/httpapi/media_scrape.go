package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/moviepilot-go/moviepilot-go/internal/media"
	"github.com/moviepilot-go/moviepilot-go/internal/metadata"
	"github.com/moviepilot-go/moviepilot-go/internal/organization"
)

// ManualScraper 承载单文件或目录的元数据 NFO + 海报/背景图刮削能力。
type ManualScraper interface {
	ScrapeMovieNFO(ctx context.Context, tmdbID int, mediaPath string) error
	ScrapeMovieImages(ctx context.Context, tmdbID int, mediaPath string) error
	ScrapeTVShowNFO(ctx context.Context, tmdbID int, mediaPath string) error
	ScrapeTVShowImages(ctx context.Context, tmdbID int, mediaPath string) error
	ScrapeEpisodeNFO(ctx context.Context, tmdbID, season, episode int, mediaPath string) error
	ScrapeEpisodeImages(ctx context.Context, tmdbID, season, episode int, mediaPath string) error
}

// MediaServerRefresher 在手动整理/刮削后通知媒体服务器刷新媒体库。
type MediaServerRefresher interface {
	RefreshAllMediaServers(ctx context.Context) error
}

type scrapeFileItemRequest struct {
	Storage string `json:"storage"`
	Type    string `json:"type"`
	Name    string `json:"name"`
	Path    string `json:"path"`
}

// scrapeMedia 对应 V2 `POST /api/v1/media/scrape/{storage}`：
// 识别本地媒体文件/目录并写入 NFO 与海报/背景图。
func (d Dependencies) scrapeMedia(w http.ResponseWriter, r *http.Request) {
	storageName := strings.ToLower(strings.TrimSpace(chi.URLParam(r, "storage")))
	if storageName == "" {
		storageName = "local"
	}
	if storageName != "local" {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: fmt.Sprintf("暂不支持对 %s 存储执行刮削", storageName)})
		return
	}
	if d.Scraper == nil {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "未配置 TMDB 刮削服务，无法刮削媒体文件"})
		return
	}

	var item scrapeFileItemRequest
	if err := json.NewDecoder(r.Body).Decode(&item); err != nil {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "刮削请求 JSON 无效"})
		return
	}
	targetPath := strings.TrimSpace(item.Path)
	if targetPath == "" {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "刮削路径无效"})
		return
	}

	query := r.URL.Query()
	mediaSource := strings.ToLower(strings.TrimSpace(query.Get("media_source")))
	mediaIDStr := strings.TrimSpace(query.Get("media_id"))
	typeName := strings.TrimSpace(query.Get("type_name"))

	if mediaIDStr != "" && mediaSource == "" {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "指定媒体ID时必须同时指定媒体数据源"})
		return
	}
	var explicitTMDBID int
	if mediaIDStr != "" {
		parsedID, err := strconv.Atoi(mediaIDStr)
		if err != nil || parsedID <= 0 {
			writeJSON(w, http.StatusOK, Response{Success: false, Message: "媒体ID格式无效"})
			return
		}
		if mediaSource == "themoviedb" || mediaSource == "tmdb" {
			explicitTMDBID = parsedID
		}
	}

	info, err := os.Stat(targetPath)
	if err != nil {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "刮削路径不存在"})
		return
	}

	scrapedCount, resolvedID, scrapeErr := d.scrapeLocalPath(r.Context(), targetPath, info.IsDir(), typeName, explicitTMDBID)
	if scrapeErr != nil {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "刮削失败：" + scrapeErr.Error()})
		return
	}

	name := strings.TrimSpace(item.Name)
	if name == "" {
		name = filepath.Base(targetPath)
	}
	writeJSON(w, http.StatusOK, Response{
		Success: true,
		Message: name + " 刮削完成",
		Data: map[string]any{
			"path":    targetPath,
			"tmdb_id": resolvedID,
			"scraped": scrapedCount,
		},
	})
}

func (d Dependencies) scrapeLocalPath(
	ctx context.Context,
	targetPath string,
	isDir bool,
	typeHint string,
	explicitTMDBID int,
) (int, int, error) {
	parsed := media.Parse(filepath.Base(targetPath), media.Options{})
	mediaType := normalizeScrapeMediaType(typeHint, parsed.Type)

	tmdbID := explicitTMDBID
	if tmdbID <= 0 {
		id, resolvedType, err := d.resolveScrapeTMDBID(ctx, parsed, mediaType)
		if err != nil {
			return 0, 0, err
		}
		tmdbID = id
		if resolvedType != "" {
			mediaType = resolvedType
		}
	}

	if !isDir {
		if mediaType == "电视剧" {
			if err := d.Scraper.ScrapeTVShowNFO(ctx, tmdbID, filepath.Dir(targetPath)); err != nil {
				return 0, tmdbID, err
			}
			_ = d.Scraper.ScrapeTVShowImages(ctx, tmdbID, filepath.Dir(targetPath))
			season := max(parsed.BeginSeason, 1)
			if parsed.BeginEpisode > 0 {
				if err := d.Scraper.ScrapeEpisodeNFO(ctx, tmdbID, season, parsed.BeginEpisode, targetPath); err != nil {
					return 0, tmdbID, err
				}
				_ = d.Scraper.ScrapeEpisodeImages(ctx, tmdbID, season, parsed.BeginEpisode, targetPath)
			}
			return 1, tmdbID, nil
		}
		if err := d.Scraper.ScrapeMovieNFO(ctx, tmdbID, targetPath); err != nil {
			return 0, tmdbID, err
		}
		_ = d.Scraper.ScrapeMovieImages(ctx, tmdbID, targetPath)
		return 1, tmdbID, nil
	}

	// 目录模式：遍历目录内所有视频文件，电影按单文件刮削，剧集先写 tvshow.nfo 再逐集刮削。
	var videos []string
	_ = filepath.WalkDir(targetPath, func(p string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() {
			return nil
		}
		if organization.IsVideoFilename(entry.Name()) {
			videos = append(videos, p)
		}
		return nil
	})

	if mediaType == "电视剧" {
		if err := d.Scraper.ScrapeTVShowNFO(ctx, tmdbID, targetPath); err != nil {
			return 0, tmdbID, err
		}
		_ = d.Scraper.ScrapeTVShowImages(ctx, tmdbID, targetPath)
		count := 1
		for _, v := range videos {
			vp := media.Parse(filepath.Base(v), media.Options{})
			if vp.BeginEpisode <= 0 {
				continue
			}
			season := max(vp.BeginSeason, 1)
			if err := d.Scraper.ScrapeEpisodeNFO(ctx, tmdbID, season, vp.BeginEpisode, v); err == nil {
				_ = d.Scraper.ScrapeEpisodeImages(ctx, tmdbID, season, vp.BeginEpisode, v)
				count++
			}
		}
		return count, tmdbID, nil
	}

	if len(videos) == 0 {
		if err := d.Scraper.ScrapeMovieNFO(ctx, tmdbID, targetPath); err != nil {
			return 0, tmdbID, err
		}
		_ = d.Scraper.ScrapeMovieImages(ctx, tmdbID, targetPath)
		return 1, tmdbID, nil
	}

	count := 0
	for _, v := range videos {
		if err := d.Scraper.ScrapeMovieNFO(ctx, tmdbID, v); err != nil {
			return count, tmdbID, err
		}
		_ = d.Scraper.ScrapeMovieImages(ctx, tmdbID, v)
		count++
	}
	return count, tmdbID, nil
}

func (d Dependencies) resolveScrapeTMDBID(ctx context.Context, parsed media.ParseResult, mediaType string) (int, string, error) {
	if d.Metadata == nil {
		return 0, mediaType, fmt.Errorf("未提供 media_id 且元数据识别服务未初始化")
	}
	keyword := strings.TrimSpace(parsed.Name())
	if keyword == "" {
		return 0, mediaType, fmt.Errorf("无法从文件名解析媒体标题")
	}
	yearInt, _ := strconv.Atoi(strings.TrimSpace(parsed.Year))
	results, err := d.Metadata.Search(ctx, metadata.Query{
		Keyword: keyword,
		Year:    yearInt,
		Source:  "themoviedb",
	})
	if err != nil {
		return 0, mediaType, fmt.Errorf("查询元数据失败: %w", err)
	}
	for _, match := range results.Matches {
		item := match.Media
		if !strings.EqualFold(item.Source, "themoviedb") {
			continue
		}
		id, convErr := strconv.Atoi(strings.TrimSpace(item.ID))
		if convErr != nil || id <= 0 {
			continue
		}
		itemType := "电影"
		if strings.EqualFold(item.Type, "tv") || item.Type == "电视剧" {
			itemType = "电视剧"
		}
		if mediaType != "" && itemType != mediaType {
			continue
		}
		return id, itemType, nil
	}
	for _, match := range results.Matches {
		item := match.Media
		if !strings.EqualFold(item.Source, "themoviedb") {
			continue
		}
		if id, convErr := strconv.Atoi(strings.TrimSpace(item.ID)); convErr == nil && id > 0 {
			itemType := "电影"
			if strings.EqualFold(item.Type, "tv") || item.Type == "电视剧" {
				itemType = "电视剧"
			}
			return id, itemType, nil
		}
	}
	return 0, mediaType, fmt.Errorf("未能识别到对应的 TMDB 媒体信息")
}

func normalizeScrapeMediaType(hint string, parsed media.MediaType) string {
	switch strings.ToLower(strings.TrimSpace(hint)) {
	case "tv", "电视剧":
		return "电视剧"
	case "movie", "电影":
		return "电影"
	}
	if parsed == media.TypeTV {
		return "电视剧"
	}
	if parsed == media.TypeMovie {
		return "电影"
	}
	return ""
}

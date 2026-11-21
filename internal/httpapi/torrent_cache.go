package httpapi

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/moviepilot-go/moviepilot-go/internal/database"
	"github.com/moviepilot-go/moviepilot-go/internal/media"
	"github.com/moviepilot-go/moviepilot-go/internal/metadata"
)

// torrentCacheStore 是种子缓存管理所需的持久化子集，便于测试替身注入。
type torrentCacheStore interface {
	ListTorrentCache(ctx context.Context, limit int) ([]database.TorrentCacheItem, error)
	ClearTorrentCache(ctx context.Context) (int64, error)
	FindTorrentCacheItem(ctx context.Context, domain, torrentHash string) (database.TorrentCacheItem, bool, error)
	DeleteTorrentCacheItem(ctx context.Context, domain, torrentHash string) (bool, error)
	UpdateTorrentCacheRecognition(ctx context.Context, id int64, recognizedTitle string, failCount int) error
}

func (d Dependencies) torrentCacheRepo() torrentCacheStore {
	if d.TorrentCache != nil {
		return d.TorrentCache
	}
	if d.Store != nil {
		return d.Store
	}
	return nil
}

// listTorrentCache 对应 V2 `GET /api/v1/torrent/cache`：返回 `site_rss_items` 中的真实缓存种子列表。
func (d Dependencies) listTorrentCache(w http.ResponseWriter, r *http.Request) {
	repo := d.torrentCacheRepo()
	if repo == nil {
		torrentCacheList(w, r)
		return
	}
	items, err := repo.ListTorrentCache(r.Context(), 500)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "读取种子缓存失败")
		return
	}
	domains := make(map[string]struct{})
	data := make([]map[string]any, 0, len(items))
	for _, item := range items {
		domains[item.Domain] = struct{}{}
		parsed := media.Parse(item.Title, media.Options{})
		mediaName := strings.TrimSpace(item.RecognizedMediaTitle)
		if mediaName == "" {
			mediaName = strings.TrimSpace(parsed.Name())
		}
		seasonEp := strings.TrimSpace(parsed.Season() + parsed.Episode())
		pubdate := ""
		if !item.PublishedAt.IsZero() {
			pubdate = formatTorrentCacheTime(item.PublishedAt, d.Config.Location)
		}
		data = append(data, map[string]any{
			"hash":           item.Hash,
			"domain":         item.Domain,
			"title":          item.Title,
			"description":    item.Description,
			"size":           item.SizeBytes,
			"pubdate":        pubdate,
			"site_name":      item.SiteName,
			"media_name":     mediaName,
			"media_year":     parsed.Year,
			"media_type":     string(parsed.Type),
			"season_episode": seasonEp,
			"resource_term":  strings.TrimSpace(parsed.Resolution),
			"enclosure":      item.Enclosure,
			"page_url":       item.PageURL,
			"poster_path":    "",
			"backdrop_path":  "",
		})
	}
	writeJSON(w, http.StatusOK, Response{
		Success: true,
		Data: map[string]any{
			"count": len(data),
			"sites": len(domains),
			"data":  data,
		},
	})
}

// clearTorrentCache 对应 V2 `DELETE /api/v1/torrent/cache`：清空所有非处理中的缓存条目。
func (d Dependencies) clearTorrentCache(w http.ResponseWriter, r *http.Request) {
	repo := d.torrentCacheRepo()
	if repo == nil {
		successOnly(w, r)
		return
	}
	deleted, err := repo.ClearTorrentCache(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "清空种子缓存失败")
		return
	}
	writeJSON(w, http.StatusOK, Response{
		Success: true,
		Message: "种子缓存已清空",
		Data:    map[string]any{"deleted": deleted},
	})
}

// deleteTorrentCacheItem 对应 V2 `DELETE /api/v1/torrent/cache/{domain}/{torrent_hash}`。
func (d Dependencies) deleteTorrentCacheItem(w http.ResponseWriter, r *http.Request) {
	repo := d.torrentCacheRepo()
	if repo == nil {
		successOnly(w, r)
		return
	}
	domain := strings.TrimSpace(chi.URLParam(r, "domain"))
	hash := strings.TrimSpace(chi.URLParam(r, "torrent_hash"))
	deleted, err := repo.DeleteTorrentCacheItem(r.Context(), domain, hash)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "删除种子缓存失败")
		return
	}
	if !deleted {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "未找到缓存条目"})
		return
	}
	writeJSON(w, http.StatusOK, Response{Success: true, Message: "缓存条目已删除"})
}

// refreshTorrentCache 对应 V2 `POST /api/v1/torrent/cache/refresh`：触发一次 RSS 增量刷新。
func (d Dependencies) refreshTorrentCache(w http.ResponseWriter, r *http.Request) {
	if d.Scheduler != nil {
		if _, err := d.Scheduler.RunScheduler(r.Context(), "subscribe_refresh"); err != nil {
			writeJSON(w, http.StatusOK, Response{Success: false, Message: "刷新种子缓存失败"})
			return
		}
	}
	writeJSON(w, http.StatusOK, Response{Success: true, Message: "已触发种子缓存刷新"})
}

// reidentifyTorrentCacheItem 对应 V2 `POST /api/v1/torrent/cache/reidentify/{domain}/{torrent_hash}`：
// 重新识别指定缓存种子的媒体信息，并把识别结果写回 `site_rss_items`。
func (d Dependencies) reidentifyTorrentCacheItem(w http.ResponseWriter, r *http.Request) {
	repo := d.torrentCacheRepo()
	if repo == nil {
		torrentCacheReidentify(w, r)
		return
	}
	domain := strings.TrimSpace(chi.URLParam(r, "domain"))
	hash := strings.TrimSpace(chi.URLParam(r, "torrent_hash"))
	item, found, err := repo.FindTorrentCacheItem(r.Context(), domain, hash)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "查询种子缓存失败")
		return
	}
	if !found {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "未找到缓存条目"})
		return
	}

	query := r.URL.Query()
	titleHint := strings.TrimSpace(query.Get("title"))
	yearHint := strings.TrimSpace(query.Get("year"))

	parsed := media.Parse(item.Title, media.Options{})
	recognizedTitle := titleHint
	if recognizedTitle == "" {
		recognizedTitle = strings.TrimSpace(parsed.Name())
	}
	year := yearHint
	if year == "" {
		year = parsed.Year
	}

	var matched metadata.Media
	if d.Metadata != nil && recognizedTitle != "" {
		yearInt, _ := strconv.Atoi(strings.TrimSpace(year))
		if results, searchErr := d.Metadata.Search(r.Context(), metadata.Query{
			Keyword: recognizedTitle,
			Year:    yearInt,
		}); searchErr == nil && len(results.Matches) > 0 {
			matched = results.Matches[0].Media
			if strings.TrimSpace(matched.Title) != "" {
				recognizedTitle = strings.TrimSpace(matched.Title)
			}
			if matched.Year > 0 {
				year = strconv.Itoa(matched.Year)
			}
		}
	}

	failCount := 0
	if recognizedTitle == "" {
		failCount = 1
	}
	if err := repo.UpdateTorrentCacheRecognition(r.Context(), item.ID, recognizedTitle, failCount); err != nil {
		writeError(w, http.StatusInternalServerError, "更新种子识别结果失败")
		return
	}

	writeJSON(w, http.StatusOK, Response{
		Success: true,
		Message: "重新识别完成",
		Data: map[string]any{
			"hash":       item.Hash,
			"domain":     item.Domain,
			"media_name": recognizedTitle,
			"media_year": year,
			"media_type": string(parsed.Type),
			"media_id":   matched.ID,
			"source":     matched.Source,
		},
	})
}

func formatTorrentCacheTime(t time.Time, loc *time.Location) string {
	if loc == nil {
		loc = time.UTC
	}
	return t.In(loc).Format("2006-01-02 15:04:05")
}

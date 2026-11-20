package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/moviepilot-go/moviepilot-go/internal/indexer"
	"github.com/moviepilot-go/moviepilot-go/internal/media"
)

const searchSSEBatchSize = 48

func (d Dependencies) searchTitleStream(w http.ResponseWriter, r *http.Request) {
	d.streamSearch(w, r, strings.TrimSpace(r.URL.Query().Get("keyword")))
}

func (d Dependencies) searchMediaStream(w http.ResponseWriter, r *http.Request) {
	keyword := strings.TrimSpace(r.URL.Query().Get("title"))
	if keyword == "" {
		keyword = strings.TrimSpace(chi.URLParam(r, "mediaid"))
	}
	d.streamSearch(w, r, keyword)
}

func (d Dependencies) searchTitle(w http.ResponseWriter, r *http.Request) {
	d.searchJSON(w, r, strings.TrimSpace(r.URL.Query().Get("keyword")))
}

func (d Dependencies) searchMedia(w http.ResponseWriter, r *http.Request) {
	keyword := strings.TrimSpace(r.URL.Query().Get("title"))
	if keyword == "" {
		keyword = strings.TrimSpace(chi.URLParam(r, "mediaid"))
	}
	d.searchJSON(w, r, keyword)
}

func (d Dependencies) resolveResourceDownload(w http.ResponseWriter, r *http.Request) {
	siteID, err := strconv.Atoi(chi.URLParam(r, "siteid"))
	resourceID := strings.TrimSpace(chi.URLParam(r, "resourceid"))
	if err != nil || siteID <= 0 || resourceID == "" || len(resourceID) > 1024 {
		writeError(w, http.StatusBadRequest, "资源下载参数无效")
		return
	}
	var resolver indexer.DownloadResolver
	for _, adapter := range d.adapters() {
		identified, ok := adapter.(indexer.SiteIDAdapter)
		if !ok || identified.SiteID() != siteID {
			continue
		}
		resolver, _ = adapter.(indexer.DownloadResolver)
		break
	}
	if resolver == nil {
		writeError(w, http.StatusNotFound, "站点资源下载器不存在")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), d.Config.RequestTimeout)
	defer cancel()
	download, err := resolver.ResolveDownload(ctx, resourceID)
	if err != nil {
		writeError(w, http.StatusBadGateway, "获取站点资源失败")
		return
	}
	if len(download.Body) == 0 || int64(len(download.Body)) > d.Config.MaxResponseBody {
		writeError(w, http.StatusBadGateway, "站点资源大小无效")
		return
	}
	if err := validateTorrentMetainfo(download.Body); err != nil {
		writeError(w, http.StatusBadGateway, "downloaded resource is not valid torrent metainfo")
		return
	}
	contentType := download.ContentType
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	filename := strings.ReplaceAll(download.Filename, `"`, "")
	filename = strings.ReplaceAll(filename, "\\", "")
	filename = strings.ReplaceAll(filename, "/", "")
	filename = strings.ReplaceAll(filename, "\r", "")
	filename = strings.ReplaceAll(filename, "\n", "")
	if filename == "" {
		filename = "resource.bin"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Length", strconv.Itoa(len(download.Body)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(download.Body)
}

func (d Dependencies) streamSearch(w http.ResponseWriter, r *http.Request, keyword string) {
	adapters, err := selectSearchAdapters(d.adapters(), r.URL.Query().Get("sites"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	page := parseSearchPage(r.URL.Query().Get("page"))
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "当前响应不支持流式搜索")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	writeEvent := func(event map[string]any) bool {
		if err := writeSSEEvent(w, event); err != nil {
			cancel()
			return false
		}
		flusher.Flush()
		return true
	}
	if !writeEvent(map[string]any{
		"type": "progress", "stage": "searching", "value": 0,
		"text":  fmt.Sprintf("开始搜索，共 %d 个站点 ...", len(adapters)),
		"items": []any{}, "finished": 0, "total": len(adapters),
	}) {
		return
	}

	if d.Search == nil {
		writeEvent(searchDoneEvent(0))
		return
	}
	finished, totalItems := 0, 0
	resources, searchErr := d.Search.Search(ctx, adapters, indexer.Query{Keyword: keyword, Page: page, Limit: 100}, func(event indexer.Event) {
		if ctx.Err() != nil {
			return
		}
		finished++
		items := searchContexts(event.Resources, d.Config.Location)
		totalItems += len(items)
		if len(items) == 0 {
			payload := searchAppendEvent(event.Site, nil, finished, len(adapters), totalItems)
			if event.Error != "" {
				payload["message"] = event.Error
				payload["success"] = false
			}
			_ = writeEvent(payload)
			return
		}
		for start := 0; start < len(items); start += searchSSEBatchSize {
			end := min(start+searchSSEBatchSize, len(items))
			if !writeEvent(searchAppendEvent(event.Site, items[start:end], finished, len(adapters), totalItems)) {
				return
			}
		}
	})
	if searchErr != nil && !errors.Is(searchErr, context.Canceled) {
		writeEvent(map[string]any{"type": "error", "success": false, "message": searchErr.Error()})
		return
	}
	if ctx.Err() != nil {
		return
	}
	writeEvent(searchDoneEvent(len(resources)))
}

func (d Dependencies) searchJSON(w http.ResponseWriter, r *http.Request, keyword string) {
	adapters, err := selectSearchAdapters(d.adapters(), r.URL.Query().Get("sites"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if d.Search == nil {
		writeError(w, http.StatusOK, "未搜索到任何资源")
		return
	}
	resources, err := d.Search.Search(r.Context(), adapters, indexer.Query{
		Keyword: keyword, Page: parseSearchPage(r.URL.Query().Get("page")), Limit: 100,
	}, nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "搜索资源失败")
		return
	}
	if len(resources) == 0 {
		writeError(w, http.StatusOK, "未搜索到任何资源")
		return
	}
	writeSuccess(w, searchContexts(resources, d.Config.Location))
}

func (d Dependencies) adapters() []indexer.Adapter {
	if d.SearchRegistry != nil {
		return d.SearchRegistry.Snapshot()
	}
	return append([]indexer.Adapter(nil), d.SearchAdapters...)
}

func selectSearchAdapters(adapters []indexer.Adapter, sites string) ([]indexer.Adapter, error) {
	if strings.TrimSpace(sites) == "" {
		return adapters, nil
	}
	allowed := make(map[int]struct{})
	for _, raw := range strings.Split(sites, ",") {
		id, err := strconv.Atoi(strings.TrimSpace(raw))
		if err != nil || id <= 0 {
			return nil, errors.New("sites必须是逗号分隔的正整数")
		}
		allowed[id] = struct{}{}
	}
	selected := make([]indexer.Adapter, 0, len(adapters))
	for _, adapter := range adapters {
		identified, ok := adapter.(indexer.SiteIDAdapter)
		if !ok {
			continue
		}
		if _, ok := allowed[identified.SiteID()]; ok {
			selected = append(selected, adapter)
		}
	}
	return selected, nil
}

func parseSearchPage(value string) int {
	page, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || page < 0 {
		return 0
	}
	return page
}

func searchContexts(resources []indexer.Resource, location *time.Location) []map[string]any {
	items := make([]map[string]any, 0, len(resources))
	for _, resource := range resources {
		parsed := media.Parse(resource.Title, media.Options{})
		items = append(items, map[string]any{
			"meta_info": parsed,
			"torrent_info": map[string]any{
				"title": resource.Title, "description": resource.Description, "site": resource.SiteID, "site_name": resource.Site,
				// 凭据永远不投影到 V2 响应；下载通过服务端资源地址完成。
				"site_cookie": nil, "site_ua": nil, "site_proxy": false, "site_order": resource.SiteOrder, "site_downloader": nil,
				"page_url": resource.DetailURL, "enclosure": localResourceEnclosure(resource),
				"size": resource.SizeBytes, "seeders": resource.Seeders, "peers": resource.Peers, "grabs": resource.Grabs,
				"pubdate": publishedDate(resource.PublishedAt, location), "date_elapsed": elapsed(resource.PublishedAt),
				"imdbid": optionalText(resource.IMDBID), "labels": normalizedLabels(resource.Labels), "category": optionalText(resource.Category), "pri_order": resource.PriOrder,
				"freedate": publishedDate(resource.FreeDate, location), "freedate_diff": freedateDiff(resource.FreeDate, location), "volume_factor": volumeFactor(resource), "hit_and_run": resource.HitAndRun,
				"downloadvolumefactor": resource.DownloadVolumeFactor, "uploadvolumefactor": resource.UploadVolumeFactor,
			},
			"media_info": nil, "resource_source": "search", "match_source": "unknown",
			"candidate_recognized": false, "media_info_is_target": false,
		})
	}
	return items
}

func localResourceEnclosure(resource indexer.Resource) string {
	if resource.SiteID <= 0 || strings.TrimSpace(resource.DownloadID) == "" {
		return ""
	}
	return fmt.Sprintf("/api/v1/resource/download/%d/%s", resource.SiteID, url.PathEscape(strings.TrimSpace(resource.DownloadID)))
}

func publishedDate(value time.Time, location *time.Location) any {
	if value.IsZero() {
		return nil
	}
	if location == nil {
		location = time.UTC
	}
	return value.In(location).Format("2006-01-02 15:04:05")
}

func elapsed(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	minutes := int(time.Since(value).Minutes())
	if minutes < 0 {
		return ""
	}
	if minutes < 60 {
		return fmt.Sprintf("%d分钟", minutes)
	}
	hours := minutes / 60
	if hours < 24 {
		return fmt.Sprintf("%d小时%d分钟", hours, minutes%60)
	}
	return fmt.Sprintf("%d天%d小时", hours/24, hours%24)
}

func searchAppendEvent(site string, items []map[string]any, finished, total, totalItems int) map[string]any {
	if items == nil {
		items = []map[string]any{}
	}
	value := 100.0
	if total > 0 {
		value = float64(finished) / float64(total) * 100
	}
	return map[string]any{
		"type": "append", "stage": "searching", "value": value,
		"text":  fmt.Sprintf("正在搜索，已完成 %d / %d 个站点 ...", finished, total),
		"items": items, "site": site, "finished": finished, "total": total, "total_items": totalItems,
	}
}

func searchDoneEvent(total int) map[string]any {
	return map[string]any{
		"type": "done", "stage": "done", "value": 100,
		"text": fmt.Sprintf("搜索完成，共 %d 个资源", total), "items": []any{}, "total_items": total,
	}
}

func writeSSEEvent(w http.ResponseWriter, event map[string]any) error {
	payload, err := json.Marshal(event)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "data: %s\n\n", payload)
	return err
}

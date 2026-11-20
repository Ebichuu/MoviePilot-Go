package httpapi

import (
	"net/http"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/moviepilot-go/moviepilot-go/internal/database"
)

// transferHistoryItem 对应 V2 `schemas.TransferHistory`，覆盖成功与失败整理记录。
type transferHistoryItem struct {
	ID          int     `json:"id"`
	Src         *string `json:"src"`
	Dest        *string `json:"dest"`
	Mode        *string `json:"mode"`
	Type        string  `json:"type"`
	Category    string  `json:"category"`
	Title       string  `json:"title"`
	Year        string  `json:"year"`
	TMDBID      *int    `json:"tmdbid"`
	MediaSource string  `json:"media_source"`
	MediaID     string  `json:"media_id"`
	Seasons     string  `json:"seasons"`
	Episodes    string  `json:"episodes"`
	Image       string  `json:"image"`
	Status      bool    `json:"status"`
	ErrMsg      *string `json:"errmsg"`
	Date        string  `json:"date"`
	Username    string  `json:"username"`
}

// transferHistory 对应 V2 `GET /history/transfer`：合并成功快照与失败整理记录，
// 支持 `status=true/false` 与 `title=成功/失败/关键词` 筛选。
func (d Dependencies) transferHistory(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	page := parseHistoryInt(query.Get("page"), 1, 1, 1000)
	count := parseHistoryInt(query.Get("count"), 30, 1, 200)
	title := strings.TrimSpace(query.Get("title"))
	var statusFilter *bool
	if title == "成功" {
		title = ""
		ok := true
		statusFilter = &ok
	} else if title == "失败" {
		title = ""
		ok := false
		statusFilter = &ok
	}
	if raw := strings.TrimSpace(query.Get("status")); raw != "" {
		if wanted, err := strconv.ParseBool(raw); err == nil {
			statusFilter = &wanted
		}
	}
	if d.Store == nil {
		writeSuccess(w, map[string]any{"list": []transferHistoryItem{}, "total": 0})
		return
	}
	filter := database.TransferHistoryFilter{
		Title:  title,
		Status: statusFilter,
		Limit:  count,
		Offset: (page - 1) * count,
	}
	entries, err := d.Store.ListTransferHistoryFiltered(r.Context(), filter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "查询整理记录失败")
		return
	}
	total, err := d.Store.CountTransferHistoryFiltered(r.Context(), filter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "查询整理记录失败")
		return
	}
	items := make([]transferHistoryItem, 0, len(entries))
	for index, entry := range entries {
		id := (page-1)*count + index + 1
		if entry.Failed && entry.FileID > 0 {
			id = int(entry.FileID)
		}
		items = append(items, newTransferHistoryItem(entry, id))
	}
	writeSuccess(w, map[string]any{"list": items, "total": total})
}

func newTransferHistoryItem(entry database.TransferHistoryEntry, id int) transferHistoryItem {
	rawTitle := entry.TitleYear
	if entry.Failed && rawTitle == entry.SourcePath && entry.SourcePath != "" {
		rawTitle = path.Base(strings.ReplaceAll(entry.SourcePath, `\`, "/"))
	}
	title, year := splitTitleYear(rawTitle)
	item := transferHistoryItem{
		ID:          id,
		Src:         nullableNonEmptyString(entry.SourcePath),
		Dest:        nullableNonEmptyString(entry.TargetPath),
		Mode:        nullableNonEmptyString(entry.Mode),
		Type:        entry.MediaType,
		Category:    entry.Category,
		Title:       title,
		Year:        year,
		MediaSource: entry.MediaSource,
		MediaID:     entry.MediaID,
		Seasons:     transferHistorySeasons(entry),
		Episodes:    transferHistoryEpisodes(entry),
		Image:       entry.PosterURL,
		Status:      !entry.Failed,
		ErrMsg:      nullableNonEmptyString(entry.ErrMsg),
		Date:        entry.CreatedAt.Format("2006-01-02 15:04:05"),
		Username:    entry.Username,
	}
	if entry.MediaSource == "themoviedb" {
		if parsed, err := strconv.Atoi(strings.TrimSpace(entry.MediaID)); err == nil {
			item.TMDBID = &parsed
		}
	}
	return item
}

func nullableNonEmptyString(value string) *string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	return &value
}

// splitTitleYear 把 `沙丘 (2021)` 拆成标题与年份；没有年份后缀时原样返回。
func splitTitleYear(value string) (string, string) {
	value = strings.TrimSpace(value)
	open := strings.LastIndex(value, "(")
	if open < 0 || !strings.HasSuffix(value, ")") {
		return value, ""
	}
	year := strings.TrimSpace(value[open+1 : len(value)-1])
	if _, err := strconv.Atoi(year); err != nil || len(year) != 4 {
		return value, ""
	}
	return strings.TrimSpace(value[:open]), year
}

func transferHistorySeasons(entry database.TransferHistoryEntry) string {
	seasons := map[int]struct{}{}
	for _, fact := range entry.FileFacts {
		if fact.Season > 0 {
			seasons[fact.Season] = struct{}{}
		}
	}
	if len(seasons) == 0 {
		return ""
	}
	values := make([]int, 0, len(seasons))
	for season := range seasons {
		values = append(values, season)
	}
	sort.Ints(values)
	parts := make([]string, 0, len(values))
	for _, season := range values {
		parts = append(parts, "S"+pad2(season))
	}
	return strings.Join(parts, ",")
}

func transferHistoryEpisodes(entry database.TransferHistoryEntry) string {
	episodes := map[int]struct{}{}
	for _, fact := range entry.FileFacts {
		for _, episode := range fact.Episodes {
			if episode > 0 {
				episodes[episode] = struct{}{}
			}
		}
	}
	if len(episodes) == 0 {
		return ""
	}
	values := make([]int, 0, len(episodes))
	for episode := range episodes {
		values = append(values, episode)
	}
	sort.Ints(values)
	parts := make([]string, 0, len(values))
	for _, episode := range values {
		parts = append(parts, "E"+pad2(episode))
	}
	return strings.Join(parts, ",")
}

func pad2(value int) string {
	if value < 10 {
		return "0" + strconv.Itoa(value)
	}
	return strconv.Itoa(value)
}

func parseHistoryInt(raw string, fallback, min, max int) int {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	if value < min {
		return min
	}
	if value > max {
		return max
	}
	return value
}

// emptyTransferHistory 对应 V2 `GET /history/empty/transfer`。V2 会 truncate 整理历史表，
// 而本项目的整理成功历史直接来自 organization_success_snapshots —— 这些快照是整理流程的幂等
// 事实，清空会导致已整理文件被重复整理，因此这里明确拒绝而不是照搬删除。
func emptyTransferHistory(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, Response{
		Success: false, Message: "整理记录来自组织成功快照（幂等事实），不支持清空",
	})
}

// deleteTransferHistory 保留无依赖降级入口（供最小测试直接调用）。
func deleteTransferHistory(w http.ResponseWriter, r *http.Request) {
	(Dependencies{}).deleteTransferHistory(w, r)
}

// deleteTransferHistory 对应 V2 `DELETE /history/transfer`：
// 允许删除失败的整理文件记录（?id=<file_id>），同时继续保护不可删除的成功幂等快照。
func (d Dependencies) deleteTransferHistory(w http.ResponseWriter, r *http.Request) {
	if d.Store != nil {
		if rawID := strings.TrimSpace(r.URL.Query().Get("id")); rawID != "" {
			if fileID, err := strconv.ParseInt(rawID, 10, 64); err == nil && fileID > 0 {
				deleted, delErr := d.Store.DeleteFailedTransferHistory(r.Context(), fileID)
				if delErr != nil {
					writeError(w, http.StatusInternalServerError, "删除整理失败记录失败")
					return
				}
				if deleted {
					writeSuccess(w, map[string]any{"deleted": fileID})
					return
				}
			}
		}
	}
	writeJSON(w, http.StatusOK, Response{
		Success: false, Message: "整理记录来自组织成功快照（幂等事实），不支持删除单条记录",
	})
}

// deleteDownloadHistory 对应 V2 `DELETE /history/download`。本项目的下载历史直接来自
// 下载器里已完成的种子，没有独立的历史表，删除应到下载器侧操作。
func deleteDownloadHistory(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, Response{
		Success: false, Message: "下载历史来自下载器，请在下载器中删除任务",
	})
}

package httpapi

import (
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/moviepilot-go/moviepilot-go/internal/database"
)

// transferHistoryItem 是 V2 `schemas.TransferHistory` 的子集。只填充本仓库真实拥有的
// 字段（标题、类型、季集、时间、来源），其余保持 null，不让前端把猜测值当事实展示。
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
	Date        string  `json:"date"`
	Username    string  `json:"username"`
}

// transferHistory 对应 V2 `GET /history/transfer`：数据来自组织成功快照
// （`organization_success_snapshots`），只包含成功记录，因此 status=false 时为空。
func (d Dependencies) transferHistory(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	page := parseHistoryInt(query.Get("page"), 1, 1, 1000)
	count := parseHistoryInt(query.Get("count"), 30, 1, 200)
	title := strings.TrimSpace(query.Get("title"))
	if title == "成功" {
		title = ""
	} else if title == "失败" {
		writeSuccess(w, map[string]any{"list": []transferHistoryItem{}, "total": 0})
		return
	}
	if raw := strings.TrimSpace(query.Get("status")); raw != "" {
		if wanted, err := strconv.ParseBool(raw); err == nil && !wanted {
			writeSuccess(w, map[string]any{"list": []transferHistoryItem{}, "total": 0})
			return
		}
	}
	if d.Store == nil {
		writeSuccess(w, map[string]any{"list": []transferHistoryItem{}, "total": 0})
		return
	}
	entries, err := d.Store.ListTransferHistory(r.Context(), "", title, count, (page-1)*count)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "查询整理记录失败")
		return
	}
	total, err := d.Store.CountTransferHistory(r.Context(), "", title)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "查询整理记录失败")
		return
	}
	items := make([]transferHistoryItem, 0, len(entries))
	for index, entry := range entries {
		items = append(items, newTransferHistoryItem(entry, (page-1)*count+index+1))
	}
	writeSuccess(w, map[string]any{"list": items, "total": total})
}

func newTransferHistoryItem(entry database.TransferHistoryEntry, id int) transferHistoryItem {
	title, year := splitTitleYear(entry.TitleYear)
	item := transferHistoryItem{
		ID:          id,
		Type:        entry.MediaType,
		Category:    entry.Category,
		Title:       title,
		Year:        year,
		MediaSource: entry.MediaSource,
		MediaID:     entry.MediaID,
		Seasons:     transferHistorySeasons(entry),
		Episodes:    transferHistoryEpisodes(entry),
		Image:       entry.PosterURL,
		Status:      true,
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
	value, err := strconv.Atoi(strings.TrimSpace(raw))
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
// 而本项目的整理历史直接来自 organization_success_snapshots —— 这些快照是整理流程的幂等
// 事实，清空会导致已整理文件被重复整理，因此这里明确拒绝而不是照搬删除。
func emptyTransferHistory(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, Response{
		Success: false, Message: "整理记录来自组织成功快照（幂等事实），不支持清空",
	})
}

// deleteTransferHistory 对应 V2 `DELETE /history/transfer`，拒绝理由同上。
func deleteTransferHistory(w http.ResponseWriter, _ *http.Request) {
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

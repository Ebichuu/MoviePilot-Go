package httpapi

import (
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/moviepilot-go/moviepilot-go/internal/database"
	"github.com/moviepilot-go/moviepilot-go/internal/media"
)

// downloadHistoryItem 是 V2 `DownloadHistory` 的字段子集：本项目没有独立的下载历史表，
// 记录直接来自下载器里已完成的种子，因此站点、种子描述等只有入库时才有的字段留 null。
type downloadHistoryItem struct {
	ID           int     `json:"id"`
	Path         *string `json:"path"`
	Type         *string `json:"type"`
	Title        *string `json:"title"`
	Year         *string `json:"year"`
	Seasons      *string `json:"seasons"`
	Episodes     *string `json:"episodes"`
	DownloadHash *string `json:"download_hash"`
	TorrentName  *string `json:"torrent_name"`
	TorrentSite  *string `json:"torrent_site"`
	Username     *string `json:"username"`
	Date         *string `json:"date"`
	Note         any     `json:"note"`
	Size         int64   `json:"size"`
}

// historyStringPointer 返回字符串指针，用于 V2 的可空字段。
func historyStringPointer(value string) *string { return &value }

// downloadHistory 对应 V2 `GET /history/download`：按完成时间倒序分页返回下载记录。
func (d Dependencies) downloadHistory(w http.ResponseWriter, r *http.Request) {
	source, ok := d.DownloadTasks.(DownloadHistorySource)
	if !ok {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	query := r.URL.Query()
	page := parseHistoryInt(query.Get("page"), 1, 1, 1000)
	count := parseHistoryInt(query.Get("count"), 30, 1, 200)
	tasks, err := source.ListDownloadHistory(r.Context(), strings.TrimSpace(query.Get("name")))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "查询下载历史失败")
		return
	}
	owners := map[string]database.DownloadIntentOwner{}
	if store := d.downloadIntentOwnerStore(); store != nil {
		if labels := downloadTaskLabels(tasks); len(labels) > 0 {
			if resolved, err := store.ListDownloadIntentOwners(r.Context(), labels); err == nil {
				owners = resolved
			}
		}
	}
	sort.SliceStable(tasks, func(i, j int) bool {
		left, right := tasks[i].Task.CompletedAt, tasks[j].Task.CompletedAt
		if left.Equal(right) {
			return tasks[i].Task.Title < tasks[j].Task.Title
		}
		return left.After(right)
	})

	start := (page - 1) * count
	if start > len(tasks) {
		start = len(tasks)
	}
	items := make([]downloadHistoryItem, 0, count)
	for offset := start; offset < len(tasks) && len(items) < count; offset++ {
		items = append(items, newDownloadHistoryItem(offset+1, tasks[offset], owners))
	}
	writeJSON(w, http.StatusOK, items)
}

// newDownloadHistoryItem 把下载器任务投影成 V2 下载历史字段：标题/年份/季集来自
// 种子名解析，归属用户来自确定性标签对应的下载意图记录。
func newDownloadHistoryItem(id int, named NamedDownloadTask, owners map[string]database.DownloadIntentOwner) downloadHistoryItem {
	task := named.Task
	parsed := media.Parse(task.Title, media.Options{})
	item := downloadHistoryItem{ID: id, Size: task.SizeBytes}

	title := strings.TrimSpace(parsed.Name())
	if title == "" {
		title = strings.TrimSpace(task.Title)
	}
	if title != "" {
		item.Title = historyStringPointer(title)
	}
	if year := strings.TrimSpace(parsed.Year); year != "" {
		item.Year = historyStringPointer(year)
	}
	if mediaType := downloadHistoryMediaType(parsed, task.Title); mediaType != media.TypeUnknown {
		item.Type = historyStringPointer(string(mediaType))
	}
	if season := strings.TrimSpace(parsed.Season()); season != "" {
		item.Seasons = historyStringPointer(season)
	}
	if episode := strings.TrimSpace(parsed.Episode()); episode != "" {
		item.Episodes = historyStringPointer(episode)
	}
	path := strings.TrimSpace(task.ContentPath)
	if path == "" {
		path = strings.TrimSpace(task.SavePath)
	}
	if path != "" {
		item.Path = historyStringPointer(path)
	}
	if hash := strings.TrimSpace(task.Hash); hash != "" {
		item.DownloadHash = historyStringPointer(hash)
	}
	if name := strings.TrimSpace(task.Title); name != "" {
		item.TorrentName = historyStringPointer(name)
	}
	if owner, found := owners[strings.TrimSpace(task.Label)]; found {
		if username := strings.TrimSpace(owner.Username); username != "" {
			item.Username = historyStringPointer(username)
		}
	}
	if !task.CompletedAt.IsZero() {
		item.Date = historyStringPointer(task.CompletedAt.UTC().Format(time.RFC3339))
	}
	return item
}

// downloadHistoryTVPackRE 匹配「整季包」特征的种子名，用于在解析器给不出类型时
// 把整季资源归到电视剧。
var downloadHistoryTVPackRE = regexp.MustCompile(`(?i)(全集|全季|第[一二三四五六七八九十0-9]+季|complete|season\s*\d+)`)

// downloadHistoryMediaType 判定下载记录的类型。V2 的类型来自入库的订阅/下载记录，
// 本项目没有下载历史表，因此解析器给出结论时直接采用，否则用种子名做退让推断：
// 含整季特征按电视剧，其余按电影。
func downloadHistoryMediaType(parsed media.ParseResult, torrentName string) media.MediaType {
	if parsed.Type != media.TypeUnknown {
		return parsed.Type
	}
	if downloadHistoryTVPackRE.MatchString(torrentName) {
		return media.TypeTV
	}
	return media.TypeMovie
}

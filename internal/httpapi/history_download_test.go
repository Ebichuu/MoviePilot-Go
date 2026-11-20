package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/moviepilot-go/moviepilot-go/internal/database"
	"github.com/moviepilot-go/moviepilot-go/internal/downloader"
	"github.com/moviepilot-go/moviepilot-go/internal/media"
)

type fakeDownloadHistorySource struct {
	tasks     []NamedDownloadTask
	name      string
	err       error
	callCount int
}

func (s *fakeDownloadHistorySource) ListDownloading(context.Context, string) ([]NamedDownloadTask, error) {
	return nil, nil
}

func (s *fakeDownloadHistorySource) ListDownloadHistory(_ context.Context, name string) ([]NamedDownloadTask, error) {
	s.callCount++
	s.name = name
	return s.tasks, s.err
}

func downloadHistoryFixtureTasks() []NamedDownloadTask {
	newer := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	older := time.Date(2026, 1, 1, 20, 0, 0, 0, time.UTC)
	return []NamedDownloadTask{
		{
			Downloader: "qbittorrent",
			Task: downloader.Task{
				Title: "某剧.S01E02.2024.1080p.WEB-DL.x264", Hash: "hash-older", SizeBytes: 1000,
				Label: "moviepilot-1", SavePath: "/downloads/save1", ContentPath: "/downloads/content1",
				CompletedAt: older,
			},
		},
		{
			Downloader: "qbittorrent",
			Task: downloader.Task{
				Title: "某电影.2023.1080p.BluRay", Hash: "hash-newer", SizeBytes: 2000,
				Label: "moviepilot-2", SavePath: "/downloads/save2", CompletedAt: newer,
			},
		},
		{
			Downloader: "transmission",
			Task: downloader.Task{
				Title: "某剧 全集", Hash: "hash-pack", SizeBytes: 3000, Label: "moviepilot-unknown",
			},
		},
	}
}

func downloadHistoryOwners() *fakeDownloadIntentOwnerStore {
	return &fakeDownloadIntentOwnerStore{owners: map[string]database.DownloadIntentOwner{
		"moviepilot-1": {UserID: 1, Username: "bob"},
		"moviepilot-2": {UserID: 2, Username: "alice"},
	}}
}

func TestDownloadHistoryReturnsSortedProjection(t *testing.T) {
	source := &fakeDownloadHistorySource{tasks: downloadHistoryFixtureTasks()}
	deps := Dependencies{DownloadTasks: source, DownloadOwners: downloadHistoryOwners()}

	code, body := runRawHandler(t, deps.downloadHistory, http.MethodGet,
		"/history/download?page=1&count=2&name=%20qbittorrent%20", nil)
	if code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", code, body)
	}
	if !strings.EqualFold(source.name, "qbittorrent") {
		t.Fatalf("下载器名未透传: %q", source.name)
	}
	var items []downloadHistoryItem
	if err := json.Unmarshal(body, &items); err != nil {
		t.Fatalf("解析下载历史: %v; body=%s", err, body)
	}
	if len(items) != 2 {
		t.Fatalf("第一页条数 = %d, body=%s", len(items), body)
	}
	// 按完成时间倒序：新的在前。
	if items[0].ID != 1 || items[0].Title == nil || !strings.Contains(*items[0].Title, "某电影") {
		t.Fatalf("第一页首条 = %#v", items[0])
	}
	if items[1].ID != 2 || items[1].Title == nil || !strings.Contains(*items[1].Title, "某剧") {
		t.Fatalf("第一页次条 = %#v", items[1])
	}
	if items[0].Date == nil || *items[0].Date != "2026-01-02T03:04:05Z" {
		t.Fatalf("完成时间 = %v", items[0].Date)
	}
	if items[0].Path == nil || *items[0].Path != "/downloads/save2" {
		t.Fatalf("无内容路径时应回退保存路径: %v", items[0].Path)
	}
	if items[1].Path == nil || *items[1].Path != "/downloads/content1" {
		t.Fatalf("内容路径未优先: %v", items[1].Path)
	}
	if items[0].Username == nil || *items[0].Username != "alice" {
		t.Fatalf("归属用户 = %v", items[0].Username)
	}
	if items[1].Username == nil || *items[1].Username != "bob" {
		t.Fatalf("归属用户 = %v", items[1].Username)
	}
	if items[0].Type == nil || *items[0].Type != string(media.TypeMovie) {
		t.Fatalf("电影类型 = %v", items[0].Type)
	}

	// 第二页取到未完成时间的那条：整季包退让推断为电视剧，日期/路径留空。
	code, body = runRawHandler(t, deps.downloadHistory, http.MethodGet, "/history/download?page=2&count=2", nil)
	if code != http.StatusOK {
		t.Fatalf("第二页 status = %d, body=%s", code, body)
	}
	items = nil
	if err := json.Unmarshal(body, &items); err != nil {
		t.Fatalf("解析第二页: %v; body=%s", err, body)
	}
	if len(items) != 1 || items[0].ID != 3 {
		t.Fatalf("第二页 = %#v", items)
	}
	if items[0].Type == nil || *items[0].Type != string(media.TypeTV) {
		t.Fatalf("整季包类型 = %v", items[0].Type)
	}
	if items[0].Date != nil || items[0].Path != nil || items[0].Username != nil {
		t.Fatalf("零值字段应留空 = %#v", items[0])
	}

	// 越界页返回空数组而不是 null。
	code, body = runRawHandler(t, deps.downloadHistory, http.MethodGet, "/history/download?page=9&count=30", nil)
	if code != http.StatusOK {
		t.Fatalf("越界页 status = %d", code)
	}
	if strings.TrimSpace(string(body)) != "[]" {
		t.Fatalf("越界页 body = %s", body)
	}
}

func TestDownloadHistoryDegradesWithoutHistorySource(t *testing.T) {
	deps := Dependencies{DownloadTasks: fakeListOnlyDownloadSource{}}
	code, body := runRawHandler(t, deps.downloadHistory, http.MethodGet, "/history/download", nil)
	if code != http.StatusOK || strings.TrimSpace(string(body)) != "[]" {
		t.Fatalf("只读列表源 = (%d, %s)", code, body)
	}
}

func TestDownloadHistoryReportsSourceFailure(t *testing.T) {
	source := &fakeDownloadHistorySource{err: errors.New("下载器不可达")}
	deps := Dependencies{DownloadTasks: source}
	code, body := runRawHandler(t, deps.downloadHistory, http.MethodGet, "/history/download", nil)
	if code != http.StatusInternalServerError {
		t.Fatalf("status = %d, body=%s", code, body)
	}
	if !strings.Contains(string(body), "查询下载历史失败") {
		t.Fatalf("body = %s", body)
	}
	// 上游错误原文不能透出。
	if strings.Contains(string(body), "下载器不可达") {
		t.Fatalf("泄漏上游错误: %s", body)
	}
}

func TestNewDownloadHistoryItemFallbacks(t *testing.T) {
	// 空标题：解析器给不出名字时退回原始种子名。
	rawTitle := "某剧.S01E02.2024.1080p"
	item := newDownloadHistoryItem(7, NamedDownloadTask{Task: downloader.Task{
		Title: rawTitle, Hash: "hash", SizeBytes: 42, SavePath: "/downloads/save",
	}}, nil)
	if item.ID != 7 || item.Size != 42 {
		t.Fatalf("基础字段 = %#v", item)
	}
	if item.Title == nil || *item.Title == "" {
		t.Fatalf("标题为空 = %#v", item)
	}
	if item.TorrentName == nil || *item.TorrentName != rawTitle {
		t.Fatalf("种子名 = %v", item.TorrentName)
	}
	if item.DownloadHash == nil || *item.DownloadHash != "hash" {
		t.Fatalf("hash = %v", item.DownloadHash)
	}
	if item.Date != nil {
		t.Fatalf("零值完成时间应为 nil: %v", item.Date)
	}
	if item.Path == nil || *item.Path != "/downloads/save" {
		t.Fatalf("保存路径 = %v", item.Path)
	}

	// 完全空任务：除 ID/size 外全部可空。
	// 完全空任务：除了「按电影兜底」的类型与 ID/size 外全部可空。
	empty := newDownloadHistoryItem(1, NamedDownloadTask{}, nil)
	if empty.Type == nil || *empty.Type != string(media.TypeMovie) {
		t.Fatalf("空任务类型兜底 = %#v", empty.Type)
	}
	if empty.Title != nil || empty.Year != nil || empty.Path != nil ||
		empty.DownloadHash != nil || empty.TorrentName != nil || empty.Username != nil ||
		empty.Date != nil || empty.Seasons != nil || empty.Episodes != nil {
		t.Fatalf("空任务仍有非空字段 = %#v", empty)
	}

	// 解析器给了类型时直接采用。
	parsed := media.Parse("某剧.S02E03.2024.2160p", media.Options{})
	if got := downloadHistoryMediaType(parsed, "某剧.S02E03.2024.2160p"); got == media.TypeUnknown {
		t.Fatalf("已知类型不应为未知: %#v", parsed)
	}
}

func TestDownloadHistoryMediaTypeFallback(t *testing.T) {
	tests := []struct {
		name        string
		parsed      media.ParseResult
		torrentName string
		want        media.MediaType
	}{
		{name: "解析器结论优先", parsed: media.ParseResult{Type: media.TypeMovie}, torrentName: "某剧 全集", want: media.TypeMovie},
		{name: "全集按电视剧", parsed: media.ParseResult{Type: media.TypeUnknown}, torrentName: "某剧 全集", want: media.TypeTV},
		{name: "第N季按电视剧", parsed: media.ParseResult{Type: media.TypeUnknown}, torrentName: "某剧 第2季", want: media.TypeTV},
		{name: "中文第N季按电视剧", parsed: media.ParseResult{Type: media.TypeUnknown}, torrentName: "某剧 第二季", want: media.TypeTV},
		{name: "complete 按电视剧", parsed: media.ParseResult{Type: media.TypeUnknown}, torrentName: "Show Complete 1080p", want: media.TypeTV},
		{name: "其余按电影", parsed: media.ParseResult{Type: media.TypeUnknown}, torrentName: "Some.Movie.2024.1080p", want: media.TypeMovie},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := downloadHistoryMediaType(test.parsed, test.torrentName); got != test.want {
				t.Fatalf("downloadHistoryMediaType() = %q, want %q", got, test.want)
			}
		})
	}
}

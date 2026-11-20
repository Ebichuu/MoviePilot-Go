package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/moviepilot-go/moviepilot-go/internal/auth"
	"github.com/moviepilot-go/moviepilot-go/internal/database"
	"github.com/moviepilot-go/moviepilot-go/internal/downloader"
)

type downloadUserLookupFake struct {
	user auth.User
	err  error
}

func (f downloadUserLookupFake) UserByID(context.Context, int64) (auth.User, error) {
	return f.user, f.err
}

type downloadTaskSourceFake struct {
	tasks    []NamedDownloadTask
	err      error
	lastName string
}

func (f *downloadTaskSourceFake) ListDownloading(_ context.Context, name string) ([]NamedDownloadTask, error) {
	f.lastName = name
	return f.tasks, f.err
}

type downloadOwnerStoreFake struct {
	owners map[string]database.DownloadIntentOwner
	err    error
	labels []string
}

func (f *downloadOwnerStoreFake) ListDownloadIntentOwners(
	_ context.Context, labels []string,
) (map[string]database.DownloadIntentOwner, error) {
	f.labels = labels
	return f.owners, f.err
}

// TestListDownloadsBranches 覆盖下载列表的鉴权、查询失败、归属过滤三条主线。
func TestListDownloadsBranches(t *testing.T) {
	// 没有登录态：403。
	recorder := httptest.NewRecorder()
	Dependencies{}.listDownloads(recorder, httptest.NewRequest(http.MethodGet, "/download/", nil))
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("无登录态 = %d", recorder.Code)
	}

	alice := auth.User{ID: 1, Username: "alice"}
	deps := Dependencies{
		Users: downloadUserLookupFake{user: alice},
	}
	recorder = httptest.NewRecorder()
	deps.listDownloads(recorder, downloadHTTPRequest("", "alice"))
	if recorder.Code != http.StatusInternalServerError ||
		!strings.Contains(recorder.Body.String(), "下载任务查询暂不可用") {
		t.Fatalf("无任务源 = %d %s", recorder.Code, recorder.Body.String())
	}

	// 任务源报错。
	source := &downloadTaskSourceFake{err: errors.New("boom")}
	deps.DownloadTasks = source
	recorder = httptest.NewRecorder()
	deps.listDownloads(recorder, downloadHTTPRequest("", "alice"))
	if recorder.Code != http.StatusInternalServerError || !strings.Contains(recorder.Body.String(), "查询下载任务失败") {
		t.Fatalf("任务源报错 = %d %s", recorder.Code, recorder.Body.String())
	}

	// 有标签但没有归属存储。
	source = &downloadTaskSourceFake{tasks: []NamedDownloadTask{
		{Downloader: "QB", Task: downloader.Task{Label: "lbl", Title: "某剧.S01E01"}},
	}}
	deps.DownloadTasks = source
	recorder = httptest.NewRecorder()
	deps.listDownloads(recorder, downloadHTTPRequest("", "alice"))
	if recorder.Code != http.StatusInternalServerError || !strings.Contains(recorder.Body.String(), "查询下载任务失败") {
		t.Fatalf("缺归属存储 = %d %s", recorder.Code, recorder.Body.String())
	}

	// 归属查询报错。
	deps.DownloadOwners = &downloadOwnerStoreFake{err: errors.New("boom")}
	recorder = httptest.NewRecorder()
	deps.listDownloads(recorder, downloadHTTPRequest("", "alice"))
	if recorder.Code != http.StatusInternalServerError || !strings.Contains(recorder.Body.String(), "查询下载任务失败") {
		t.Fatalf("归属查询报错 = %d %s", recorder.Code, recorder.Body.String())
	}

	// 归属过滤：alice 只看到自己的任务，bob 的任务被跳过；
	// 没有归属记录（标签缺失或未登记）的任务也跳过。
	owners := &downloadOwnerStoreFake{owners: map[string]database.DownloadIntentOwner{
		"alice-lbl": {UserID: 1, Username: "alice"},
		"bob-lbl":   {UserID: 2, Username: "bob"},
	}}
	deps.DownloadOwners = owners
	deps.DownloadTasks = &downloadTaskSourceFake{tasks: []NamedDownloadTask{
		{Downloader: "QB", Task: downloader.Task{Label: "alice-lbl", Title: "爱丽丝的剧.S01E01", ProgressPercent: 42.5}},
		{Downloader: "QB", Task: downloader.Task{Label: "bob-lbl", Title: "鲍勃的剧.S01E01"}},
		{Downloader: "QB", Task: downloader.Task{Title: "无标签任务"}},
	}}
	recorder = httptest.NewRecorder()
	deps.listDownloads(recorder, downloadHTTPRequest("", "alice"))
	body := recorder.Body.String()
	if recorder.Code != http.StatusOK || !strings.Contains(body, "爱丽丝的剧.S01E01") ||
		strings.Contains(body, "鲍勃的剧.S01E01") || strings.Contains(body, "无标签任务") {
		t.Fatalf("归属过滤 = %d %s", recorder.Code, body)
	}
	if !strings.Contains(body, `"progress":42.5`) || !strings.Contains(body, `"state":"downloading"`) {
		t.Fatalf("任务投影 = %s", body)
	}
	if len(owners.labels) != 2 {
		t.Fatalf("归属查询标签 = %v", owners.labels)
	}

	// 超级管理员看到所有人的任务。
	deps.Users = downloadUserLookupFake{user: auth.User{ID: 1, Username: "alice", SuperUser: true}}
	recorder = httptest.NewRecorder()
	deps.listDownloads(recorder, downloadHTTPRequest("", "alice"))
	body = recorder.Body.String()
	if !strings.Contains(body, "爱丽丝的剧.S01E01") || !strings.Contains(body, "鲍勃的剧.S01E01") {
		t.Fatalf("超管视图 = %s", body)
	}

	// 用户查询失败：403。
	deps.Users = downloadUserLookupFake{err: errors.New("boom")}
	recorder = httptest.NewRecorder()
	deps.listDownloads(recorder, downloadHTTPRequest("", "alice"))
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("用户查询失败 = %d", recorder.Code)
	}
}

// TestFormatDownloadHelpers 覆盖速度/时长/标签投影的边界。
func TestFormatDownloadHelpers(t *testing.T) {
	if value := formatDownloadSpeed(0); value == "" {
		t.Fatal("零速度不应为空")
	}
	if value := formatDownloadSpeed(1536); !strings.Contains(value, "K") {
		t.Fatalf("KB 速度 = %q", value)
	}
	if value := formatDownloadSpeed(5 << 20); !strings.Contains(value, "M") {
		t.Fatalf("MB 速度 = %q", value)
	}
	if value := formatDownloadSpeed(3 << 30); !strings.Contains(value, "G") {
		t.Fatalf("GB 速度 = %q", value)
	}
	if value := formatDownloadDuration(-5); value != "" {
		t.Fatalf("负时长 = %q", value)
	}
	if value := formatDownloadDuration(0); value != "" {
		t.Fatalf("零时长 = %q", value)
	}
	if value := formatDownloadDuration(3661); value != "1时1分1秒" {
		t.Fatalf("时长 = %q", value)
	}

	if tags := publicDownloadTags("", ""); tags != "" {
		t.Fatalf("空标签 = %q", tags)
	}
	if tags := publicDownloadTags("下载, 电影, 内部, ", "内部"); tags != "下载,电影" {
		t.Fatalf("公开标签 = %q", tags)
	}
	if isInternalDownloadTag("内部") || isInternalDownloadTag("mp-短") {
		t.Fatal("内部标签判定错误")
	}
	if !isInternalDownloadTag("mp-0123456789abcdef0123456789abcdef") {
		t.Fatal("合法内部标签未被识别")
	}
	if isInternalDownloadTag("mp-0123456789abcdef0123456789abcdeZ") {
		t.Fatal("非十六进制内部标签被误判")
	}

	if labels := downloadTaskLabels([]NamedDownloadTask{
		{Task: downloader.Task{Label: " a "}},
		{Task: downloader.Task{Label: ""}},
		{Task: downloader.Task{Label: "a"}},
	}); len(labels) != 1 || labels[0] != "a" {
		t.Fatalf("标签去重 = %v", labels)
	}
}

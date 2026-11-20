package httpapi

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/moviepilot-go/moviepilot-go/internal/command"
	"github.com/moviepilot-go/moviepilot-go/internal/database"
	"github.com/moviepilot-go/moviepilot-go/internal/doctor"
)

type fakeDownloadTasks struct {
	tasks []NamedDownloadTask
	err   error
}

func (f fakeDownloadTasks) ListDownloading(context.Context, string) ([]NamedDownloadTask, error) {
	return f.tasks, f.err
}

// 只实现被命令层用到的方法：其余方法内嵌 nil 接口，一旦被误用会立刻 panic。
type commandSiteStore struct {
	SiteStore
	sites []database.Site
	err   error
}

func (f commandSiteStore) ListSites(context.Context) ([]database.Site, error) {
	return f.sites, f.err
}

func TestRunDoctorCommandRequiresRunner(t *testing.T) {
	if _, err := runDoctorCommand(context.Background(), nil); err == nil {
		t.Fatal("没有自检器时应返回错误，而不是伪造通过")
	}
}

func TestRunDoctorCommandRendersChecks(t *testing.T) {
	result, err := runDoctorCommand(context.Background(), &doctor.Runner{})
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	// 一项都没接入时，「没有 fail」不等于「通过」：必须说清什么都没验证到。
	if !strings.Contains(result.Text, "自检未完成") {
		t.Fatalf("全部跳过时应报告未完成，实际 %q", result.Text)
	}
	if strings.Contains(result.Text, "自检通过") {
		t.Fatalf("全部跳过时不能报告通过: %q", result.Text)
	}
	if !strings.Contains(result.Text, "· [") {
		t.Fatalf("结果应逐项列出检查项: %q", result.Text)
	}
	if _, ok := result.Details["report"]; !ok {
		t.Fatalf("结果应带完整报告: %+v", result.Details)
	}

	// 至少有一项真正通过时，才允许说「通过」。
	passing := &doctor.Runner{Probes: doctor.Probes{Environment: "test"}}
	result, err = runDoctorCommand(context.Background(), passing)
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if !strings.Contains(result.Text, "自检通过") {
		t.Fatalf("有 ok 项时应报告通过，实际 %q", result.Text)
	}
}

func TestRunDownloadsCommandRequiresSource(t *testing.T) {
	if _, err := (Dependencies{}).runDownloadsCommand(context.Background(), command.Request{}); err == nil {
		t.Fatal("没有下载任务源时应返回错误")
	}
}

func TestRunDownloadsCommandAggregatesByDownloader(t *testing.T) {
	deps := Dependencies{DownloadTasks: fakeDownloadTasks{tasks: []NamedDownloadTask{
		{Downloader: "qbittorrent"},
		{Downloader: "qbittorrent"},
		{Downloader: "transmission"},
	}}}
	result, err := deps.runDownloadsCommand(context.Background(), command.Request{Username: "admin"})
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	want := "正在下载 3 个任务，qbittorrent 2，transmission 1"
	if result.Text != want {
		t.Fatalf("文本 = %q，期望 %q", result.Text, want)
	}
	if total, _ := result.Details["total"].(int); total != 3 {
		t.Fatalf("total = %#v", result.Details["total"])
	}

	// 空列表也要给出可读结论，而不是空字符串。
	deps = Dependencies{DownloadTasks: fakeDownloadTasks{}}
	result, err = deps.runDownloadsCommand(context.Background(), command.Request{})
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if result.Text != "正在下载 0 个任务" {
		t.Fatalf("空列表文本 = %q", result.Text)
	}
}

func TestRunDownloadsCommandWrapsSourceError(t *testing.T) {
	deps := Dependencies{DownloadTasks: fakeDownloadTasks{err: errors.New("boom")}}
	_, err := deps.runDownloadsCommand(context.Background(), command.Request{})
	if err == nil || !strings.Contains(err.Error(), "查询下载任务失败") {
		t.Fatalf("错误应带上查询上下文，实际 %v", err)
	}
}

func TestRunSitesCommandRequiresStore(t *testing.T) {
	if _, err := (Dependencies{}).runSitesCommand(context.Background()); err == nil {
		t.Fatal("没有站点存储时应返回错误")
	}
}

func TestRunSitesCommandSummarizesSites(t *testing.T) {
	deps := Dependencies{Sites: commandSiteStore{sites: []database.Site{
		{Name: "A", Enabled: true},
		{Name: "B", Enabled: false},
		{Name: "C", Enabled: true},
	}}}
	result, err := deps.runSitesCommand(context.Background())
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if !strings.Contains(result.Text, "共 3 个站点，启用 2 个") {
		t.Fatalf("统计文本不对: %q", result.Text)
	}
	if !strings.Contains(result.Text, "未启用：B") {
		t.Fatalf("应点名未启用的站点: %q", result.Text)
	}
	if enabled, _ := result.Details["enabled"].(int); enabled != 2 {
		t.Fatalf("enabled = %#v", result.Details["enabled"])
	}
	disabled, _ := result.Details["disabled"].([]string)
	if len(disabled) != 1 || disabled[0] != "B" {
		t.Fatalf("disabled = %#v", result.Details["disabled"])
	}
}

func TestRunSitesCommandCapsListedLines(t *testing.T) {
	sites := make([]database.Site, 0, 14)
	for index := 0; index < 14; index++ {
		sites = append(sites, database.Site{Name: string(rune('a' + index)), Enabled: true})
	}
	result, err := Dependencies{Sites: commandSiteStore{sites: sites}}.runSitesCommand(context.Background())
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if got := strings.Count(result.Text, "· "); got != 10 {
		t.Fatalf("明细最多列 10 行（避免刷屏），实际 %d", got)
	}
}

func TestRunSitesCommandWrapsStoreError(t *testing.T) {
	deps := Dependencies{Sites: commandSiteStore{err: errors.New("boom")}}
	_, err := deps.runSitesCommand(context.Background())
	if err == nil || !strings.Contains(err.Error(), "查询站点失败") {
		t.Fatalf("错误应带上查询上下文，实际 %v", err)
	}
}

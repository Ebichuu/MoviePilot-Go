package jobs

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/riverqueue/river"
)

// fakeSiteSigninHandler 让签到 worker 的可测部分不依赖真实站点。
type fakeSiteSigninHandler struct {
	report SiteSigninReport
	err    error
}

func (f fakeSiteSigninHandler) SigninEnabledSites(context.Context) (SiteSigninReport, error) {
	return f.report, f.err
}

func TestSiteSigninSweepWorkerRunsHandler(t *testing.T) {
	handler := fakeSiteSigninHandler{report: SiteSigninReport{
		Signed: 1, Skipped: 1, Failed: 1,
		Items: []SiteSigninResult{
			{SiteID: 1, Site: "ok", Status: "signed"},
			{SiteID: 2, Site: "skip", Status: "skipped", Reason: "未配置登录凭据"},
			{SiteID: 3, Site: "bad", Status: "failed", Reason: "登录被拒绝"},
		},
	}}
	worker := &siteSigninSweepWorker{handler: handler}
	if err := worker.Work(context.Background(), &river.Job[SiteSigninSweepArgs]{}); err != nil {
		t.Fatalf("签到任务不应因单站点失败而报错: %v", err)
	}
}

func TestSiteSigninSweepWorkerPropagatesChannelError(t *testing.T) {
	worker := &siteSigninSweepWorker{handler: fakeSiteSigninHandler{err: context.DeadlineExceeded}}
	if err := worker.Work(context.Background(), &river.Job[SiteSigninSweepArgs]{}); err == nil {
		t.Fatal("通道级错误应返回给 River 以便重试")
	}
}

func TestRegisterSiteSigninSweepWorkerSkipsWithoutHandler(t *testing.T) {
	enabled, err := registerSiteSigninSweepWorker(river.NewWorkers(), nil, nil)
	if err != nil {
		t.Fatalf("注册失败: %v", err)
	}
	if enabled {
		t.Fatal("handler 为 nil 时不应注册周期任务")
	}
}

func TestBackupKeepFallsBackToSeven(t *testing.T) {
	cases := map[string]int{
		"": 7, "0": 7, "-3": 7, "abc": 7, "1": 1, "30": 30,
	}
	for input, want := range cases {
		if got := backupKeep(input); got != want {
			t.Fatalf("backupKeep(%q) = %d，期望 %d", input, got, want)
		}
	}
}

func TestPruneBackupsKeepsNewest(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{
		"settings-20260101-000000.json",
		"settings-20260102-000000.json",
		"settings-20260103-000000.json",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("{}"), 0o600); err != nil {
			t.Fatalf("写备份文件失败: %v", err)
		}
	}
	pruneBackups(dir, 2)

	matches, err := filepath.Glob(filepath.Join(dir, backupFilePrefix+"*.json"))
	if err != nil {
		t.Fatalf("列目录失败: %v", err)
	}
	if len(matches) != 2 {
		t.Fatalf("保留 %d 份，期望 2 份: %v", len(matches), matches)
	}
	for _, path := range matches {
		if filepath.Base(path) == "settings-20260101-000000.json" {
			t.Fatal("最旧的备份应被清理")
		}
	}
}

func TestCleanupSweepPeriodicJobHasStableID(t *testing.T) {
	job := cleanupSweepPeriodicJob()
	if job == nil {
		t.Fatal("清理周期任务不应为 nil")
	}
	if cleanupSweepPeriodicID == "" || backupSweepPeriodicID == "" || siteSigninSweepPeriodicID == "" {
		t.Fatal("周期任务 ID 不能为空，否则重启会重复注册")
	}
}

func TestMaintenanceQueuesAreDistinct(t *testing.T) {
	queues := map[string]struct{}{
		queueCleanupSweep:    {},
		queueBackupSweep:     {},
		queueSiteSigninSweep: {},
	}
	if len(queues) != 3 {
		t.Fatal("维护类任务必须各用一条队列，避免互相阻塞")
	}
	if cleanupSweepInterval != 6*time.Hour || backupSweepInterval != 24*time.Hour || siteSigninSweepInterval != 24*time.Hour {
		t.Fatal("维护任务的周期与文档不一致")
	}
}

package jobs

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// 本文件覆盖签到/备份周期的热更新：插件配置改了间隔要能换掉周期任务，
// 但不能重启 River 客户端、也不能因为换周期而多跑一轮。

func TestSiteSigninSweepIntervalHotUpdate(t *testing.T) {
	bundle := newFakePeriodicJobBundle(7)
	manager := &Manager{
		siteSigninSweepEnabled:   true,
		siteSigninPeriodicJobs:   bundle,
		siteSigninPeriodicHandle: 7,
	}

	if err := manager.SetSiteSigninSweepInterval(6 * time.Hour); err != nil {
		t.Fatalf("error = %v", err)
	}
	active, removed := bundle.snapshot()
	if len(removed) != 1 || removed[0] != 7 {
		t.Fatalf("旧句柄应被移除: %v", removed)
	}
	if len(active) != 1 {
		t.Fatalf("应当只剩一个新的周期任务: %d", len(active))
	}
	if _, reused := active[7]; reused {
		t.Fatalf("旧句柄不应继续存活")
	}
}

func TestSiteSigninSweepIntervalGuards(t *testing.T) {
	if err := (&Manager{}).SetSiteSigninSweepInterval(time.Hour); !errors.Is(err, ErrSiteSigninSweepUnavailable) {
		t.Fatalf("未启用 error = %v", err)
	}
	if err := (*Manager)(nil).SetSiteSigninSweepInterval(time.Hour); !errors.Is(err, ErrSiteSigninSweepUnavailable) {
		t.Fatalf("nil manager error = %v", err)
	}

	bundle := newFakePeriodicJobBundle(1)
	bundle.addErr = errors.New("river 拒绝")
	manager := &Manager{siteSigninSweepEnabled: true, siteSigninPeriodicJobs: bundle}
	err := manager.SetSiteSigninSweepInterval(time.Hour)
	if err == nil || !strings.Contains(err.Error(), "更新站点签到周期任务") {
		t.Fatalf("error = %v", err)
	}
	// 注册失败时不能移除旧句柄，否则签到会彻底停摆。
	if _, removed := bundle.snapshot(); len(removed) != 0 {
		t.Fatalf("失败时不应移除旧句柄: %v", removed)
	}
}

func TestBackupSweepIntervalHotUpdate(t *testing.T) {
	bundle := newFakePeriodicJobBundle(3)
	manager := &Manager{backupSweepEnabled: true, backupPeriodicJobs: bundle, backupPeriodicHandle: 3}
	if err := manager.SetBackupSweepInterval(12 * time.Hour); err != nil {
		t.Fatalf("error = %v", err)
	}
	active, removed := bundle.snapshot()
	if len(removed) != 1 || removed[0] != 3 {
		t.Fatalf("旧句柄应被移除: %v", removed)
	}
	if len(active) != 1 {
		t.Fatalf("应当只剩一个新的周期任务: %d", len(active))
	}

	if err := (&Manager{}).SetBackupSweepInterval(time.Hour); !errors.Is(err, ErrBackupSweepUnavailable) {
		t.Fatalf("未启用 error = %v", err)
	}

	failing := newFakePeriodicJobBundle(5)
	failing.addErr = errors.New("river 拒绝")
	broken := &Manager{backupSweepEnabled: true, backupPeriodicJobs: failing}
	if err := broken.SetBackupSweepInterval(time.Hour); err == nil || !strings.Contains(err.Error(), "更新备份周期任务") {
		t.Fatalf("error = %v", err)
	}
}

func TestPluginSweepIntervalNormalization(t *testing.T) {
	cases := []struct {
		name  string
		input time.Duration
		want  time.Duration
	}{
		{name: "零值取下限", input: 0, want: time.Hour},
		{name: "负值取下限", input: -time.Hour, want: time.Hour},
		{name: "半小时抬到一小时", input: 30 * time.Minute, want: time.Hour},
		{name: "正常值保留", input: 6 * time.Hour, want: 6 * time.Hour},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if got := normalizeSiteSigninSweepInterval(test.input); got != test.want {
				t.Fatalf("签到 %v = %v, want %v", test.input, got, test.want)
			}
			if got := normalizeBackupSweepInterval(test.input); got != test.want {
				t.Fatalf("备份 %v = %v, want %v", test.input, got, test.want)
			}
		})
	}

	if siteSigninRuntimePeriodicJob(time.Hour) == nil || backupRuntimePeriodicJob(time.Hour) == nil {
		t.Fatalf("热更新用的周期任务不应为空")
	}
	// 热更新任务不能带固定 ID：River 的 Remove 不释放 ID 唯一性。
	if backupSweepPeriodicJob() == nil || siteSigninSweepPeriodicJob() == nil {
		t.Fatalf("启动期周期任务不应为空")
	}
}

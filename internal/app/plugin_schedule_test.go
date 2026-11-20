package app

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/moviepilot-go/moviepilot-go/internal/jobs"
	"github.com/moviepilot-go/moviepilot-go/internal/pluginengine"
)

// 本文件覆盖「插件配置 → 周期任务」这一段：只有签到和备份的间隔由插件管，
// 其它插件不该悄悄改别人的调度。

type scheduleCall struct {
	kind     string
	interval time.Duration
}

type fakePluginScheduler struct {
	calls []scheduleCall
	err   error
}

func (f *fakePluginScheduler) record(kind string, interval time.Duration) error {
	f.calls = append(f.calls, scheduleCall{kind: kind, interval: interval})
	return f.err
}

func (f *fakePluginScheduler) SetSiteSigninSweepInterval(interval time.Duration) error {
	return f.record("signin", interval)
}

func (f *fakePluginScheduler) SetBackupSweepInterval(interval time.Duration) error {
	return f.record("backup", interval)
}

func TestPluginScheduleListenerMapsIntervals(t *testing.T) {
	scheduler := &fakePluginScheduler{}
	listener := pluginScheduleListener(scheduler, nil)
	if listener == nil {
		t.Fatal("有调度器时回调不应为空")
	}
	ctx := context.Background()

	listener(ctx, pluginengine.PluginSiteSignIn, pluginengine.Config{"interval_hours": int64(6)})
	listener(ctx, pluginengine.PluginCloudBackup, pluginengine.Config{"interval_hours": float64(12)})

	want := []scheduleCall{{kind: "signin", interval: 6 * time.Hour}, {kind: "backup", interval: 12 * time.Hour}}
	if len(scheduler.calls) != len(want) {
		t.Fatalf("调用 = %#v", scheduler.calls)
	}
	for index, expected := range want {
		if scheduler.calls[index] != expected {
			t.Fatalf("第 %d 次调用 = %#v, want %#v", index, scheduler.calls[index], expected)
		}
	}

	// 其它插件、以及间隔缺失/为零时都不能动调度。
	scheduler.calls = nil
	for _, id := range []string{
		pluginengine.PluginRSSEnhance,
		pluginengine.PluginSubscriptionAssistant,
		pluginengine.PluginMediaLibrarySync,
	} {
		listener(ctx, id, pluginengine.Config{"interval_hours": int64(3), "interval_minutes": int64(15)})
	}
	listener(ctx, pluginengine.PluginSiteSignIn, pluginengine.Config{})
	listener(ctx, pluginengine.PluginCloudBackup, pluginengine.Config{"interval_hours": int64(0)})
	listener(ctx, pluginengine.PluginSiteSignIn, pluginengine.Config{"interval_hours": "6"})
	if len(scheduler.calls) != 0 {
		t.Fatalf("不该触发调度: %#v", scheduler.calls)
	}
}

func TestPluginScheduleListenerWithoutScheduler(t *testing.T) {
	if pluginScheduleListener(nil, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))) != nil {
		t.Fatal("没有调度器时应当返回 nil，让注册表跳过回调")
	}
}

func TestPluginScheduleListenerLogsFailure(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
	scheduler := &fakePluginScheduler{err: errors.New("river 拒绝")}
	listener := pluginScheduleListener(scheduler, logger)

	// 失败不能像成功一样安静：要留一条可查的告警，但不能 panic。
	listener(context.Background(), pluginengine.PluginSiteSignIn, pluginengine.Config{"interval_hours": int64(6)})

	output := logs.String()
	if !strings.Contains(output, "插件配置未能应用到调度") || !strings.Contains(output, "site_signin") {
		t.Fatalf("告警日志 = %q", output)
	}
}

// 启动期应用已存配置时，队列没起来（无数据库）只能记日志，不能拦住启动。
func TestNewPluginRegistrySurvivesScheduleFailure(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn}))

	registry, err := newPluginRegistry(context.Background(), pluginengine.NewMemoryStore(), &jobs.Manager{}, logger)
	if err != nil {
		t.Fatalf("启动不应失败: %v", err)
	}
	if registry == nil || len(registry.List()) != 5 {
		t.Fatalf("注册表 = %#v", registry)
	}
	// 未安装的插件用的是默认间隔，回调会去调 manager；manager 没启用 worker，
	// 于是得到「未配置」错误并落一条告警——这正是重启后不会静默失效的原因。
	if !strings.Contains(logs.String(), "插件配置未能应用到调度") {
		t.Fatalf("应当留下告警: %q", logs.String())
	}
}

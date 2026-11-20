package app

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/moviepilot-go/moviepilot-go/internal/database"
	"github.com/moviepilot-go/moviepilot-go/internal/jobs"
	"github.com/moviepilot-go/moviepilot-go/internal/pluginengine"
)

// 本文件覆盖插件动作到后台任务的映射：参数怎么取、空配置怎么兜底、
// 部分失败怎么如实上报。用假队列，避免为了测映射去连数据库。

type pluginQueueCall struct {
	kind   string
	dir    string
	keep   int
	cutoff time.Time
	server string
}

type fakePluginQueue struct {
	calls     []pluginQueueCall
	failKinds map[string]error
	failBySrv map[string]error
}

func (f *fakePluginQueue) record(call pluginQueueCall) error {
	f.calls = append(f.calls, call)
	if err, bad := f.failKinds[call.kind]; bad {
		return err
	}
	if err, bad := f.failBySrv[call.server]; bad {
		return err
	}
	return nil
}

func (f *fakePluginQueue) EnqueueSiteSigninSweep(context.Context) error {
	return f.record(pluginQueueCall{kind: "signin"})
}

func (f *fakePluginQueue) EnqueueBackupSweep(_ context.Context, dir string, keep int) error {
	return f.record(pluginQueueCall{kind: "backup", dir: dir, keep: keep})
}

func (f *fakePluginQueue) EnqueueRSSSweep(_ context.Context, cutoff time.Time) error {
	return f.record(pluginQueueCall{kind: "rss", cutoff: cutoff})
}

func (f *fakePluginQueue) EnqueueSubscriptionSearchSweep(context.Context) error {
	return f.record(pluginQueueCall{kind: "subscription"})
}

func (f *fakePluginQueue) EnqueueMediaServerSync(_ context.Context, server string) error {
	return f.record(pluginQueueCall{kind: "media", server: server})
}

func TestNewPluginServicesWithoutQueue(t *testing.T) {
	services := newPluginServices(nil)
	if services.SignInAll != nil || services.Backup != nil || services.EnhanceRSS != nil ||
		services.AssistSubscription != nil || services.SyncMediaServers != nil {
		t.Fatalf("没有队列时不应接线任何能力: %#v", services)
	}
}

func TestPluginServicesTriggerExistingJobs(t *testing.T) {
	ctx := context.Background()
	queue := &fakePluginQueue{}
	services := newPluginServices(queue)

	t.Run("签到", func(t *testing.T) {
		result, err := services.SignInAll(ctx, pluginengine.RunInput{})
		if err != nil {
			t.Fatalf("error = %v", err)
		}
		if len(queue.calls) != 1 || queue.calls[0].kind != "signin" {
			t.Fatalf("calls = %#v", queue.calls)
		}
		if !strings.Contains(result.Message, "已入队") {
			t.Fatalf("message = %q", result.Message)
		}
	})

	t.Run("备份读取目录与保留份数", func(t *testing.T) {
		queue.calls = nil
		result, err := services.Backup(ctx, pluginengine.RunInput{Config: pluginengine.Config{
			"path":      "  /data/backup  ",
			"keep":      float64(5),
			"encrypted": false,
		}})
		if err != nil {
			t.Fatalf("error = %v", err)
		}
		if len(queue.calls) != 1 {
			t.Fatalf("calls = %#v", queue.calls)
		}
		if queue.calls[0].dir != "/data/backup" || queue.calls[0].keep != 5 {
			t.Fatalf("call = %#v", queue.calls[0])
		}
		if result.Details["keep"] != 5 || result.Details["dir"] != "/data/backup" {
			t.Fatalf("details = %#v", result.Details)
		}
	})

	t.Run("备份缺配置时交给 worker 兜底", func(t *testing.T) {
		queue.calls = nil
		if _, err := services.Backup(ctx, pluginengine.RunInput{}); err != nil {
			t.Fatalf("error = %v", err)
		}
		if queue.calls[0].dir != "" || queue.calls[0].keep != 0 {
			t.Fatalf("call = %#v", queue.calls[0])
		}
	})

	t.Run("RSS 增量扫描用当前时间做 cutoff", func(t *testing.T) {
		queue.calls = nil
		before := time.Now().UTC()
		if _, err := services.EnhanceRSS(ctx, pluginengine.RunInput{}); err != nil {
			t.Fatalf("error = %v", err)
		}
		cutoff := queue.calls[0].cutoff
		if cutoff.Before(before.Add(-time.Second)) || cutoff.After(time.Now().UTC().Add(time.Second)) {
			t.Fatalf("cutoff = %v", cutoff)
		}
	})

	t.Run("订阅助手", func(t *testing.T) {
		queue.calls = nil
		if _, err := services.AssistSubscription(ctx, pluginengine.RunInput{}); err != nil {
			t.Fatalf("error = %v", err)
		}
		if queue.calls[0].kind != "subscription" {
			t.Fatalf("call = %#v", queue.calls[0])
		}
	})

	t.Run("媒体库同步为空时覆盖所有服务器", func(t *testing.T) {
		queue.calls = nil
		result, err := services.SyncMediaServers(ctx, pluginengine.RunInput{})
		if err != nil {
			t.Fatalf("error = %v", err)
		}
		if len(queue.calls) != 1 || queue.calls[0].server != "" {
			t.Fatalf("calls = %#v", queue.calls)
		}
		if !strings.Contains(result.Message, "全部服务器") {
			t.Fatalf("message = %q", result.Message)
		}
	})

	t.Run("媒体库同步按逗号拆分并去重空项", func(t *testing.T) {
		queue.calls = nil
		result, err := services.SyncMediaServers(ctx, pluginengine.RunInput{Config: pluginengine.Config{
			"servers": " emby , jellyfin ,, ",
		}})
		if err != nil {
			t.Fatalf("error = %v", err)
		}
		if len(queue.calls) != 2 || queue.calls[0].server != "emby" || queue.calls[1].server != "jellyfin" {
			t.Fatalf("calls = %#v", queue.calls)
		}
		if !strings.Contains(result.Message, "2 个服务器") {
			t.Fatalf("message = %q", result.Message)
		}
	})

	t.Run("媒体库同步部分失败如实上报", func(t *testing.T) {
		queue.calls = nil
		queue.failBySrv = map[string]error{"jellyfin": errors.New("队列不可用")}
		defer func() { queue.failBySrv = nil }()
		result, err := services.SyncMediaServers(ctx, pluginengine.RunInput{Config: pluginengine.Config{
			"servers": "emby,jellyfin",
		}})
		if err == nil {
			t.Fatalf("部分失败必须返回错误")
		}
		if !strings.Contains(err.Error(), "jellyfin") {
			t.Fatalf("error = %v", err)
		}
		if !strings.Contains(result.Message, "成功 1") {
			t.Fatalf("message = %q", result.Message)
		}
		if result.Details["succeeded"] != 1 {
			t.Fatalf("details = %#v", result.Details)
		}
	})
}

func TestPluginServicesPropagateQueueErrors(t *testing.T) {
	ctx := context.Background()
	boom := errors.New("队列不可用")
	cases := []struct {
		name string
		run  func(pluginengine.Services) (pluginengine.Result, error)
	}{
		{name: "签到", run: func(s pluginengine.Services) (pluginengine.Result, error) {
			return s.SignInAll(ctx, pluginengine.RunInput{})
		}},
		{name: "备份", run: func(s pluginengine.Services) (pluginengine.Result, error) {
			return s.Backup(ctx, pluginengine.RunInput{})
		}},
		{name: "RSS", run: func(s pluginengine.Services) (pluginengine.Result, error) {
			return s.EnhanceRSS(ctx, pluginengine.RunInput{})
		}},
		{name: "订阅", run: func(s pluginengine.Services) (pluginengine.Result, error) {
			return s.AssistSubscription(ctx, pluginengine.RunInput{})
		}},
		{name: "媒体库（全部）", run: func(s pluginengine.Services) (pluginengine.Result, error) {
			return s.SyncMediaServers(ctx, pluginengine.RunInput{})
		}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			// failKinds 为空，靠 failBySrv 与「全部失败」两条路覆盖；
			// 这里用 recording 队列 + 全量失败映射来模拟。
			queue := &fakePluginQueue{failKinds: map[string]error{"signin": boom, "backup": boom, "rss": boom, "subscription": boom, "media": boom}}
			if _, err := test.run(newPluginServices(queue)); !errors.Is(err, boom) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestPluginConfigHelpers(t *testing.T) {
	if got := pluginConfigInt(pluginengine.Config{"keep": int(3)}, "keep"); got != 3 {
		t.Fatalf("int = %d", got)
	}
	if got := pluginConfigInt(pluginengine.Config{"keep": int64(4)}, "keep"); got != 4 {
		t.Fatalf("int64 = %d", got)
	}
	if got := pluginConfigInt(pluginengine.Config{"keep": float64(5)}, "keep"); got != 5 {
		t.Fatalf("float64 = %d", got)
	}
	if got := pluginConfigInt(pluginengine.Config{"keep": "6"}, "keep"); got != 0 {
		t.Fatalf("字符串应视为未配置 = %d", got)
	}
	if got := pluginConfigInt(nil, "keep"); got != 0 {
		t.Fatalf("nil 配置 = %d", got)
	}

	if got := pluginConfigList(pluginengine.Config{}, "servers"); got != nil {
		t.Fatalf("空配置 = %#v", got)
	}
	if got := pluginConfigList(pluginengine.Config{"servers": "   "}, "servers"); got != nil {
		t.Fatalf("空白配置 = %#v", got)
	}
	if got := pluginConfigList(pluginengine.Config{"servers": "a, b"}, "servers"); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("拆分结果 = %#v", got)
	}
}

func TestPluginStateStoreRejectsNilStore(t *testing.T) {
	if _, err := pluginStateStore(nil); err == nil {
		t.Fatalf("没有数据库时应当失败")
	}
	// 零值 Store 只用来证明包装成功；真正的读写在上面的设置表测试里覆盖。
	stateStore, err := pluginStateStore(&database.Store{})
	if err != nil || stateStore == nil {
		t.Fatalf("stateStore = %v, err = %v", stateStore, err)
	}
}

func TestNewPluginRegistryWiresQueueFromManager(t *testing.T) {
	ctx := context.Background()
	registry, err := newPluginRegistry(ctx, pluginengine.NewMemoryStore(), &jobs.Manager{}, nil)
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	if err := registry.Install(ctx, pluginengine.PluginSiteSignIn); err != nil {
		t.Fatalf("安装失败: %v", err)
	}
	if err := registry.Enable(ctx, pluginengine.PluginSiteSignIn); err != nil {
		t.Fatalf("启用失败: %v", err)
	}
	// 接上了队列但队列未启动：错误应当来自 jobs 包的「未启用」哨兵，
	// 而不是 pluginengine 的「能力未接线」——后者说明根本没接上。
	_, err = registry.Run(ctx, pluginengine.PluginSiteSignIn, pluginengine.ActionRun, nil)
	if !errors.Is(err, jobs.ErrSiteSigninSweepUnavailable) {
		t.Fatalf("error = %v", err)
	}
}

func TestNewPluginRegistryWithoutManager(t *testing.T) {
	ctx := context.Background()
	registry, err := newPluginRegistry(ctx, pluginengine.NewMemoryStore(), nil, nil)
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	if registry == nil {
		t.Fatalf("注册表不应为空")
	}
	if plugins := registry.List(); len(plugins) != 5 {
		t.Fatalf("插件数量 = %d", len(plugins))
	}
	// 没有 JobManager 时能力留空：运行动作要如实报「未接线」，不能假装成功。
	if err := registry.Install(ctx, pluginengine.PluginSiteSignIn); err != nil {
		t.Fatalf("安装失败: %v", err)
	}
	if err := registry.Enable(ctx, pluginengine.PluginSiteSignIn); err != nil {
		t.Fatalf("启用失败: %v", err)
	}
	if _, err := registry.Run(ctx, pluginengine.PluginSiteSignIn, pluginengine.ActionRun, nil); !errors.Is(err, pluginengine.ErrServiceUnavailable) {
		t.Fatalf("error = %v", err)
	}
}

func TestNewPluginRegistryWarnsOnStaleConfig(t *testing.T) {
	ctx := context.Background()
	store := pluginengine.NewMemoryStore()
	// 历史配置里有一个当前清单不认识的键：Restore 会回退默认值并告警。
	if err := store.Save(ctx, map[string]pluginengine.PersistedState{
		pluginengine.PluginSiteSignIn: {Installed: true, Config: map[string]any{"retired_key": true}},
	}); err != nil {
		t.Fatalf("预置状态失败: %v", err)
	}
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	registry, err := newPluginRegistry(ctx, store, nil, logger)
	if err != nil {
		t.Fatalf("配置过期不应阻断启动: %v", err)
	}
	plugin, err := registry.Get(pluginengine.PluginSiteSignIn)
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	if !plugin.Installed {
		t.Fatalf("安装状态应保留: %#v", plugin)
	}
	if _, present := plugin.Config["retired_key"]; present {
		t.Fatalf("过期键应被丢弃: %#v", plugin.Config)
	}
	if !strings.Contains(logs.String(), "插件历史配置已失效") {
		t.Fatalf("应当记录告警: %s", logs.String())
	}
}

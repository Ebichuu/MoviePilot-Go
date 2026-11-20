package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/moviepilot-go/moviepilot-go/internal/database"
	"github.com/moviepilot-go/moviepilot-go/internal/jobs"
	"github.com/moviepilot-go/moviepilot-go/internal/pluginengine"
)

// pluginStateStore 把设置表包装成插件状态存储。nil 的 *database.Store 装进
// SettingsStore 接口后不是 nil，所以必须在这里判空，否则要等到第一次读设置时才炸。
func pluginStateStore(store *database.Store) (pluginengine.StateStore, error) {
	if store == nil {
		return nil, errors.New("插件状态需要已连接数据库的设置存储")
	}
	stateStore, err := pluginengine.NewSettingsStateStore(store, "")
	if err != nil {
		return nil, fmt.Errorf("初始化插件状态存储: %w", err)
	}
	return stateStore, nil
}

// newPluginRegistry 用给定状态存储构建注册表，并把 5 个插件接到 manager 的队列上。
// 历史配置与当前清单不符时 NewBuiltinRegistry 会回退默认值并返回告警：
// 这里只记日志、继续启动，不拿一个可自愈的问题去阻断服务。
func newPluginRegistry(
	ctx context.Context,
	stateStore pluginengine.StateStore,
	manager *jobs.Manager,
	logger *slog.Logger,
) (*pluginengine.Registry, error) {
	// 显式判空再赋给接口：nil 的 *jobs.Manager 装进接口后不是 nil，
	// 只有在这里挡住，服务层才能用 queue == nil 统一短路。
	var queue pluginJobs
	if manager != nil {
		queue = manager
	}
	registry, err := pluginengine.NewBuiltinRegistry(ctx, stateStore, newPluginServices(queue))
	if registry == nil {
		return nil, fmt.Errorf("初始化插件注册表: %w", err)
	}
	if err != nil {
		if logger != nil {
			logger.Warn("插件历史配置已失效，已回退默认值", "error", err)
		}
	}
	// 同样显式判空：nil 的 *jobs.Manager 装进 pluginScheduler 后不是 nil。
	var scheduler pluginScheduler
	if manager != nil {
		scheduler = manager
	}
	listener := pluginScheduleListener(scheduler, logger)
	registry.SetConfigListener(listener)
	if listener != nil {
		// 启动时按已存配置应用一次：否则重启后周期会退回硬编码的 24 小时，
		// 与界面上显示的间隔对不上，用户只会觉得「配置没生效」。
		for _, plugin := range registry.List() {
			listener(ctx, plugin.ID, plugin.Config)
		}
	}
	return registry, nil
}

// pluginJobs 是插件动作需要的队列入口。用接口而不是 *jobs.Manager：
// 没有数据库时也能测清楚「配置怎么映射成任务参数」。
type pluginJobs interface {
	EnqueueSiteSigninSweep(ctx context.Context) error
	EnqueueBackupSweep(ctx context.Context, dir string, keep int) error
	EnqueueRSSSweep(ctx context.Context, cutoff time.Time) error
	EnqueueSubscriptionSearchSweep(ctx context.Context) error
	EnqueueMediaServerSync(ctx context.Context, server string) error
}

// pluginScheduler 是能热更新周期任务的队列入口。单独拆一个接口：
// 「插件配置能不能改调度」这件事要在编译期就显式，测试也只需实现这两个方法。
type pluginScheduler interface {
	SetSiteSigninSweepInterval(interval time.Duration) error
	SetBackupSweepInterval(interval time.Duration) error
}

// pluginScheduleListener 把插件配置里的间隔翻译成周期任务热更新。
//
// 只有签到（interval_hours）与备份（interval_hours）由插件管周期：它们的周期在
// 后端本来就只有「每天一次」这个常量，插件配置是唯一真相。RSS 增量扫描的周期
// 属于系统设置（`subscribe.rss_interval`，由 app 直接调 SetRSSSweepInterval），
// 媒体库同步的周期按媒体服务器各配一份——给它们再存一份插件配置会出现两个真相。
//
// 失败只记日志：配置已经落盘，让保存接口返回错误会让用户以为没保存。
func pluginScheduleListener(scheduler pluginScheduler, logger *slog.Logger) pluginengine.ConfigListener {
	if scheduler == nil {
		return nil
	}
	apply := func(id string, interval time.Duration, set func(time.Duration) error) {
		if interval <= 0 {
			return
		}
		if err := set(interval); err != nil && logger != nil {
			logger.Warn("插件配置未能应用到调度", "plugin", id, "interval", interval.String(), "error", err)
		}
	}
	return func(_ context.Context, id string, config pluginengine.Config) {
		switch id {
		case pluginengine.PluginSiteSignIn:
			apply(id, time.Duration(pluginConfigInt(config, "interval_hours"))*time.Hour, scheduler.SetSiteSigninSweepInterval)
		case pluginengine.PluginCloudBackup:
			apply(id, time.Duration(pluginConfigInt(config, "interval_hours"))*time.Hour, scheduler.SetBackupSweepInterval)
		}
	}
}

// newPluginServices 把 5 个原生插件接到既有的后台任务上。
//
// 每个动作只做「触发一轮既有任务」：不复制业务逻辑，也不声称同步完成——
// 返回值只说明「已入队」，真正什么时候跑完由队列与 worker 决定。
// 队列错误原样上抛，HTTP 层会把它变成可读的失败信息（而不是假装成功）。
func newPluginServices(queue pluginJobs) pluginengine.Services {
	if queue == nil {
		return pluginengine.Services{}
	}
	return pluginengine.Services{
		SignInAll: func(ctx context.Context, _ pluginengine.RunInput) (pluginengine.Result, error) {
			if err := queue.EnqueueSiteSigninSweep(ctx); err != nil {
				return pluginengine.Result{}, err
			}
			return pluginengine.Result{Message: "已入队：站点签到"}, nil
		},
		Backup: func(ctx context.Context, in pluginengine.RunInput) (pluginengine.Result, error) {
			dir, _ := in.Config["path"].(string)
			dir = strings.TrimSpace(dir)
			keep := pluginConfigInt(in.Config, "keep")
			if err := queue.EnqueueBackupSweep(ctx, dir, keep); err != nil {
				return pluginengine.Result{}, err
			}
			return pluginengine.Result{
				Message: "已入队：配置备份",
				Details: map[string]any{"dir": dir, "keep": keep},
			}, nil
		},
		EnhanceRSS: func(ctx context.Context, _ pluginengine.RunInput) (pluginengine.Result, error) {
			if err := queue.EnqueueRSSSweep(ctx, time.Now().UTC()); err != nil {
				return pluginengine.Result{}, err
			}
			return pluginengine.Result{Message: "已入队：RSS 增量扫描"}, nil
		},
		AssistSubscription: func(ctx context.Context, _ pluginengine.RunInput) (pluginengine.Result, error) {
			if err := queue.EnqueueSubscriptionSearchSweep(ctx); err != nil {
				return pluginengine.Result{}, err
			}
			return pluginengine.Result{Message: "已入队：订阅精准搜索"}, nil
		},
		SyncMediaServers: func(ctx context.Context, in pluginengine.RunInput) (pluginengine.Result, error) {
			servers := pluginConfigList(in.Config, "servers")
			if len(servers) == 0 {
				if err := queue.EnqueueMediaServerSync(ctx, ""); err != nil {
					return pluginengine.Result{}, err
				}
				return pluginengine.Result{Message: "已入队：媒体库同步（全部服务器）"}, nil
			}
			var failures []error
			succeeded := 0
			for _, server := range servers {
				if err := queue.EnqueueMediaServerSync(ctx, server); err != nil {
					failures = append(failures, fmt.Errorf("服务器 %s: %w", server, err))
					continue
				}
				succeeded++
			}
			if len(failures) > 0 {
				// 部分成功要如实说清楚：整体返回错误，Details 里保留成功计数。
				return pluginengine.Result{
						Message: fmt.Sprintf("媒体库同步部分入队失败（成功 %d / 共 %d）", succeeded, len(servers)),
						Details: map[string]any{"succeeded": succeeded, "servers": servers},
					},
					errors.Join(failures...)
			}
			return pluginengine.Result{Message: fmt.Sprintf("已入队：媒体库同步（%d 个服务器）", succeeded)}, nil
		},
	}
}

// pluginConfigInt 读取整数配置：JSON 解出的是 float64，手工构造的是 int/int64，
// 三种都要认，否则「配置看起来存进去了但 keep 一直是 0」。
func pluginConfigInt(config pluginengine.Config, key string) int {
	switch value := config[key].(type) {
	case int:
		return value
	case int64:
		return int(value)
	case float64:
		return int(value)
	default:
		return 0
	}
}

// pluginConfigList 读取逗号分隔的字符串列表，忽略空项。
func pluginConfigList(config pluginengine.Config, key string) []string {
	raw, _ := config[key].(string)
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	items := make([]string, 0, 4)
	for _, item := range strings.Split(raw, ",") {
		if trimmed := strings.TrimSpace(item); trimmed != "" {
			items = append(items, trimmed)
		}
	}
	return items
}

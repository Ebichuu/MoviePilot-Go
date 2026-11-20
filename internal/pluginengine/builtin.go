package pluginengine

import "context"

// 5 个原生插件的 ID。这些功能本身已经在 Go 侧实现（站点签到、自动备份、RSS、
// 订阅、媒体库同步），本包补的是「插件形态」：清单、安装/启停、配置与统一动作入口，
// 不复制第二套业务逻辑。
const (
	PluginSiteSignIn            = "site_signin"
	PluginCloudBackup           = "cloud_backup"
	PluginRSSEnhance            = "rss_enhance"
	PluginSubscriptionAssistant = "subscription_assistant"
	PluginMediaLibrarySync      = "media_library_sync"
)

// ActionRun 是本包约定的统一动作名：执行一次完整流程（手工触发或定时触发）。
const ActionRun = "run"

// Capability 是插件能力的实现签名。实现方拿到的 Config 已按清单校验并补齐默认值。
// 它与 ActionFunc 是同一个类型，只是名字在「注册」与「实现」两个语境下更好读。
type Capability = ActionFunc

// Services 注入 5 个原生插件背后的真实能力。字段为 nil 表示尚未接线，
// 对应动作返回 ErrServiceUnavailable：如实上报「还没接」，而不是假装执行成功。
type Services struct {
	SignInAll          Capability
	Backup             Capability
	EnhanceRSS         Capability
	AssistSubscription Capability
	SyncMediaServers   Capability
}

// BuiltinDefinitions 返回 5 个原生插件的定义。清单只声明真正会读取的配置项，
// 避免出现「表单里能填、代码里没人读」的假开关。
func BuiltinDefinitions(services Services) []Definition {
	return []Definition{
		{
			Manifest: Manifest{
				ID:          PluginSiteSignIn,
				Name:        "站点自动签到",
				Author:      "moviepilot-go",
				Version:     "1.0.0",
				Level:       1,
				Label:       "站点",
				Description: "按间隔对所有已配置站点执行登录保活与签到，结果如实上报成功/跳过/失败数量。",
				Fields: []Field{
					{Key: "interval_hours", Label: "执行间隔（小时）", Type: FieldInteger, Default: int64(24), Required: true},
					{Key: "notify", Label: "执行后发送通知", Type: FieldBoolean, Default: false},
				},
			},
			Actions: map[string]ActionFunc{ActionRun: services.SignInAll},
		},
		{
			Manifest: Manifest{
				ID:          PluginCloudBackup,
				Name:        "自动备份",
				Author:      "moviepilot-go",
				Version:     "1.0.0",
				Level:       1,
				Label:       "系统",
				Description: "备份数据库与配置并按份数保留，超出 keep 的旧备份会被清理。",
				Fields: []Field{
					{Key: "interval_hours", Label: "执行间隔（小时）", Type: FieldInteger, Default: int64(24), Required: true, Help: "最短 1 小时；保存后立即重建备份周期任务"},
					{Key: "keep", Label: "保留份数", Type: FieldInteger, Default: int64(7), Required: true},
					{Key: "path", Label: "备份目录", Type: FieldString, Default: "", Help: "留空表示使用默认备份目录"},
					{Key: "encrypted", Label: "加密备份", Type: FieldBoolean, Default: true},
				},
			},
			Actions: map[string]ActionFunc{ActionRun: services.Backup},
		},
		{
			Manifest: Manifest{
				ID:          PluginRSSEnhance,
				Name:        "RSS 增强",
				Author:      "moviepilot-go",
				Version:     "1.0.0",
				Level:       1,
				Label:       "订阅",
				Description: "对已添加的 RSS 源做二次过滤与匹配增强，不改变订阅本身的判定结果。",
				// 这里刻意不放「轮询间隔」：RSS 增量扫描的周期属于系统设置
				// （subscribe.rss_interval），插件再存一份会出现两个真相。
				Fields: []Field{
					{Key: "free_only", Label: "只看免费/优惠种子", Type: FieldBoolean, Default: false},
					{Key: "keyword_filter", Label: "关键词过滤", Type: FieldString, Default: "", Help: "多个关键词用英文逗号分隔，留空表示不过滤"},
				},
			},
			Actions: map[string]ActionFunc{ActionRun: services.EnhanceRSS},
		},
		{
			Manifest: Manifest{
				ID:          PluginSubscriptionAssistant,
				Name:        "订阅助手",
				Author:      "moviepilot-go",
				Version:     "1.0.0",
				Level:       1,
				Label:       "订阅",
				Description: "为已完成的订阅做洗版与缺集补全，触发条件与订阅自身的质量规则一致。",
				Fields: []Field{
					{Key: "best_version", Label: "自动洗版", Type: FieldBoolean, Default: true},
					{Key: "missing_episode", Label: "补全缺集", Type: FieldBoolean, Default: true},
					{Key: "max_retry", Label: "单次最大重试", Type: FieldInteger, Default: int64(3), Required: true},
				},
			},
			Actions: map[string]ActionFunc{ActionRun: services.AssistSubscription},
		},
		{
			Manifest: Manifest{
				ID:          PluginMediaLibrarySync,
				Name:        "媒体库同步",
				Author:      "moviepilot-go",
				Version:     "1.0.0",
				Level:       1,
				Label:       "媒体库",
				Description: "把本地入库结果同步到媒体服务器并刷新条目缓存，按服务器名逐个执行。",
				// 刻意不放「同步间隔」：媒体库同步的周期由媒体服务器设置里的
				// 每服务器间隔决定，插件再存一份只会出现两个真相。
				Fields: []Field{
					{Key: "servers", Label: "服务器", Type: FieldString, Default: "", Help: "服务器名用英文逗号分隔，留空表示全部"},
					{Key: "notify", Label: "同步后发送通知", Type: FieldBoolean, Default: false},
				},
			},
			Actions: map[string]ActionFunc{ActionRun: services.SyncMediaServers},
		},
	}
}

// NewBuiltinRegistry 注册 5 个原生插件并恢复持久化状态。
//
// 返回的 error 非 nil 时注册表依然可用：它表示部分插件的历史配置与当前清单不符，
// 已经回退到默认值。调用方应当把这种情况记为告警并继续启动，而不是中断服务。
func NewBuiltinRegistry(ctx context.Context, store StateStore, services Services) (*Registry, error) {
	registry := NewRegistry(store)
	if err := registry.RegisterAll(BuiltinDefinitions(services)); err != nil {
		return nil, err
	}
	if err := registry.Restore(ctx); err != nil {
		return registry, err
	}
	return registry, nil
}

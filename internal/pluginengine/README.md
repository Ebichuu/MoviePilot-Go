# internal/pluginengine

原生插件的**清单 / 安装 / 启停 / 配置 / 动作分派**框架。

本包不含任何 Python 运行时：
V2 的 31 条插件接口里真正高频的只有站点签到、自动备份、RSS 增强、订阅助手、
媒体库同步这 5 个，而这 5 项能力 Go 侧本来就有实现。本包补的是「插件形态」，
不是第二套业务逻辑。

## 做了什么

| 能力 | 说明 |
|---|---|
| 清单校验 | ID/名称/版本/字段定义不合法直接拒绝注册，`RegisterAll` 是原子的（一个不合法则一个都不进表） |
| 安装 / 卸载 | 幂等；卸载会同时关停并清空配置，避免残留配置在重装后意外生效 |
| 启用 / 停用 | 未安装不允许启用 |
| 配置 | 按 `Field` 声明的类型归一：整数不接受小数与字符串，布尔不接受 `"true"`，`select` 只接受声明过的值，未知键直接拒绝 |
| 动作分派 | 检查顺序固定：存在 → 已安装 → 已启用 → 动作存在 → 已接线 |
| 持久化 | `StateStore` 接口，落盘成功后才提交内存状态（失败即回滚） |

## 刻意不做

- 不执行 Python 代码，也不下载/解压 V2 插件包（`install` 的语义是「安装内置插件」）。
- 不提供 HTTP 路由：本包没有任何 `net/http` 依赖，由 `httpapi` 决定怎么暴露。
- 不内置业务逻辑：5 项能力通过 `Services` 注入，未注入时动作返回 `ErrServiceUnavailable`，
  如实上报「还没接」，而不是返回假的成功。

## 5 个内置插件

| ID | 名称 | 关键配置 |
|---|---|---|
| `site_signin` | 站点自动签到 | `interval_hours`(24)、`notify`(false) |
| `cloud_backup` | 自动备份 | `interval_hours`(24)、`keep`(7)、`path`("")、`encrypted`(true) |
| `rss_enhance` | RSS 增强 | `free_only`(false)、`keyword_filter`("") |
| `subscription_assistant` | 订阅助手 | `best_version`(true)、`missing_episode`(true)、`max_retry`(3) |
| `media_library_sync` | 媒体库同步 | `servers`("")、`notify`(false) |

括号内是默认值。所有插件只有统一的 `run` 动作。

RSS 增强与媒体库同步**刻意没有间隔字段**：前者的轮询周期属于系统设置
（`subscribe.rss_interval`，由 app 直接调用 `SetRSSSweepInterval`），后者的周期按媒体
服务器各配一份。插件再存一份就会出现「两个真相」，改了界面不生效或改了才生效都说不清。

## 接线位置（已接入）

本包仍然只有 `net/http` 之外的纯逻辑；真正的接线在两处薄适配里：

| 位置 | 作用 |
|---|---|
| `internal/app/plugin_services.go` | 5 个能力 → 既有 River 队列，`pluginStateStore` 把状态落到 `settings` 表（键 `PluginState:v1`） |
| `internal/httpapi/plugin_api.go` | V2 前端契约的 HTTP 边界（字段名对齐 `web/dist` 里的插件页） |

能力映射只做「触发一轮既有任务」，不复制业务逻辑：

| 插件 | 动作实际做的事 |
|---|---|
| `site_signin` | `jobs.EnqueueSiteSigninSweep` |
| `cloud_backup` | `jobs.EnqueueBackupSweep(dir, keep)`，`keep` 来自配置，`<=0` 时回退环境变量 |
| `rss_enhance` | `jobs.EnqueueRSSSweep(now)` |
| `subscription_assistant` | `jobs.EnqueueSubscriptionSearchSweep` |
| `media_library_sync` | `jobs.EnqueueMediaServerSync(server)`；配置为空时一次覆盖全部服务器 |

成功只代表**已入队**，不代表已执行完；队列未启用时原样上抛 `jobs` 包的哨兵错误。

### 配置怎么变成运行时效果

配置不是「存起来好看」的：保存成功后 `Registry` 会回调 `ConfigListener`
（`SetConfigListener`），由 `internal/app` 把它翻译成周期任务的热更新：

| 插件 | 配置 | 运行时效果 |
|---|---|---|
| `site_signin` | `interval_hours` | `jobs.SetSiteSigninSweepInterval(h)`：AddSafely 新周期 + Remove 旧句柄 |
| `cloud_backup` | `interval_hours` | `jobs.SetBackupSweepInterval(h)`，同上 |
| 其它三个 | — | 不改调度，也不假装改；媒体库按服务器、RSS 按系统设置 |

两条设计约束：

- 回调**没有返回值**，失败只记 `WARN`。配置此时已经落盘，让保存接口报错会让用户
  以为没保存——真正的失败原因在日志里（`插件配置未能应用到调度`）。
- 启动时按已存配置**再应用一次**（`newPluginRegistry`），否则重启后周期会退回硬编码的
  24 小时，和界面上显示的间隔对不上。

### HTTP 契约（对齐 V2 前端）

| 方法 路径 | 行为 |
|---|---|
| `GET /plugin/?state=installed\|market` | 插件列表；`market` 返回这 5 个内置插件（`installed` 照实），`has_page`/`has_update` 恒为 false |
| `GET /plugin/installed` | 已安装插件的 ID 列表 |
| `GET /plugin/{id}` | 单个插件快照 |
| `GET /plugin/install/{id}` | 安装并启用（本项目没有独立启用开关，只装不开会永远不运行） |
| `DELETE /plugin/{id}` | 卸载（同时停用并复位配置） |
| `PUT /plugin/{id}` | 保存配置，请求体就是配置模型本身 |
| `GET /plugin/reset/{id}` | 配置恢复默认值 |
| `GET /plugin/form/{id}` | `render_mode="vuetify"` + 按字段生成的 `conf` 组件描述：空串会被前端当成「加载失败」 |
| `GET /plugin/reload/{id}` | 以 `settings` 表为准重载内存状态 |
| `POST /plugin/{id}/action?action=run` | 本项目的扩展端点：手工触发一轮（V2 没有这个能力） |

仍然保持拒绝（并在消息里说明原因）：`/plugin/dashboard/*`、`/plugin/file/*`、
`/plugin/page/*`、`/plugin/history/*`、`/plugin/rating*`、`/plugin/releases/*`、
`/plugin/clone/*` —— 这些都依赖 Python 插件运行时或远端插件仓库。

## 错误语义

| 错误 | 触发条件 |
|---|---|
| `ErrInvalidManifest` | 清单自相矛盾（注册期） |
| `ErrInvalidConfig` | 配置与清单不符（写入期） |
| `ErrPluginNotFound` / `ErrPluginExists` | 注册表里没有 / 已存在 |
| `ErrPluginNotInstalled` / `ErrPluginDisabled` | 未安装 / 未启用时执行 |
| `ErrUnknownAction` | 插件没声明这个动作 |
| `ErrServiceUnavailable` | 动作声明了，但后端能力尚未注入 |

## 验证

```powershell
pwsh -NoProfile -ExecutionPolicy Bypass -File .tmp\gowin.ps1 test ./internal/pluginengine -count=1
pwsh -NoProfile -ExecutionPolicy Bypass -File .tmp\gowin.ps1 vet ./internal/pluginengine
# 需要本地 PostgreSQL：验证状态真的落进 settings 表并在重建注册表后读回
pwsh -NoProfile -ExecutionPolicy Bypass -File .tmp\gowin.ps1 test -tags integration ./internal/app -run TestPluginStatePersistsInSettingsTable -count=1
# 需要本地 PostgreSQL：验证三个手工入队入口真的写进 river_job
pwsh -NoProfile -ExecutionPolicy Bypass -File .tmp\gowin.ps1 test -tags integration ./internal/jobs -run TestPluginEnqueueMethodsWithRiver -count=1
```

覆盖率（`-coverprofile` 实测）：本包 99.7%（其中 `registry.go` 100%，含配置回调与锁外通知）、
`httpapi/plugin_api.go` 98.5%、`app/plugin_services.go` 97.5%；`jobs` 侧在 `-tags integration`
下：`site_signin_sweep.go` 93.8%、`backup_sweep.go` 92.8%、`subscription_search.go` 94.1%
（剩余未覆盖是这些文件里既有的 worker/prune 分支，不属于本轮新增代码；`manager.go` 里
按配置注册周期任务的分支由 `TestPluginEnqueueMethodsWithRiver` 覆盖）。

三处未覆盖的分支是结构性不可达的：
`NewBuiltinRegistry` 里 `RegisterAll` 的失败（内置清单是常量，恒定合法）、
`installPlugin` 里 `Install` 之后 `Enable` 的失败（`Enable` 只校验「已安装」）、
以及 `newPluginRegistry` 里 `registry == nil` 的判断（同上）。
